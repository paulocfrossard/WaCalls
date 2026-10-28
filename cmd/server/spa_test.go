package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const spaTestIndex = "<!doctype html><html><body>WaCalls SPA shell</body></html>"
const spaTestAsset = "console.log('wacalls');"
const spaTestSecret = "TOP_SECRET_SHOULD_NEVER_LEAK"

func spaLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeSPAFixture creates a static dir with index.html and assets/app.js and
// returns its path.
func writeSPAFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(spaTestIndex), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte(spaTestAsset), 0o644); err != nil {
		t.Fatalf("write app.js: %v", err)
	}
	return dir
}

func spaGet(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestSpaHistoryFallback(t *testing.T) {
	dir := writeSPAFixture(t)
	s := &server{log: spaLogger(), staticDir: dir}
	h := s.routes()

	rec := spaGet(h, "/login")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != spaTestIndex {
		t.Fatalf("GET /login body = %q, want the SPA index", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET /login content type = %q, want text/html", ct)
	}

	rec = spaGet(h, "/assets/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js: got %d, want 200", rec.Code)
	}
	if rec.Body.String() != spaTestAsset {
		t.Fatalf("asset body = %q, want %q", rec.Body.String(), spaTestAsset)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("asset content type = %q, want a javascript type", ct)
	}
}

func TestSpaAPINotFound(t *testing.T) {
	dir := writeSPAFixture(t)
	s := &server{log: spaLogger(), staticDir: dir}
	h := s.routes()

	for _, target := range []string{"/api/nope", "/api"} {
		rec := spaGet(h, target)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s: got %d, want 404 (body=%s)", target, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("GET %s content type = %q, want application/json", target, ct)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s body %q is not JSON: %v", target, rec.Body.String(), err)
		}
		if body["error"] != "not found" {
			t.Fatalf("GET %s body = %v, want error=not found", target, body)
		}
	}
}

func TestSpaPathTraversal(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "static")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir static: %v", err)
	}
	// A file directly above the static root: escaping it would leak this.
	if err := os.WriteFile(filepath.Join(base, "secret.txt"), []byte(spaTestSecret), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// Exercise the filesystem boundary directly so the HTTP router cannot
	// rewrite the path first. No index.html exists, so any path that is not a
	// real file inside the root must be a 404.
	s := &server{log: spaLogger(), staticDir: dir}
	handler := s.spaHandler(dir)
	for _, target := range []string{
		"/../secret.txt",
		"/../etc/passwd",
		"/%2e%2e%2fetc/passwd",
		"/%2e%2e%2fsecret.txt",
		"/..%2f..%2fetc%2fpasswd",
	} {
		rec := spaGet(handler, target)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s: got %d, want 404 (body=%q)", target, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), spaTestSecret) || strings.Contains(rec.Body.String(), "root:") {
			t.Fatalf("GET %s leaked file content: %q", target, rec.Body.String())
		}
	}

	// Also drive the full stack: ServeMux canonicalises a raw ".." segment to a
	// 301 before the handler runs, while the percent-encoded form reaches the
	// handler (404). In no case may file content come back.
	full := &server{log: spaLogger(), staticDir: dir}
	h := full.routes()
	for _, target := range []string{"/../etc/passwd", "/%2e%2e%2fetc/passwd", "/../secret.txt"} {
		rec := spaGet(h, target)
		if strings.Contains(rec.Body.String(), spaTestSecret) || strings.Contains(rec.Body.String(), "root:") {
			t.Fatalf("GET %s leaked file content: %q", target, rec.Body.String())
		}
		switch rec.Code {
		case http.StatusNotFound:
		case http.StatusMovedPermanently, http.StatusTemporaryRedirect:
			if strings.Contains(rec.Header().Get("Location"), "..") {
				t.Fatalf("GET %s redirect keeps traversal segment: %q", target, rec.Header().Get("Location"))
			}
		default:
			t.Fatalf("GET %s: got %d, want 404 or a canonical redirect", target, rec.Code)
		}
	}
}
