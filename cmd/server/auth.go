package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "wacalls_session"
	auditChanCap      = 2048
	auditActorMax     = 64
	auditDrainTimeout = 3 * time.Second
	auditWriteTimeout = 2 * time.Second

	// auditInvalidCredCap bounds auth_failed rows produced by the middleware
	// per client IP in one window. Middleware audits are attacker-triggerable
	// (any bad token), so they must not become a write amplifier.
	auditInvalidCredCap    = 20
	auditInvalidCredWindow = time.Minute

	// sseTicketTTL bounds how long a one-time SSE ticket can be replayed if it
	// leaks (access log, Referer). sseTicketMaxEntries caps the in-memory map so
	// a flood of ticket requests cannot grow it without bound.
	sseTicketTTL           = 60 * time.Second
	sseTicketMaxEntries    = 10_000
	sseTicketPruneInterval = time.Second
)

type authContextKey struct{}

// authInfo is the authenticated principal attached to a request context.
type authInfo struct {
	userID    string
	username  string
	role      string
	sessionID string // empty for API-key authentication
	keyID     string // empty for session authentication
	method    string // "session", "api_key" or "disabled"
	// keyPrefix is the stored prefix of the authenticating API key. It is the
	// only key material safe to place in audit records and it is empty for
	// session authentication.
	keyPrefix string
}

// userFromContext reports the authenticated user id and role, if any.
func userFromContext(ctx context.Context) (userID, role string, ok bool) {
	info, ok := authInfoFromContext(ctx)
	if !ok {
		return "", "", false
	}
	return info.userID, info.role, true
}

// actorFromContext returns the audit actor for the authenticated principal:
// the user id for a session (or an auth-disabled principal), and
// "key:<prefix>" for an API key so the credential itself is attributable
// without ever storing its secret. It returns "" when no principal is attached.
func actorFromContext(ctx context.Context) string {
	info, ok := authInfoFromContext(ctx)
	if !ok {
		return ""
	}
	if info.method == "api_key" && info.keyPrefix != "" {
		return "key:" + info.keyPrefix
	}
	return info.userID
}

func authInfoFromContext(ctx context.Context) (authInfo, bool) {
	info, ok := ctx.Value(authContextKey{}).(authInfo)
	return info, ok
}

