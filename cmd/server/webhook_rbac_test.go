package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// newWebhookRBACServer builds a real server with authentication enabled and
// two accounts: an admin (bootstrapped) and an operator (seeded). It returns
// the server plus raw session tokens for each principal.
func newWebhookRBACServer(t *testing.T) (*server, string, string) {
	t.Helper()
	ctx := context.Background()
	cancelCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)

	dbPath := filepath.Join(t.TempDir(), "webhook_rbac.db")
	s, err := newServer(cancelCtx, serverOptions{
		DBPath:     dbPath,
		AdminUser:  "admin",
		AdminPass:  "adminpass123",
		SessionTTL: time.Hour,
	}, testLogger())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() { s.audit.Close() })

	// Seed an operator account with a known id.
	if _, err := s.auth.insertUserIfAbsent(ctx, "rbac-op-user", "operator1", []byte("operatorpass"), "operator", time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed operator: %v", err)
	}

	// Resolve the bootstrapped admin's user id.
	adminID, _, _, err := s.auth.getUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("lookup admin: %v", err)
	}

	const adminToken = "rbac-admin-token"
	const operatorToken = "rbac-operator-token"
	now := time.Now()
	if err := s.auth.insertSession(ctx, newSessionID(), adminID, sha256Hex(adminToken), now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("seed admin session: %v", err)
	}
	if err := s.auth.insertSession(ctx, newSessionID(), "rbac-op-user", sha256Hex(operatorToken), now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("seed operator session: %v", err)
	}

	return s, adminToken, operatorToken
}

func webhookRBACRequest(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookRBACOperatorForbidden(t *testing.T) {
	s, _, operatorToken := newWebhookRBACServer(t)
	h := s.routes()

	// GET
	rec := webhookRBACRequest(t, h, http.MethodGet, "/api/webhooks", operatorToken, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator GET /api/webhooks: got %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	// POST
	rec = webhookRBACRequest(t, h, http.MethodPost, "/api/webhooks", operatorToken, map[string]any{
		"url": "https://example.com/hook", "secret": "s",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator POST /api/webhooks: got %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	// DELETE
	rec = webhookRBACRequest(t, h, http.MethodDelete, "/api/webhooks/anyid", operatorToken, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator DELETE /api/webhooks/{id}: got %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestWebhookRBACAdminAllowed(t *testing.T) {
	s, adminToken, _ := newWebhookRBACServer(t)
	h := s.routes()

	// GET
	rec := webhookRBACRequest(t, h, http.MethodGet, "/api/webhooks", adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /api/webhooks: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// POST
	rec = webhookRBACRequest(t, h, http.MethodPost, "/api/webhooks", adminToken, map[string]any{
		"url": "https://example.com/hook", "secret": "s",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin POST /api/webhooks: got %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("admin POST response missing id: %v body=%s", err, rec.Body.String())
	}

	// DELETE
	rec = webhookRBACRequest(t, h, http.MethodDelete, "/api/webhooks/"+created.ID, adminToken, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin DELETE /api/webhooks/{id}: got %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestWebhookRBACUnauthenticated(t *testing.T) {
	s, _, _ := newWebhookRBACServer(t)
	h := s.routes()

	// GET
	rec := webhookRBACRequest(t, h, http.MethodGet, "/api/webhooks", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth GET /api/webhooks: got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}

	// POST
	rec = webhookRBACRequest(t, h, http.MethodPost, "/api/webhooks", "", map[string]any{
		"url": "https://example.com/hook", "secret": "s",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth POST /api/webhooks: got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}

	// DELETE
	rec = webhookRBACRequest(t, h, http.MethodDelete, "/api/webhooks/anyid", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth DELETE /api/webhooks/{id}: got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}
