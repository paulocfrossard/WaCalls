package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	loginMaxAttempts = 10
	// loginMaxAttemptsPerIP and loginMaxAttemptsGlobal bound username rotation:
	// an attacker changing the username on every request would otherwise get a
	// fresh per-username bucket each time and drive CPU through bcrypt. Both
	// buckets share the same window and are reserved atomically with the
	// IP|username bucket; a successful login refunds its own unit from each,
	// so only failed attempts consume them.
	loginMaxAttemptsPerIP  = 30
	loginMaxAttemptsGlobal = 300
	loginWindow            = time.Minute
	// loginMaxUsername bounds the username before it is used as a map key.
	loginMaxUsername = 64
	// loginMaxEntries bounds the limiter map so a spray of distinct keys cannot
	// grow it without limit.
	loginMaxEntries = 10_000
	// loginPruneInterval makes pruning lazy: at most once per second instead of
	// a full scan on every attempt.
	loginPruneInterval = time.Second
	// loginMaxKeyIP bounds the client-IP component of the key; X-Forwarded-For
	// is attacker-controlled when a proxy is trusted.
	loginMaxKeyIP = 64
	// apiKeyPrefix marks raw API keys so they are recognisable in logs/support.
	apiKeyPrefix = "wc_"
	// apiKeyPrefixLen is how many leading characters are stored as the
	// searchable/labelled prefix.
	apiKeyPrefixLen = 8
	apiKeyNameMax   = 128
	// dummyPasswordPlaintext seeds the startup dummy hash. The server generates
	// a fresh bcrypt hash for it at bcryptCost so unknown-username logins pay
	// the same work factor as real ones.
	dummyPasswordPlaintext = "dummy-password-for-timing-parity"
	// dummyPasswordHash is a bcrypt cost-12 hash of dummyPasswordPlaintext,
	// used only when startup generation fails (and by tests that construct a
	// server without newServer). It never matches a real password.
	dummyPasswordHash = "$2a$12$LoaxhwRzlYBkdH3zlVuQ0uLUxLvQF/1j31cpia7zIhPgPv7/ZQ/kK"
)

type loginAttempt struct {
	count       int
	windowStart time.Time
}

// loginKey scopes the login rate limiter to one client/username pair. Both
// components are capped so the map key itself cannot be made arbitrarily large.
func (s *server) loginKey(r *http.Request, username string) string {
	username = truncateBytes(username, loginMaxUsername)
	ip := truncateBytes(s.clientIP(r), loginMaxKeyIP)
	return ip + "|" + username
}

// loginBucket is one independently-limited counter charged on login entry.
type loginBucket struct {
	key   string
	limit int
}

// reserveLoginAttempts atomically charges one attempt against every bucket.
// It returns false when the caller is already over any limit; a rejected
// attempt is charged against none of them, so a limiter hit cannot itself
// extend a lockout. All buckets are reserved under one lock, closing the
// check-then-record TOCTOU window.
func (s *server) reserveLoginAttempts(buckets []loginBucket) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	now := time.Now()
	s.pruneLoginAttemptsLocked(now)
	if s.loginAttempts == nil {
		s.loginAttempts = make(map[string]*loginAttempt)
	}
	for _, b := range buckets {
		a := s.loginAttempts[b.key]
		if a != nil && now.Sub(a.windowStart) < loginWindow && a.count >= b.limit {
			return false
		}
	}
	for _, b := range buckets {
		a := s.loginAttempts[b.key]
		if a == nil || now.Sub(a.windowStart) >= loginWindow {
			a = &loginAttempt{windowStart: now}
			s.loginAttempts[b.key] = a
		}
		a.count++
	}
	s.evictLoginAttemptsLocked()
	return true
}

// clearLoginAttempt settles the reservation taken by a successful login.
//
// The IP|username entry is deleted outright: a decrement would leave earlier
// failures counted against the next window, and a clean slate is the
// documented post-success behaviour. The shared per-IP and global buckets are
// different — they exist to bound total login work regardless of username, so
// a success refunds exactly one unit from each (the reservation this request
// made) and never more. That keeps the invariant that only failed attempts
// consume them: failures are not refunded.
func (s *server) clearLoginAttempt(buckets []loginBucket) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if len(buckets) == 0 {
		return
	}
	// buckets[0] is IP|username; the rest are the shared counters.
	delete(s.loginAttempts, buckets[0].key)
	for _, b := range buckets[1:] {
		s.refundLoginAttemptLocked(b.key)
	}
}

