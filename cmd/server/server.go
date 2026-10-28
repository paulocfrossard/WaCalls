package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

type server struct {
	broker    *Broker
	sessions  *SessionManager
	webhooks  *webhookStore
	log       *slog.Logger
	staticDir string

	// webhookAllowHosts, when non-empty, restricts webhook registration and
	// delivery to these exact hostnames (case-insensitive, any port).
	webhookAllowHosts []string

	auth           *authStore
	audit          *auditWriter
	authDisabled   bool
	adminUser      string
	adminPass      string
	sessionTTL     time.Duration
	allowedOrigins []string
	cookieSecure   bool
	trustProxy     bool
	dbPath         string

	// dummyHash is a bcrypt hash at bcryptCost generated once at startup. Login
	// compares unknown usernames against it so the failure path costs the same
	// as a real comparison (no timing oracle for username enumeration).
	dummyHash []byte

	loginMu        sync.Mutex
	loginAttempts  map[string]*loginAttempt
	lastLoginPrune time.Time

	// authFailMu guards the per-IP cap on auth_failed audit rows.
	authFailMu        sync.Mutex
	authFailCounts    map[string]*failedAuthWindow
	lastAuthFailPrune time.Time

	// sseTicketMu guards sseTickets, the one-time tickets used to authenticate
	// GET /api/events without exposing a long-lived session token in the URL.
	sseTicketMu        sync.Mutex
	sseTickets         map[string]*sseTicket
	lastSSETicketPrune time.Time
}

// serverOptions configures newServer. The zero value is safe: AdminUser
// defaults to "admin", SessionTTL to 24h and authentication stays enabled.
// CookieSecure defaults to false here (dev-friendly for direct construction);
// the CLI passes its own default of true.
type serverOptions struct {
	DBPath            string
	StaticDir         string
	MaxCalls          int
	WebhookAllowHosts []string
	AdminUser         string
	AdminPass         string
	SessionTTL        time.Duration
	AllowedOrigins    []string
	AuthDisabled      bool
	CookieSecure      bool
	TrustProxy        bool
}