// authMiddleware enforces authentication for /api routes.
//
// Public: POST /api/auth/login and POST /api/auth/logout (both must work
// without a valid session), plus everything outside /api/ (static assets).
// Credentials are accepted, in order, from:
//
//	Authorization: Bearer <session token or api key>
//	X-API-Key: <api key>
//	?ticket=<sse ticket>(GET /api/events only, single use, 60s TTL)
//	cookie wacalls_session
//
// No credential is ever accepted as a general query parameter: EventSource
// cannot set headers, so /api/events callers obtain a short-lived one-time
// ticket from POST /api/auth/sse-ticket instead. API keys must arrive via
// X-API-Key or Authorization: Bearer; there is no ?apiKey= fallback. Query
// credentials would leak secrets into access logs, proxies and Referer
// headers. A present-but-malformed Authorization header is rejected outright
// instead of falling through to weaker sources.
//
// Session tokens and API keys are stored hashed (sha256 hex) and looked up by
// hash. A nil auth store makes this middleware a no-op; -auth-disabled injects a
// synthetic dev principal so handlers can still resolve a user.
func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authDisabled {
			// Synthetic principal so /api/auth/me and key management still
			// resolve a user when authentication is explicitly disabled.
			info := authInfo{userID: "dev", username: "dev", role: "admin", method: "disabled"}
			ctx := context.WithValue(r.Context(), authContextKey{}, info)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if s.auth == nil {
			if s.log != nil {
				s.log.Warn("auth store is nil; authentication disabled implicitly")
			} else {
				slog.Default().Warn("auth store is nil; authentication disabled implicitly")
			}
			next.ServeHTTP(w, r)
			return
		}
		if isPublicAuthPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// SSE tickets are single-use and scoped to GET /api/events. They are
		// consumed before any other credential source so a presented-but-dead
		// ticket cannot silently fall through to a weaker credential.
		if ticket := sseTicketFromRequest(r); ticket != "" {
			info, ok := s.consumeSSETicket(ticket)
			if !ok {
				s.auditInvalidCredential(r)
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			ctx := context.WithValue(r.Context(), authContextKey{}, info)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		creds := credentialsFromRequest(r)
		if creds.malformed {
			s.auditInvalidCredential(r)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if creds.sessionToken == "" && creds.apiKey == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		info, ok, err := s.authenticate(r.Context(), creds)
		if err != nil {
			s.log.Error("auth: credential lookup failed", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if !ok {
			// A credential was presented but is unusable (unknown/expired/
			// revoked session or key). Requests with no credential at all are
			// deliberately not audited: that would let anyone grow the audit
			// table with a single unauthenticated request.
			s.auditInvalidCredential(r)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		// CSRF: state-changing requests authenticated by cookie only must come
		// from the same host (or an allowed origin) when a browser supplies an
		// Origin header. Header credentials are not auto-attached, so exempt.
		if creds.fromCookie && !isSafeMethod(r.Method) {
			if origin := r.Header.Get("Origin"); origin != "" && !s.csrfOriginAllowed(r, origin) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
				return
			}
		}

		ctx := context.WithValue(r.Context(), authContextKey{}, info)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isSafeMethod reports whether the HTTP method is CSRF-safe by definition.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// csrfOriginAllowed reports whether origin matches the request host or is an
// explicitly allowlisted origin.
func (s *server) csrfOriginAllowed(r *http.Request, origin string) bool {
	if s.originAllowed(origin) {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// isPublicAuthPath reports whether the request may skip authentication. Only
// the POST variants are public; other methods still require a credential.
func isPublicAuthPath(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/api/auth/login", "/api/auth/logout":
		return true
	}
	return false
}

// requireRole wraps a handler so only the given role (or an auth-disabled /
// auth-less server) may reach it.
func (s *server) requireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil || s.authDisabled {
			if s.auth == nil && !s.authDisabled {
				if s.log != nil {
					s.log.Warn("auth store is nil; role check bypassed")
				} else {
					slog.Default().Warn("auth store is nil; role check bypassed")
				}
			}
			next(w, r)
			return
		}
		_, got, ok := userFromContext(r.Context())
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if got != role {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		next(w, r)
	}
}

// requestCredentials is the raw, unverified material extracted from a request.
type requestCredentials struct {
	sessionToken string
	apiKey       string
	// fromBearer marks a session token that arrived via "Authorization: Bearer".
	// Such a token may also be an API key (F1).
	fromBearer bool
	// fromCookie marks a session token read from the session cookie. Used to
	// scope CSRF checks to cookie-only authentication.
	fromCookie bool
	// malformed marks an Authorization header that is present but is not
	// "Bearer <non-empty>". Callers must reject, not downgrade.
	malformed bool
}

// credentialsFromRequest extracts credentials using the documented precedence.
func credentialsFromRequest(r *http.Request) requestCredentials {
	var c requestCredentials

	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > len("Bearer ") && strings.EqualFold(h[:len("Bearer ")], "Bearer ") {
			c.sessionToken = strings.TrimSpace(h[len("Bearer "):])
		}
		if c.sessionToken == "" {
			c.malformed = true
			return c
		}
		c.fromBearer = true
		return c
	}

	if k := strings.TrimSpace(r.Header.Get("X-API-Key")); k != "" {
		c.apiKey = k
		return c
	}

	if c.sessionToken == "" && c.apiKey == "" {
		if ck, err := r.Cookie(sessionCookieName); err == nil {
			c.sessionToken = strings.TrimSpace(ck.Value)
			c.fromCookie = c.sessionToken != ""
		}
	}
	return c
}

// authenticate resolves credentials to a principal. A non-nil error means the
// lookup itself failed (500); (zero, false, nil) means the credential missed.
func (s *server) authenticate(ctx context.Context, c requestCredentials) (authInfo, bool, error) {
	if c.sessionToken != "" {
		sessionID, userID, role, expiresAt, revoked, err := s.auth.getSessionByHash(ctx, sha256Hex(c.sessionToken))
		if err == nil {
			if revoked || expiresAt <= time.Now().UnixMilli() {
				// A known-but-unusable session must not fall through to weaker
				// credential sources.
				return authInfo{}, false, nil
			}
			return authInfo{userID: userID, role: role, sessionID: sessionID, method: "session"}, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return authInfo{}, false, err
		}
		if !c.fromBearer {
			return authInfo{}, false, nil
		}
		// A Bearer credential may be an API key rather than a session token.
		return s.apiKeyAuthenticate(ctx, c.sessionToken)
	}
	if c.apiKey != "" {
		// X-API-Key is an API key only; it is never retried as a session token.
		return s.apiKeyAuthenticate(ctx, c.apiKey)
	}
	return authInfo{}, false, nil
}

// apiKeyLastUsedIntervalMs throttles last_used writes: at most one UPDATE per
// key per minute, which keeps read-heavy API traffic from amplifying writes.
const apiKeyLastUsedIntervalMs int64 = 60_000

func (s *server) apiKeyAuthenticate(ctx context.Context, rawKey string) (authInfo, bool, error) {
	id, userID, username, role, keyPrefix, lastUsed, revoked, err := s.auth.getApiKeyAuthByHash(ctx, sha256Hex(rawKey))
	if errors.Is(err, sql.ErrNoRows) {
		return authInfo{}, false, nil
	}
	if err != nil {
		return authInfo{}, false, err
	}
	if revoked {
		return authInfo{}, false, nil
	}
	// Best effort: stamp last use without ever failing authentication on it.
	// Keep it inline (no goroutine) so the auth store's single connection is
	// not raced by a fire-and-forget write.
	now := time.Now().UnixMilli()
	if !lastUsed.Valid || lastUsed.Int64 == 0 || now-lastUsed.Int64 > apiKeyLastUsedIntervalMs {
		_ = s.auth.touchApiKeyLastUsed(ctx, id, now)
	}
	return authInfo{userID: userID, username: username, role: role, keyID: id, method: "api_key", keyPrefix: keyPrefix}, true, nil
}

func sha256Hex(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// newSessionToken returns 32 random bytes as hex.
func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sseTicket is the server-side state behind a one-time ?ticket= value. Only the
// ticket's sha256 is used as the map key, mirroring session/API-key storage.
type sseTicket struct {
	userID    string
	role      string
	sessionID string
	keyID     string
	keyPrefix string
	expiresAt time.Time
}

// sseTicketFromRequest extracts ?ticket= for GET /api/events only.
func sseTicketFromRequest(r *http.Request) string {
	if r.Method != http.MethodGet || r.URL.Path != "/api/events" {
		return ""
	}
	return strings.TrimSpace(r.URL.Query().Get("ticket"))
}

// issueSSETicket mints a single-use ticket for the given principal and returns
// the raw value plus its expiry in unix milliseconds.
func (s *server) issueSSETicket(info authInfo) (string, int64, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", 0, err
	}
	raw := hex.EncodeToString(b)
	now := time.Now()
	expiresAt := now.Add(sseTicketTTL)

	s.sseTicketMu.Lock()
	defer s.sseTicketMu.Unlock()
	s.pruneSSETicketsLocked(now)
	if len(s.sseTickets) >= sseTicketMaxEntries {
		// At the cap with nothing expired: evict the earliest-expiring ticket.
		var oldestKey string
		var oldest time.Time
		found := false
		for k, t := range s.sseTickets {
			if t == nil {
				delete(s.sseTickets, k)
				continue
			}
			if !found || t.expiresAt.Before(oldest) {
				oldestKey, oldest, found = k, t.expiresAt, true
			}
		}
		if found {
			delete(s.sseTickets, oldestKey)
		}
	}
	s.sseTickets[sha256Hex(raw)] = &sseTicket{
		userID:    info.userID,
		role:      info.role,
		sessionID: info.sessionID,
		keyID:     info.keyID,
		keyPrefix: info.keyPrefix,
		expiresAt: expiresAt,
	}
	return raw, expiresAt.UnixMilli(), nil
}

// consumeSSETicket validates and burns a ticket. A ticket is deleted on the
// first consume attempt, even when it is already expired, so it can never be
// replayed.
func (s *server) consumeSSETicket(raw string) (authInfo, bool) {
	if raw == "" {
		return authInfo{}, false
	}
	now := time.Now()
	hash := sha256Hex(raw)
	s.sseTicketMu.Lock()
	defer s.sseTicketMu.Unlock()
	s.pruneSSETicketsLocked(now)
	t := s.sseTickets[hash]
	if t == nil {
		return authInfo{}, false
	}
	delete(s.sseTickets, hash)
	if !now.Before(t.expiresAt) {
		return authInfo{}, false
	}
	method := "session"
	if t.keyID != "" || t.keyPrefix != "" {
		method = "api_key"
	}
	return authInfo{
		userID:    t.userID,
		role:      t.role,
		sessionID: t.sessionID,
		keyID:     t.keyID,
		keyPrefix: t.keyPrefix,
		method:    method,
	}, true
}

// pruneSSETicketsLocked drops expired tickets at most once per second.
func (s *server) pruneSSETicketsLocked(now time.Time) {
	if s.sseTickets == nil {
		s.sseTickets = make(map[string]*sseTicket)
		return
	}
	if !s.lastSSETicketPrune.IsZero() && now.Sub(s.lastSSETicketPrune) < sseTicketPruneInterval {
		return
	}
	s.lastSSETicketPrune = now
	for k, t := range s.sseTickets {
		if t == nil || !now.Before(t.expiresAt) {
			delete(s.sseTickets, k)
		}
	}
}

// generatePassword returns n random characters from a confusable-free alphabet
// using rejection sampling (no modulo bias).
func generatePassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	const maxByte = 256 - (256 % len(alphabet))
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) >= maxByte {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clientIP returns the peer address, or the rightmost X-Forwarded-For hop when
// the server is told to trust a front proxy.
//
// Only the rightmost hop is trusted: it is the one the trusted proxy appended
// to the header. Every hop to its left is client-supplied and therefore
// untrusted — a client can send "X-Forwarded-For: 1.2.3.4" itself and a naive
// leftmost read would let it spoof its source address (defeating per-IP rate
// limiting and audit attribution).
func (s *server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			hops := strings.Split(xff, ",")
			for i := len(hops) - 1; i >= 0; i-- {
				if hop := strings.TrimSpace(hops[i]); hop != "" {
					return hop
				}
			}
		}
	}
	return remoteIP(r)
}

// forwardedProto returns the first X-Forwarded-Proto hop, lowercased.
func forwardedProto(r *http.Request) string {
	v := r.Header.Get("X-Forwarded-Proto")
	if v == "" {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(strings.Split(v, ",")[0]))
}

// secureCookie reports whether issued cookies must carry the Secure attribute.
// A direct TLS connection always qualifies; behind a trusted proxy, a proven
// HTTPS external scheme qualifies too. The flag makes it explicit for
// deployments that terminate TLS elsewhere.
func (s *server) secureCookie(r *http.Request) bool {
	return s.cookieSecure || r.TLS != nil || (s.trustProxy && forwardedProto(r) == "https")
}

// auditWriter serializes audit inserts onto one goroutine so request handlers
// never block on the database.
type auditWriter struct {
	store *authStore
	log   *slog.Logger
	ch    chan auditEntry

	mu        sync.Mutex
	closed    bool
	done      chan struct{}
	closeOnce sync.Once
}

type auditEntry struct {
	ts        int64
	actor     string
	action    string
	resource  string
	ip        string
	userAgent string
	detail    string
}

func newAuditWriter(ctx context.Context, store *authStore, log *slog.Logger) *auditWriter {
	a := &auditWriter{
		store: store,
		log:   log,
		ch:    make(chan auditEntry, auditChanCap),
		done:  make(chan struct{}),
	}
	go a.run(ctx)
	return a
}

func (a *auditWriter) run(ctx context.Context) {
	defer close(a.done)
	for {
		select {
		case e, ok := <-a.ch:
			if !ok {
				return
			}
			a.write(e)
		case <-ctx.Done():
			// Never drop already-queued entries just because the context ended:
			// keep draining until Close closes the channel.
			for e := range a.ch {
				a.write(e)
			}
			return
		}
	}
}

func (a *auditWriter) write(e auditEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
	defer cancel()
	if err := a.store.insertAudit(ctx, e.ts, e.actor, e.action, e.resource, e.ip, e.userAgent, e.detail); err != nil && a.log != nil {
		a.log.Warn("audit write failed", "action", e.action, "err", err)
	}
}

// Close stops accepting entries, drains what is queued (bounded) and waits for
// the writer goroutine. Safe to call more than once and on a nil writer.
func (a *auditWriter) Close() {
	if a == nil {
		return
	}
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		close(a.ch)
		a.mu.Unlock()
	})
	ctx, cancel := context.WithTimeout(context.Background(), auditDrainTimeout)
	defer cancel()
	select {
	case <-a.done:
	case <-ctx.Done():
		if a.log != nil {
			a.log.Warn("audit drain timed out", "timeout", auditDrainTimeout)
		}
	}
}

// record enqueues an audit entry without blocking; a full channel drops it.
func (a *auditWriter) record(e auditEntry) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	select {
	case a.ch <- e:
	default:
		if a.log != nil {
			a.log.Warn("audit queue full, dropping entry", "action", e.action, "actor", e.actor)
		}
	}
}