// refundLoginAttemptLocked decrements one shared counter by a single unit,
// flooring at zero and dropping the entry when nothing is left. An expired
// window is already back at zero conceptually, so it is removed instead of
// decremented.
func (s *server) refundLoginAttemptLocked(key string) {
	a := s.loginAttempts[key]
	if a == nil {
		return
	}
	if time.Since(a.windowStart) >= loginWindow {
		delete(s.loginAttempts, key)
		return
	}
	a.count--
	if a.count <= 0 {
		delete(s.loginAttempts, key)
	}
}

// pruneLoginAttemptsLocked drops expired entries at most once per second.
func (s *server) pruneLoginAttemptsLocked(now time.Time) {
	if s.loginAttempts == nil {
		s.loginAttempts = make(map[string]*loginAttempt)
		return
	}
	if now.Sub(s.lastLoginPrune) < loginPruneInterval {
		return
	}
	s.lastLoginPrune = now
	for k, a := range s.loginAttempts {
		if a == nil || now.Sub(a.windowStart) >= loginWindow {
			delete(s.loginAttempts, k)
		}
	}
}

// evictLoginAttemptsLocked keeps the map bounded by evicting the oldest window
// whenever it grows past loginMaxEntries.
func (s *server) evictLoginAttemptsLocked() {
	for len(s.loginAttempts) > loginMaxEntries {
		var oldestKey string
		var oldest time.Time
		found := false
		for k, a := range s.loginAttempts {
			if a == nil {
				delete(s.loginAttempts, k)
				continue
			}
			if !found || a.windowStart.Before(oldest) {
				oldestKey, oldest, found = k, a.windowStart, true
			}
		}
		if !found {
			return
		}
		delete(s.loginAttempts, oldestKey)
	}
}

