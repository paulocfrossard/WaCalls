package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/logbuf"

	"go.mau.fi/whatsmeow/types"
)

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/me", s.handleMe)
	// Short-lived, single-use credential for EventSource (which cannot send
	// headers); keeps long-lived session tokens out of URLs.
	mux.HandleFunc("POST /api/auth/sse-ticket", s.handleSSETicket)

	// API key management (admin only). Raw keys are only ever returned by the
	// create call.
	mux.HandleFunc("POST /api/auth/keys", s.requireRole("admin", s.handleApiKeyCreate))
	mux.HandleFunc("GET /api/auth/keys", s.requireRole("admin", s.handleApiKeyList))
	mux.HandleFunc("DELETE /api/auth/keys/{id}", s.requireRole("admin", s.handleApiKeyDelete))

	mux.HandleFunc("GET /api/sessions", s.handleSessionList)
	mux.HandleFunc("POST /api/sessions", s.handleSessionCreate)
	mux.HandleFunc("DELETE /api/sessions/{sid}", s.handleSessionDelete)
	mux.HandleFunc("POST /api/sessions/{sid}/logout", s.handleSessionLogout)
	mux.HandleFunc("POST /api/sessions/{sid}/pair", s.handleSessionPair)
	mux.HandleFunc("POST /api/sessions/{sid}/calls", s.handleStartCall)
	mux.HandleFunc("POST /api/sessions/{sid}/calls/{id}/webrtc", s.handleWebRTC)
	mux.HandleFunc("POST /api/sessions/{sid}/calls/{id}/accept", s.handleAccept)
	mux.HandleFunc("POST /api/sessions/{sid}/calls/{id}/reject", s.handleReject)
	mux.HandleFunc("DELETE /api/sessions/{sid}/calls/{id}", s.handleEndCall)
	mux.HandleFunc("GET /api/sessions/{sid}/history", s.handleHistory)

	mux.HandleFunc("GET /api/events", s.handleEvents)

	// Webhook management (outbound HTTP POST notifications). Requires auth;
	// the payload itself is HMAC-signed so external receivers can verify
	// authenticity — see docs/api.md.
	mux.HandleFunc("POST /api/webhooks", s.handleWebhookCreate)
	mux.HandleFunc("GET /api/webhooks", s.handleWebhookList)
	mux.HandleFunc("DELETE /api/webhooks/{id}", s.handleWebhookDelete)

	// Debug log ring (fed by logbuf; toggle with -debug-log). Admin-only.
	mux.HandleFunc("GET /api/debug/logs", s.requireRole("admin", s.handleDebugLogs))

	if s.staticDir != "" {
		if _, err := os.Stat(s.staticDir); err == nil {
			mux.Handle("/", s.spaHandler(s.staticDir))
		}
	}
	return s.withCORS(s.authMiddleware(mux))
}

// spaHandler serves the built client with history-API fallback:
//
//   - paths under /api/ are never static: unknown API routes get a JSON 404;
//   - an existing file under dir is served as-is;
//   - a missing path that has a file extension is a JSON 404 (no shell);
//   - anything else is the SPA shell (index.html) for GET/HEAD;
//   - other methods get 405.
func (s *server) spaHandler(dir string) http.Handler {
	root, err := filepath.Abs(dir)
	if err != nil {
		root = dir
	}
	root = filepath.Clean(root)
	index := filepath.Join(root, "index.html")

	notFound := func(w http.ResponseWriter) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			notFound(w)
			return
		}
		// Reject Windows separator smuggling after URL-decoding: on Windows,
		// "..\.." and percent-encoded "%5c" would otherwise reach the
		// filesystem through filepath.FromSlash while path.Clean only knows
		// about "/". Check the raw form too so double-encoded variants are
		// refused as well.
		decoded := r.URL.Path
		if unescaped, uerr := url.PathUnescape(decoded); uerr == nil {
			decoded = unescaped
		}
		if strings.ContainsRune(decoded, '\\') ||
			strings.Contains(strings.ToLower(decoded), "%5c") ||
			strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%5c") ||
			strings.Contains(strings.ToLower(r.URL.RawPath), "%5c") {
			notFound(w)
			return
		}
		// Clean the request path before touching the filesystem so ".."
		// segments cannot escape the static root.
		rel := strings.TrimPrefix(path.Clean("/"+decoded), "/")
		if rel != "" {
			abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
			if !withinRoot(root, abs) {
				notFound(w)
				return
			}
			if serveStaticFile(w, r, abs) {
				return
			}
		}
		// A path that looks like a concrete asset (it has an extension) but did
		// not resolve to a file is a missing asset, not a client-side route.
		// Falling back to index.html would answer JS/CSS/image requests with
		// HTML and hide the 404 from monitoring.
		if rel != "" && filepath.Ext(rel) != "" {
			notFound(w)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if !serveStaticFile(w, r, index) {
			notFound(w)
		}
	})
}

