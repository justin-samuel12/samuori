package webrun

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandler: files and client-side routes come from the frontend folder, nothing outside it is served, and API
// paths reach the backend.
func TestHandler(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	for name, body := range map[string]string{"index.html": "<app>", "assets/app.js": "js"} {
		p := filepath.Join(root, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644)
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "api:"+r.URL.Path)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	h := Handler(Config{Root: root, Prefixes: []string{"/api"}, Backend: target})
	get := func(method, p string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "http://x"+p, nil)
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, c := range []struct {
		method, path string
		code         int
		body         string
	}{
		{"GET", "/", 200, "<app>"},
		{"GET", "/assets/app.js", 200, "js"},
		{"GET", "/orders/42", 200, "<app>"},
		{"GET", "/missing.png", 404, ""},
		{"GET", "/assets/", 200, "<app>"},
		{"GET", "/../" + filepath.Base(outside) + "/secret.txt", 404, ""},
		{"GET", "/leak.txt", 404, ""},
		{"GET", "/api/health", 200, "api:/api/health"},
		{"GET", "/api", 200, "api:/api"},
		{"GET", "/apiary", 200, "<app>"},
		{"POST", "/", 405, ""},
		{"POST", "/api/orders", 200, "api:/api/orders"},
	} {
		code, body := get(c.method, c.path)
		if code != c.code || (c.body != "" && body != c.body) || strings.Contains(body, "secret") {
			t.Errorf("%s %s = %d %q, want %d %q", c.method, c.path, code, body, c.code, c.body)
		}
	}
}

// TestRunStopsWithTheBackend: the launcher exits with the backend's status when the backend exits.
func TestRunStopsWithTheBackend(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1")
	var out bytes.Buffer
	code := Run(context.Background(), Config{Listen: "127.0.0.1:0", Root: t.TempDir(), Prefixes: []string{"/api"}, Backend: target, Command: []string{"sh", "-c", "exit 3"}}, &out, &out)
	if code != 3 || !strings.Contains(out.String(), "the backend exited (3)") {
		t.Errorf("code = %d, output %q", code, out.String())
	}
}
