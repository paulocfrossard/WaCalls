package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	// auditRetention is exactly 30 days: rows older than this are purged by
	// the startup sweep and the periodic runAuditRetention ticker. Keep this
	// explicit — docs/security.md documents the same 30-day window.
	auditRetention = 30 * 24 * time.Hour // 30 days
	// auditPruneInterval is how often the retention sweep re-runs after the
	// startup pass.
	auditPruneInterval = 6 * time.Hour
	// Column caps applied before insert so a hostile client cannot grow the
	// audit table without bound (audit DoS).
	auditIPMax        = 64
	auditUserAgentMax = 512
	auditDetailMax    = 1024
	auditResourceMax  = 256
	// bcryptCost is the work factor used for password hashes. 12 exceeds
	// bcrypt.DefaultCost (10) at a cost that stays acceptable for interactive
	// logins.
	bcryptCost = 12
)

type authStore struct{ db *sql.DB }

func newAuthStore(ctx context.Context, db *sql.DB, log *slog.Logger) (*authStore, error) {
	if db == nil {
		return nil, errors.New("nil db")
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id            TEXT PRIMARY KEY,
			username      TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'operator',
			created_at    INTEGER NOT NULL,
			last_login    INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name       TEXT NOT NULL,
			key_hash   TEXT NOT NULL,
			key_prefix TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_used  INTEGER,
			revoked    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS sessions_auth (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			revoked    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			ts         INTEGER NOT NULL,
			actor      TEXT NOT NULL,
			action     TEXT NOT NULL,
			resource   TEXT,
			ip         TEXT,
			user_agent TEXT,
			detail     TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log(actor)`,
		// Supports the session purge predicate (expires_at / revoked sweeps).
		`CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions_auth(expires_at)`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("authstore: newAuthStore: %w", err)
		}
	}
	// Unique indexes can fail on legacy databases that already contain
	// duplicates. That is loud but not fatal: authentication still works
	// without the index, and aborting startup would brick an upgrade.
	uniqueIndexes := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_token_hash ON sessions_auth(token_hash)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys(key_hash)`,
	}
	for _, stmt := range uniqueIndexes {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if log != nil {
				log.Error("authstore: unique index creation failed; continuing without it", "err", err)
			}
		}
	}
	return &authStore{db: db}, nil
}

// insertUserIfAbsent hashes password with bcrypt and inserts the user unless
// the username is already taken, reporting whether a row was created.
//
// ON CONFLICT makes concurrent bootstraps converge instead of racing between a
// COUNT check and an INSERT.
func (s *authStore) insertUserIfAbsent(ctx context.Context, id, username string, password []byte, role string, createdAt int64) (bool, error) {
	hash, err := bcrypt.GenerateFromPassword(password, bcryptCost)
	if err != nil {
		return false, fmt.Errorf("authstore: insertUserIfAbsent: hash: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(username) DO NOTHING`,
		id, username, string(hash), role, createdAt)
	if err != nil {
		return false, fmt.Errorf("authstore: insertUserIfAbsent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("authstore: insertUserIfAbsent: rows: %w", err)
	}
	return n > 0, nil
}

func (s *authStore) getUserByUsername(ctx context.Context, username string) (id, passwordHash, role string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT id, password_hash, role FROM users WHERE username = ?`, username).
		Scan(&id, &passwordHash, &role)
	if err != nil {
		return id, passwordHash, role, fmt.Errorf("authstore: getUserByUsername: %w", err)
	}
	return id, passwordHash, role, nil
}

func (s *authStore) insertApiKey(ctx context.Context, id, userID, name, keyHash, keyPrefix string, createdAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, user_id, name, key_hash, key_prefix, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, name, keyHash, keyPrefix, createdAt)
	if err != nil {
		return fmt.Errorf("authstore: insertApiKey: %w", err)
	}
	return nil
}

// apiKeyRecord is one row of the API-key listing (never includes the hash).
type apiKeyRecord struct {
	id        string
	name      string
	prefix    string
	createdAt int64
	lastUsed  sql.NullInt64
	revoked   bool
}

func (s *authStore) listApiKeys(ctx context.Context) ([]apiKeyRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, key_prefix, created_at, last_used, revoked
		 FROM api_keys ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("authstore: listApiKeys: %w", err)
	}
	defer rows.Close()

	out := make([]apiKeyRecord, 0)
	for rows.Next() {
		var r apiKeyRecord
		var revokedInt int
		if err := rows.Scan(&r.id, &r.name, &r.prefix, &r.createdAt, &r.lastUsed, &revokedInt); err != nil {
			return nil, fmt.Errorf("authstore: listApiKeys: scan: %w", err)
		}
		r.revoked = revokedInt != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authstore: listApiKeys: rows: %w", err)
	}
	return out, nil
}

// revokeApiKey marks a key revoked. It returns sql.ErrNoRows when no such key
// exists.
func (s *authStore) revokeApiKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("authstore: revokeApiKey: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("authstore: revokeApiKey: rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("authstore: revokeApiKey: %w", sql.ErrNoRows)
	}
	return nil
}

// revokeApiKeysByUser revokes every live key owned by userID and reports how
// many rows changed.
func (s *authStore) revokeApiKeysByUser(ctx context.Context, userID string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked = 1 WHERE user_id = ? AND revoked = 0`, userID)
	if err != nil {
		return 0, fmt.Errorf("authstore: revokeApiKeysByUser: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("authstore: revokeApiKeysByUser: rows: %w", err)
	}
	return n, nil
}

// touchApiKeyLastUsed stamps the last successful use of a key (unix millis).
func (s *authStore) touchApiKeyLastUsed(ctx context.Context, id string, ts int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used = ? WHERE id = ?`, ts, id); err != nil {
		return fmt.Errorf("authstore: touchApiKeyLastUsed: %w", err)
	}
	return nil
}

func (s *authStore) insertAudit(ctx context.Context, ts int64, actor, action, resource, ip, userAgent, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, resource, ip, user_agent, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ts, truncateActor(actor), action,
		truncateBytes(resource, auditResourceMax), ip,
		truncateBytes(userAgent, auditUserAgentMax), truncateBytes(detail, auditDetailMax))
	if err != nil {
		return fmt.Errorf("authstore: insertAudit: %w", err)
	}
	return nil
}

// pruneAudit deletes audit rows older than the retention window. It is called
// once at startup; the caller treats failures as best-effort.
func (s *authStore) pruneAudit(ctx context.Context, cutoff int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE ts < ?`, cutoff); err != nil {
		return fmt.Errorf("authstore: pruneAudit: %w", err)
	}
	return nil
}

// truncateActor caps the audit actor at auditActorMax characters without
// splitting a multi-byte rune.
func truncateActor(actor string) string {
	r := []rune(actor)
	if len(r) <= auditActorMax {
		return actor
	}
	return string(r[:auditActorMax])
}

// truncateBytes caps s at max bytes without splitting a multi-byte rune.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func (s *authStore) getUserByID(ctx context.Context, id string) (username, role string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT username, role FROM users WHERE id = ?`, id).
		Scan(&username, &role)
	if err != nil {
		return username, role, fmt.Errorf("authstore: getUserByID: %w", err)
	}
	return username, role, nil
}