// withinRoot reports whether abs is root itself or lives underneath it. Both
// paths are expected to be already cleaned.
func withinRoot(root, abs string) bool {
	if abs == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(abs, prefix)
}

// serveStaticFile serves one already-resolved path, reporting whether it did.
// It uses ServeContent (not ServeFile) so the response depends on the resolved
// path rather than the raw request URL.
func serveStaticFile(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := os.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	return true
}

// withCORS sets CORS headers. With no configured origins it falls back to a
// wildcard (dev); otherwise it echoes only allowlisted request origins.
func (s *server) withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.allowedOrigins) == 0 {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if origin := r.Header.Get("Origin"); origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Client-Id, Authorization, X-API-Key")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *server) originAllowed(origin string) bool {
	for _, allowed := range s.allowedOrigins {
		if strings.EqualFold(strings.TrimSuffix(allowed, "/"), strings.TrimSuffix(origin, "/")) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func clientID(r *http.Request) string {
	if id := r.Header.Get("X-Client-Id"); id != "" {
		return id
	}
	return r.URL.Query().Get("clientId")
}

func (s *server) sessionByID(w http.ResponseWriter, sid string) *Session {
	sess, ok := s.sessions.Get(sid)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session"})
		return nil
	}
	return sess
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.broker.serveSSE(w, r, clientID(r))
}

// handleWebhookCreate registers an outbound webhook endpoint.
//
//	POST /api/webhooks {"url": "...", "secret": "...", "events": ["incoming", "call-ended"]}
//	→ 201 {"id": "..."}
//
// An empty or omitted "events" list subscribes the webhook to every event.
// Each event name must be one of the allowlisted types (incoming, call-status,
// call-ended); unknown or empty names are rejected with 400.
func (s *server) handleWebhookCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		URL    string   `json:"url"`
		Secret string   `json:"secret"`
		Events []string `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	if err := validateWebhookURL(body.URL, s.webhookAllowHosts); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	body.Secret = strings.TrimSpace(body.Secret)
	if body.Secret == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "secret required"})
		return
	}
	// Normalize and validate the events list against the allowlist.
	normalized := make([]string, 0, len(body.Events))
	for _, e := range body.Events {
		e = strings.TrimSpace(e)
		if e == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty event name"})
			return
		}
		if !webhookEvents[e] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown event: " + e})
			return
		}
		normalized = append(normalized, e)
	}
	id, err := s.webhooks.Create(r.Context(), body.URL, body.Secret, normalized)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// handleWebhookList returns all registered webhooks. The secret is intentionally
// omitted: it is write-only (used to HMAC-sign outbound payloads).
func (s *server) handleWebhookList(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.webhooks.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	type view struct {
		ID        string    `json:"id"`
		URL       string    `json:"url"`
		Events    []string  `json:"events"`
		CreatedAt time.Time `json:"createdAt"`
	}
	out := make([]view, 0, len(hooks))
	for _, h := range hooks {
		events := h.Events
		if events == nil {
			events = []string{}
		}
		out = append(out, view{ID: h.ID, URL: h.URL, Events: events, CreatedAt: h.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

// handleWebhookDelete removes a webhook by id.
//
//	DELETE /api/webhooks/{id} → 204 | 404 | 500
func (s *server) handleWebhookDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.webhooks.Delete(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such webhook"})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDebugLogs serves the in-memory diagnostic ring for the front-end logs
// page.
//
//	GET /api/debug/logs?since=<seq>&limit=100
//	→ {"events":[{"seq","ts","source","level","msg","fields"}],"nextSeq":N}
//
// Poll with since=nextSeq to tail. limit is capped at 500 (default 100).
//
// SECURITY: admin role required. The payload exposes call IDs, SSRCs and
// audio-level telemetry, so keep the server on a trusted network as well.
func (s *server) handleDebugLogs(w http.ResponseWriter, r *http.Request) {
	since := int64(0)
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			since = n
		}
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, 500)
		}
	}
	events, nextSeq := logbuf.Default().EventsSince(since, limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"events":  events,
		"nextSeq": nextSeq,
	})
}

func (s *server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.sessions.infos()})
}

func (s *server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "Session"
	}
	id, err := s.sessions.Create(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if s.sessionByID(w, sid) == nil {
		return
	}
	if err := s.sessions.Delete(r.Context(), sid); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleSessionLogout(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if s.sessionByID(w, sid) == nil {
		return
	}
	if err := s.sessions.Logout(r.Context(), sid); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleSessionPair(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if s.sessionByID(w, sid) == nil {
		return
	}
	if err := s.sessions.Pair(sid); err != nil {
		if strings.Contains(err.Error(), "already paired") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleStartCall(w http.ResponseWriter, r *http.Request) {
	if sess := s.sessionByID(w, r.PathValue("sid")); sess != nil {
		s.doStartCall(sess, w, r)
	}
}

func (s *server) handleWebRTC(w http.ResponseWriter, r *http.Request) {
	if sess := s.sessionByID(w, r.PathValue("sid")); sess != nil {
		s.doWebRTC(sess, w, r)
	}
}

func (s *server) handleAccept(w http.ResponseWriter, r *http.Request) {
	if sess := s.sessionByID(w, r.PathValue("sid")); sess != nil {
		s.doAccept(sess, w, r)
	}
}

func (s *server) handleReject(w http.ResponseWriter, r *http.Request) {
	if sess := s.sessionByID(w, r.PathValue("sid")); sess != nil {
		s.doReject(sess, w, r)
	}
}

func (s *server) handleEndCall(w http.ResponseWriter, r *http.Request) {
	if sess := s.sessionByID(w, r.PathValue("sid")); sess != nil {
		s.doEndCall(sess, w, r)
	}
}

func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if sess := s.sessionByID(w, r.PathValue("sid")); sess != nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": s.broker.historyRows(sess.id, 50)})
	}
}

func (s *server) doStartCall(sess *Session, w http.ResponseWriter, r *http.Request) {
	sess.mu.Lock()
	client := sess.client
	sess.mu.Unlock()
	if client.Store.ID == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not paired"})
		return
	}
	var body struct {
		Phone      string `json:"phone"`
		DurationMs int    `json:"duration_ms"`
		Record     bool   `json:"record"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Phone) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "phone required"})
		return
	}
	owner := clientID(r)
	if other := s.broker.ownerActiveCall(owner); other != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "operator already on a call"})
		return
	}
	if max := s.sessions.maxCalls; max > 0 && sess.reg.count() >= max {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "max concurrent calls"})
		return
	}
	peer := types.NewJID(normalizePhone(body.Phone), types.DefaultUserServer)

	callID, err := sess.startOutgoing(r.Context(), peer, false)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.broker.upsertCall(CallRecord{
		SessionID: sess.id, CallID: callID, Owner: &owner, Direction: "outbound", Peer: peer.String(),
		StartedAt: time.Now().UnixMilli(), Status: StatusRinging,
	})
	writeJSON(w, http.StatusOK, map[string]any{"call": map[string]string{"callId": callID}})
}