// auditRecord enqueues an audit entry for the current request.
func (s *server) auditRecord(r *http.Request, actor, action, resource, detail string) {
	if s.audit == nil {
		return
	}
	s.audit.record(auditEntry{
		ts:        time.Now().UnixMilli(),
		actor:     actor,
		action:    action,
		resource:  resource,
		ip:        truncateBytes(s.clientIP(r), auditIPMax),
		userAgent: r.UserAgent(),
		detail:    detail,
	})
}

// failedAuthWindow counts middleware-detected invalid credentials for one IP.
type failedAuthWindow struct {
	count int
	start time.Time
}

// auditInvalidCredential records an auth_failed row for a presented-but-invalid
// credential. It is capped per client IP and drops silently past the cap.
func (s *server) auditInvalidCredential(r *http.Request) {
	if s.audit == nil {
		return
	}
	if !s.allowAuthFailureAudit(s.clientIP(r)) {
		return
	}
	s.auditRecord(r, "unknown", "auth_failed", "auth", "invalid_credential")
}

// allowAuthFailureAudit charges one auth_failed audit against ip's window. It
// is shared by middleware and login failures so a single per-IP budget bounds
// all attacker-triggerable auth audit rows. Prunes expired windows lazily so
// the map stays small under a spray of source IPs.
func (s *server) allowAuthFailureAudit(ip string) bool {
	now := time.Now()
	s.authFailMu.Lock()
	defer s.authFailMu.Unlock()
	if s.authFailCounts == nil {
		s.authFailCounts = make(map[string]*failedAuthWindow)
	}
	if s.lastAuthFailPrune.IsZero() || now.Sub(s.lastAuthFailPrune) >= auditInvalidCredWindow {
		s.lastAuthFailPrune = now
		for k, w := range s.authFailCounts {
			if w == nil || now.Sub(w.start) >= auditInvalidCredWindow {
				delete(s.authFailCounts, k)
			}
		}
	}
	w := s.authFailCounts[ip]
	if w == nil || now.Sub(w.start) >= auditInvalidCredWindow {
		w = &failedAuthWindow{start: now}
		s.authFailCounts[ip] = w
	}
	if w.count >= auditInvalidCredCap {
		return false
	}
	w.count++
	return true
}

// runAuditRetention sweeps audit rows older than auditRetention every
// auditPruneInterval until ctx is canceled. Failures are logged and retried on
// the next tick; retention is never allowed to fail a request.
func (s *server) runAuditRetention(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(auditPruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				purgeCtx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
				err := s.auth.pruneAudit(purgeCtx, time.Now().Add(-auditRetention).UnixMilli())
				cancel()
				if err != nil && s.log != nil {
					s.log.Warn("audit retention purge failed", "err", err)
				}
			}
		}
	}()
}