// countUsers counts real accounts, ignoring the synthetic "dev" principal that
// -auth-disabled creates. Otherwise a later boot with auth enabled would skip
// admin bootstrap and lock the operator out.
func (s *authStore) countUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id <> 'dev'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("authstore: countUsers: %w", err)
	}
	return n, nil
}

func (s *authStore) insertSession(ctx context.Context, id, userID, tokenHash string, createdAt, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions_auth (id, user_id, token_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		id, userID, tokenHash, createdAt, expiresAt)
	if err != nil {
		return fmt.Errorf("authstore: insertSession: %w", err)
	}
	return nil
}

// getSessionByHash resolves a session token hash to its session and user role.
func (s *authStore) getSessionByHash(ctx context.Context, tokenHash string) (sessionID, userID, role string, expiresAt int64, revoked bool, err error) {
	var revokedInt int
	err = s.db.QueryRowContext(ctx,
		`SELECT s.id, s.user_id, u.role, s.expires_at, s.revoked
		 FROM sessions_auth s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = ?`, tokenHash).
		Scan(&sessionID, &userID, &role, &expiresAt, &revokedInt)
	if err != nil {
		return sessionID, userID, role, expiresAt, false, fmt.Errorf("authstore: getSessionByHash: %w", err)
	}
	return sessionID, userID, role, expiresAt, revokedInt != 0, nil
}

// revalidatePrincipal is the read-only check used by the SSE keepalive. It
// reports whether the stream's session or API key is still live. Any lookup
// error is fail-closed (false). It never writes and never audits.
func (s *authStore) revalidatePrincipal(ctx context.Context, sessionID, keyID string) bool {
	switch {
	case sessionID != "":
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM sessions_auth WHERE id = ? AND revoked = 0 AND expires_at > ?`,
			sessionID, time.Now().UnixMilli()).Scan(&one)
		return err == nil
	case keyID != "":
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM api_keys WHERE id = ? AND revoked = 0`, keyID).Scan(&one)
		return err == nil
	default:
		return false
	}
}

func (s *authStore) revokeSession(ctx context.Context, sessionID string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions_auth SET revoked = 1 WHERE id = ?`, sessionID); err != nil {
		return fmt.Errorf("authstore: revokeSession: %w", err)
	}
	return nil
}

// deleteExpiredSessions purges sessions that can no longer authenticate.
func (s *authStore) deleteExpiredSessions(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions_auth WHERE expires_at < ? OR revoked = 1`,
		time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("authstore: deleteExpiredSessions: %w", err)
	}
	return nil
}

// getApiKeyAuthByHash resolves an API key hash to its owning user, role and
// stored prefix. lastUsed is NULL until the key authenticates for the first
// time; callers use it to throttle the last_used write. The prefix is what
// audit records use to identify the key without storing its hash.
func (s *authStore) getApiKeyAuthByHash(ctx context.Context, keyHash string) (id, userID, username, role, keyPrefix string, lastUsed sql.NullInt64, revoked bool, err error) {
	var revokedInt int
	err = s.db.QueryRowContext(ctx,
		`SELECT k.id, k.user_id, u.username, u.role, k.key_prefix, k.last_used, k.revoked
		 FROM api_keys k JOIN users u ON u.id = k.user_id
		 WHERE k.key_hash = ?`, keyHash).
		Scan(&id, &userID, &username, &role, &keyPrefix, &lastUsed, &revokedInt)
	if err != nil {
		return id, userID, username, role, keyPrefix, lastUsed, false, fmt.Errorf("authstore: getApiKeyAuthByHash: %w", err)
	}
	return id, userID, username, role, keyPrefix, lastUsed, revokedInt != 0, nil
}