func openDB(dbPath string) (*sql.DB, error) {
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func newServer(ctx context.Context, opts serverOptions, log *slog.Logger) (*server, error) {
	// bcrypt silently ignores everything past 72 bytes; reject early instead of
	// bootstrapping an account whose effective password differs from the input.
	if len(opts.AdminPass) > 72 {
		return nil, fmt.Errorf("WACALLS_ADMIN_PASSWORD is %d bytes; bcrypt accepts at most 72", len(opts.AdminPass))
	}
	db, err := openDB(opts.DBPath)
	if err != nil {
		return nil, err
	}
	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	if err := container.Upgrade(ctx); err != nil {
		return nil, err
	}
	store, err := newSessionStore(ctx, db)
	if err != nil {
		return nil, err
	}

	waLogger := waLog.Noop
	if log.Enabled(ctx, slog.LevelDebug) {
		waLogger = waLog.Stdout("WA", "INFO", true)
	}

	whStore, err := newWebhookStore(ctx, db)
	if err != nil {
		return nil, err
	}

	auth, err := newAuthStore(ctx, db, log)
	if err != nil {
		return nil, err
	}
	audit := newAuditWriter(ctx, auth, log)
	// Retention is enforced once at startup, best-effort: a failure must not
	// prevent the server from accepting traffic.
	if err := auth.pruneAudit(ctx, time.Now().Add(-auditRetention).UnixMilli()); err != nil {
		log.Warn("audit retention purge failed", "err", err)
	}

	broker := NewBroker()
	worker := newWebhookWorker(whStore, log, opts.WebhookAllowHosts)
	broker.Dispatcher = worker
	worker.Start(ctx, 5)
	mgr := newSessionManager(ctx, container, broker, store, waLogger, log, opts.MaxCalls)
	broker.SnapshotFn = mgr.snapshotEvents

	// Generate the timing-parity dummy hash once. Doing it per request would
	// add a full bcrypt work factor to every login; doing it at a lower cost
	// than bcryptCost leaks username existence through response time.
	dummyHash, err := bcrypt.GenerateFromPassword([]byte(dummyPasswordPlaintext), bcryptCost)
	if err != nil {
		log.Warn("dummy password hash generation failed; using static fallback", "err", err)
		dummyHash = []byte(dummyPasswordHash)
	}

	adminUser := opts.AdminUser
	if adminUser == "" {
		adminUser = "admin"
	}
	ttl := opts.SessionTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}

	s := &server{
		broker:            broker,
		sessions:          mgr,
		webhooks:          whStore,
		log:               log,
		staticDir:         opts.StaticDir,
		webhookAllowHosts: opts.WebhookAllowHosts,
		auth:              auth,
		audit:             audit,
		authDisabled:      opts.AuthDisabled,
		adminUser:         adminUser,
		adminPass:         opts.AdminPass,
		sessionTTL:        ttl,
		allowedOrigins:    opts.AllowedOrigins,
		cookieSecure:      opts.CookieSecure,
		trustProxy:        opts.TrustProxy,
		dbPath:            opts.DBPath,
		dummyHash:         dummyHash,
		sseTickets:        make(map[string]*sseTicket),
	}
	if !opts.CookieSecure {
		log.Warn("cookie-secure disabled: session cookies may travel over plain HTTP",
			"note", "enable -cookie-secure in production; direct TLS or a trusted HTTPS proxy still marks cookies Secure")
	}
	if opts.AuthDisabled {
		log.Warn("authentication disabled", "note", "-auth-disabled: every /api route is open")
		// No admin bootstrap and no password file in dev mode. A synthetic
		// "dev" user row is still required because api_keys.user_id is a
		// foreign key; it has no usable password.
		if err := s.ensureDevUser(ctx); err != nil {
			return nil, err
		}
	} else {
		if err := s.bootstrapAdmin(ctx); err != nil {
			return nil, err
		}
		// The plaintext bootstrap password is no longer needed: the admin row
		// holds only its bcrypt hash. Dropping it here keeps the secret from
		// living in memory for the whole process lifetime.
		s.adminPass = ""
		// Keys minted while authentication was disabled belong to the synthetic
		// dev principal. Revoke them so turning auth back on cannot grant a
		// pre-provisioned credential admin access.
		if n, err := s.auth.revokeApiKeysByUser(ctx, "dev"); err != nil {
			log.Warn("revoke dev api keys failed", "err", err)
		} else if n > 0 {
			log.Warn("revoked api keys owned by the dev user", "count", n)
		}
	}
	s.runAuditRetention(ctx)
	return s, nil
}

// ensureDevUser inserts the synthetic principal used by -auth-disabled. The
// generated password is discarded: the account cannot be logged into.
func (s *server) ensureDevUser(ctx context.Context) error {
	pass, err := generatePassword(32)
	if err != nil {
		return err
	}
	if _, err := s.auth.insertUserIfAbsent(ctx, "dev", "dev", []byte(pass), "admin", time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("auth-disabled: create dev user: %w", err)
	}
	return nil
}

// errEmptyDatabaseNoAdminPassword is returned at startup when the users table
// is empty and no bootstrap password was supplied. The message is part of the
// operator-facing contract and is asserted verbatim by the PR test.
var errEmptyDatabaseNoAdminPassword = errors.New(
	"Banco de dados vazio. Forneça a variável de ambiente WACALLS_ADMIN_PASSWORD com a senha do admin inicial.")

// bootstrapAdmin creates the initial admin user when the users table is empty.
//
// The password must come from WACALLS_ADMIN_PASSWORD: no password is ever
// generated or written to disk. An existing admin row is reused as-is. The
// insert stays idempotent (ON CONFLICT(username) DO NOTHING) so two instances
// booting at the same time converge on one row instead of racing.
func (s *server) bootstrapAdmin(ctx context.Context) error {
	n, err := s.auth.countUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if s.adminPass == "" {
		return errEmptyDatabaseNoAdminPassword
	}

	inserted, err := s.auth.insertUserIfAbsent(ctx, newSessionID(), s.adminUser, []byte(s.adminPass), "admin", time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if !inserted {
		// Lost the bootstrap race; the winner's password is the valid one.
		s.log.Info("admin user already exists; bootstrap skipped", "username", s.adminUser)
		return nil
	}

	// Re-check: after a successful insert at least one user must exist.
	if n, err = s.auth.countUsers(ctx); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("bootstrap: admin user was not persisted")
	}

	s.log.Info("admin user created", "username", s.adminUser)
	return nil
}
