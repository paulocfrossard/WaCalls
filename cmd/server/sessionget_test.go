package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionGet(t *testing.T) {
	m := newTestManager(t)
	sess := m.addUnconnected(t, "Account A")
	h := (&server{sessions: m}).routes()

	t.Run("existing session returns the session object", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.id, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q, want application/json", ct)
		}
		var got SessionInfo
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("body is not JSON: %v (body=%q)", err, rec.Body.String())
		}
		if got.ID != sess.id || got.Name != "Account A" || got.Paired {
			t.Fatalf("unexpected session: %+v", got)
		}
	})

	t.Run("unknown session returns a JSON 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/does-not-exist", nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q, want application/json (body=%q)", ct, rec.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("404 body is not JSON: %v (body=%q)", err, rec.Body.String())
		}
		if body["error"] != "no such session" {
			t.Fatalf("error = %q, want no such session", body["error"])
		}
	})
}