// handleLogin authenticates a username/password pair and issues a session.
//
//	POST /api/auth/login {"username","password"}
//	→ 201 {"expiresAt","user":{"id","username","role"}}
//
// The session token is delivered only as an HttpOnly cookie, never in the
// response body: browsers use the cookie and API clients use API keys, so
// exposing the token to JavaScript would only widen its blast radius.
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "auth not configured"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if len(body.Username) > loginMaxUsername {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username too long"})
		return
	}
	if body.Username == "" || body.Password == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	key := s.loginKey(r, body.Username)
	// The per-IP and global buckets bound username rotation: without them an
	// attacker could pick a fresh username per request and never refill a
	// bucket while still forcing one bcrypt comparison each time.
	buckets := []loginBucket{
		{key: key, limit: loginMaxAttempts},
		{key: "ip:" + truncateBytes(s.clientIP(r), loginMaxKeyIP), limit: loginMaxAttemptsPerIP},
		{key: "global", limit: loginMaxAttemptsGlobal},
	}
	// Reserve atomically on entry (before any DB or bcrypt work); a rejected
	// attempt is not charged and is deliberately not audited (otherwise rate
	// limiting itself becomes a write amplifier).
	if !s.reserveLoginAttempts(buckets) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many login attempts"})
		return
	}

	userID, passwordHash, role, err := s.auth.getUserByUsername(r.Context(), body.Username)
	if errors.Is(err, sql.ErrNoRows) {
		// Compare against the startup-generated dummy hash so the unknown-user
		// path costs the same bcrypt work factor as a real comparison.
		dummy := s.dummyHash
		if len(dummy) == 0 {
			dummy = []byte(dummyPasswordHash)
		}
		_ = bcrypt.CompareHashAndPassword(dummy, []byte(body.Password))
		s.auditLoginFailure(r, body.Username, "unknown_user")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	if err != nil {
		s.log.Error("login: user lookup failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(body.Password)); err != nil {
		s.auditLoginFailure(r, body.Username, "bad_password")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	token, err := newSessionToken()
	if err != nil {
		s.log.Error("login: token generation failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	ttl := s.sessionTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now := time.Now()
	expiresAt := now.Add(ttl).UnixMilli()
	sessionID := newSessionID()
	if err := s.auth.insertSession(r.Context(), sessionID, userID, sha256Hex(token), now.UnixMilli(), expiresAt); err != nil {
		s.log.Error("login: session insert failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Settle the limiter only once the session exists: a failed insert must
	// not reset the attempt budget, or a 500 becomes a brute-force accelerator.
	// This clears the IP|username key and refunds the shared counters so only
	// failed attempts consume them.
	s.clearLoginAttempt(buckets)
	// Best effort: drop sessions that can no longer authenticate.
	if err := s.auth.deleteExpiredSessions(r.Context()); err != nil {
		s.log.Warn("login: session purge failed", "err", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  time.UnixMilli(expiresAt),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookie(r),
	})
	s.auditRecord(r, body.Username, "login_success", "auth", "session="+sessionID)
	// No token in the body: the HttpOnly cookie above is the browser
	// credential, and API clients authenticate with API keys.
	writeJSON(w, http.StatusCreated, map[string]any{
		"expiresAt": expiresAt,
		"user": map[string]string{
			"id":       userID,
			"username": body.Username,
			"role":     role,
		},
	})
}

// auditLoginFailure records a failed login attempt, sharing the middleware's
// per-IP auth-failure budget. Over the cap the row is skipped; the caller still
// returns 401, so this bounds the audit table without changing auth semantics.
func (s *server) auditLoginFailure(r *http.Request, username, detail string) {
	if s.audit == nil {
		return
	}
	if !s.allowAuthFailureAudit(s.clientIP(r)) {
		return
	}
	s.auditRecord(r, username, "auth_failed", "auth", detail)
}

// handleSSETicket mints a short-lived, single-use ticket for GET /api/events.
// EventSource cannot set headers, so the client exchanges its session for this
// ticket instead of putting a 24h session token in the URL.
//
//	POST /api/auth/sse-ticket → 201 {"ticket":"<hex>","expiresAt":<unix millis>}
func (s *server) handleSSETicket(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	info, ok := authInfoFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ticket, expiresAt, err := s.issueSSETicket(info)
	if err != nil {
		s.log.Error("sse ticket: generation failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ticket":    ticket,
		"expiresAt": expiresAt,
	})
}

// handleLogout revokes the presented session (when valid) and always clears the
// session cookie. It is public: calling it without credentials is a no-op
// success so a stale cookie can still be removed.
//
//	POST /api/auth/logout → 204
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	actor := s.revokeSessionFromRequest(r)
	// Logout is a public path, so the middleware normally leaves no principal
	// in the context; when one is present prefer it.
	if ctxActor := actorFromContext(r.Context()); ctxActor != "" {
		actor = ctxActor
	}
	if actor != "" {
		s.auditRecord(r, actor, "logout", "auth", "session")
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookie(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// revokeSessionFromRequest revokes the session token carried by the request
// (Bearer header or session cookie) and returns the owning user id, if any.
// An unusable token is not an error: logout stays idempotent.
func (s *server) revokeSessionFromRequest(r *http.Request) string {
	if s.auth == nil {
		return ""
	}
	token := ""
	if h := r.Header.Get("Authorization"); len(h) > len("Bearer ") && strings.EqualFold(h[:len("Bearer ")], "Bearer ") {
		token = strings.TrimSpace(h[len("Bearer "):])
	}
	if token == "" {
		if c, err := r.Cookie(sessionCookieName); err == nil {
			token = strings.TrimSpace(c.Value)
		}
	}
	if token == "" {
		return ""
	}
	sessionID, userID, _, expiresAt, revoked, err := s.auth.getSessionByHash(r.Context(), sha256Hex(token))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Error("logout: session lookup failed", "err", err)
		}
		return ""
	}
	if revoked || expiresAt <= time.Now().UnixMilli() {
		return userID
	}
	if err := s.auth.revokeSession(r.Context(), sessionID); err != nil {
		s.log.Error("logout: revoke failed", "err", err)
	} else if s.broker != nil {
		s.broker.Revoke(ssePrincipal{sessionID: sessionID})
	}
	return userID
}

// handleMe returns the authenticated principal.
//
//	GET /api/auth/me → 200 {"user":{"id","username","role"}}
func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, role, ok := userFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	info, _ := authInfoFromContext(r.Context())
	username := info.username
	if username == "" && s.auth != nil && !s.authDisabled {
		var err error
		username, role, err = s.auth.getUserByID(r.Context(), userID)
		if err != nil {
			s.log.Error("me: user lookup failed", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]string{
			"id":       userID,
			"username": username,
			"role":     role,
		},
	})
}

// newApiKey returns a fresh raw API key: "wc_" followed by 32 random bytes in
// hex. Only its sha256 hash is ever persisted.
func newApiKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return apiKeyPrefix + hex.EncodeToString(b), nil
}

// handleApiKeyCreate issues an API key owned by the calling admin.
//
//	POST /api/auth/keys {"name":"..."}
//	→ 201 {"id","name","key","prefix","createdAt"}
//
// The raw key is returned exactly once; subsequent reads only expose prefix.
func (s *server) handleApiKeyCreate(w http.ResponseWriter, r *http.Request) {
	// requireRole opens up for -auth-disabled, but minting a long-lived
	// credential on a server with no authentication is a privilege escalation:
	// the key would survive a later boot with auth enabled.
	if s.authDisabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "auth disabled"})
		return
	}
	if s.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "auth not configured"})
		return
	}
	userID, _, ok := userFromContext(r.Context())
	if !ok || userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "api keys require an authenticated user"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	if len(name) > apiKeyNameMax {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name too long"})
		return
	}
	raw, err := newApiKey()
	if err != nil {
		s.log.Error("api key: generation failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	id := newSessionID()
	prefix := raw[:apiKeyPrefixLen]
	createdAt := time.Now().UnixMilli()
	if err := s.auth.insertApiKey(r.Context(), id, userID, name, sha256Hex(raw), prefix, createdAt); err != nil {
		s.log.Error("api key: insert failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	actor := actorFromContext(r.Context())
	if actor == "" {
		actor = userID
	}
	s.auditRecord(r, actor, "create_key", "api_key", "id="+id)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        id,
		"name":      name,
		"key":       raw,
		"prefix":    prefix,
		"createdAt": createdAt,
	})
}

// handleApiKeyList lists API keys without exposing hashes or raw values.
//
//	GET /api/auth/keys → 200 {"keys":[{"id","name","prefix","createdAt","lastUsed","revoked"}]}
func (s *server) handleApiKeyList(w http.ResponseWriter, r *http.Request) {
	// Mirrors handleApiKeyCreate: key management is off while authentication
	// is disabled, so listing (and revoking) cannot leak or mutate credentials
	// that only exist for a later, authenticated boot.
	if s.authDisabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "auth disabled"})
		return
	}
	if s.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "auth not configured"})
		return
	}
	keys, err := s.auth.listApiKeys(r.Context())
	if err != nil {
		s.log.Error("api key: list failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	type view struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Prefix    string `json:"prefix"`
		CreatedAt int64  `json:"createdAt"`
		LastUsed  *int64 `json:"lastUsed"`
		Revoked   bool   `json:"revoked"`
	}
	out := make([]view, 0, len(keys))
	for _, k := range keys {
		var lastUsed *int64
		if k.lastUsed.Valid {
			v := k.lastUsed.Int64
			lastUsed = &v
		}
		out = append(out, view{
			ID:        k.id,
			Name:      k.name,
			Prefix:    k.prefix,
			CreatedAt: k.createdAt,
			LastUsed:  lastUsed,
			Revoked:   k.revoked,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// handleApiKeyDelete revokes an API key.
//
//	DELETE /api/auth/keys/{id} → 204 | 404
func (s *server) handleApiKeyDelete(w http.ResponseWriter, r *http.Request) {
	// Same guard as create/list: a server with authentication disabled must
	// not mutate credentials it cannot authenticate the caller for.
	if s.authDisabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "auth disabled"})
		return
	}
	if s.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "auth not configured"})
		return
	}
	id := r.PathValue("id")
	if err := s.auth.revokeApiKey(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such api key"})
			return
		}
		s.log.Error("api key: revoke failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if s.broker != nil {
		s.broker.Revoke(ssePrincipal{keyID: id})
	}
	userID, _, _ := userFromContext(r.Context())
	actor := actorFromContext(r.Context())
	if actor == "" {
		actor = userID
	}
	s.auditRecord(r, actor, "revoke_key", "api_key", "id="+id)
	w.WriteHeader(http.StatusNoContent)
}