func (s *server) doWebRTC(sess *Session, w http.ResponseWriter, r *http.Request) {
	callID := r.PathValue("id")
	ac, ok := sess.reg.get(callID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such call"})
		return
	}
	var body struct {
		SDPOffer string `json:"sdp_offer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SDPOffer == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sdp_offer required"})
		return
	}
	bridge, answer, err := NewBridge(body.SDPOffer, s.log)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	bridge.SetCallID(callID)

	// Track every assignment: OnBrowserPCM assigned twice for the same call
	// (two bridges surviving setBridge) is a duplication path.
	logbuf.LogEvent("webrtc-assign", logbuf.LevelInfo, "OnBrowserPCM assigned", map[string]any{
		"call_id": callID, "session_id": sess.id,
	})
	bridge.OnBrowserPCM = func(pcm []float32) {
		// Rate-limited 5/s: mic pushes ~125 frames/s into this closure.
		if logbuf.Enabled() && logbuf.Allow("uplink-mic:"+callID, 5) {
			rms, peak := logbuf.PCMStats(pcm)
			logbuf.LogEvent("uplink-mic", logbuf.LevelDebug, "browser pcm captured", map[string]any{
				"call_id": callID,
				"len":     len(pcm),
				"rms":     logbuf.Round4(rms),
				"peak":    logbuf.Round4(peak),
			})
		}
		ac.cm.FeedCapturedPCM(pcm)
	}
	bridge.OnTerminalICE = func() {
		go sess.terminateCall(callID, core.EndCallReasonUserEnded)
	}
	sess.setBridge(callID, bridge)
	writeJSON(w, http.StatusOK, map[string]string{"sdp_answer": answer})
}

func (s *server) doAccept(sess *Session, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ac, ok := sess.reg.get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such call"})
		return
	}
	owner := clientID(r)
	if other := s.broker.ownerActiveCall(owner); other != "" && other != id {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "operator already on a call"})
		return
	}
	if !s.broker.setOwner(id, owner) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "claimed by another client"})
		return
	}
	s.broker.emitIncomingClaimed(sess.id, id, owner)
	if err := ac.cm.AcceptCall(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"call": map[string]string{"callId": id}})
}

func (s *server) doReject(sess *Session, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if ac, ok := sess.reg.get(id); ok {
		_ = ac.cm.RejectCall(r.Context(), id, core.EndCallReasonDeclined)
	}
	sess.removeCall(id)
	s.broker.endCall(id, string(core.EndCallReasonDeclined))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) doEndCall(sess *Session, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if ac, ok := sess.reg.get(id); ok {
		_ = ac.cm.EndCall(r.Context(), core.EndCallReasonUserEnded)
	}
	sess.removeCall(id)
	s.broker.endCall(id, string(core.EndCallReasonUserEnded))
	w.WriteHeader(http.StatusNoContent)
}

func normalizePhone(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "+")
	var b strings.Builder
	for _, c := range p {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}
