package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/discovery/sandbox"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/tenancy"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "child-discover" {
		if err := sandbox.ChildMain(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	auth.Argon2Params.Time, auth.Argon2Params.Memory, auth.Argon2Params.Threads = 1, 1024, 1
	os.Exit(m.Run())
}

type live struct {
	srv     *httptest.Server
	db      *db.DB
	dataDir string
	logs    *strings.Builder
}

func newLive(t *testing.T, secure bool) *live {
	return newLiveWith(t, secure, sources.DefaultConfig(), nil)
}

func newLiveWith(t *testing.T, secure bool, cfg sources.Config, tweak func(*Deps)) *live {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	// SAMUORI_TEST_POSTGRES points one test at an empty PostgreSQL database, for example -run TestUpgradeStatus.
	dsn := os.Getenv("SAMUORI_TEST_POSTGRES")
	if dsn == "" {
		dsn = dbtest.SQLite(t, dir)
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	logs := &strings.Builder{}
	store := tenancy.NewStore(d)
	deps := testDeps(t, d, store, slog.New(slog.NewTextHandler(logs, nil)), nil, dir, secure, cfg)
	if tweak != nil {
		tweak(&deps)
	}
	svc := deps.Auth
	handler, err := Handler(New(deps), svc)
	if err != nil {
		t.Fatal(err)
	}
	l := &live{srv: httptest.NewServer(handler), db: d, dataDir: dir, logs: logs}
	t.Cleanup(l.srv.Close)
	if _, err := svc.EnsureBootstrapToken(ctx, dir); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *live) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func (l *live) call(t *testing.T, c *http.Client, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, l.srv.URL+"/api/v1"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (l *live) token(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(l.dataDir, "bootstrap-token"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

func (l *live) setup(t *testing.T) {
	t.Helper()
	c := l.client()
	resp := l.call(t, c, "POST", "/setup/bootstrap", fmt.Sprintf(`{"token":%q,"organizationName":"acme","username":"admin","password":"correct horse battery"}`, l.token(t)), nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("bootstrap = %d %s", resp.StatusCode, b)
	}
}

func (l *live) login(t *testing.T) (*http.Client, string) {
	t.Helper()
	c := l.client()
	resp := l.call(t, c, "POST", "/auth/login", `{"username":"admin","password":"correct horse battery"}`, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == auth.CSRFCookie {
			return c, ck.Value
		}
	}
	t.Fatal("no CSRF cookie")
	return nil, ""
}

func TestBootstrapFlow(t *testing.T) {
	t.Run("SEC-BOOT-01", func(t *testing.T) {
		l := newLive(t, false)
		c := l.client()
		var st apigen.SetupStatus
		_ = json.NewDecoder(l.call(t, c, "GET", "/setup/status", "", nil).Body).Decode(&st)
		if st.Completed {
			t.Fatal("setup reported complete before bootstrap")
		}
		token := l.token(t)
		if r := l.call(t, c, "POST", "/setup/bootstrap", `{"token":"guess","organizationName":"acme","username":"admin","password":"correct horse battery"}`, nil); r.StatusCode != 403 {
			t.Fatalf("wrong token = %d", r.StatusCode)
		}
		l.setup(t)
		if r := l.call(t, c, "POST", "/setup/bootstrap", fmt.Sprintf(`{"token":%q,"organizationName":"x","username":"evil","password":"correct horse battery"}`, token), nil); r.StatusCode != 409 {
			t.Fatalf("second bootstrap = %d", r.StatusCode)
		}
		if strings.Contains(l.logs.String(), token) {
			t.Fatal("bootstrap token appeared in logs")
		}
	})
}

func TestSessionCookiesAndFixation(t *testing.T) {
	t.Run("SEC-AUTH-01", func(t *testing.T) {
		l := newLive(t, true)
		l.setup(t)
		c := l.client()
		req, _ := http.NewRequest("POST", l.srv.URL+"/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"correct horse battery"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "attacker-chosen"})
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		var session *http.Cookie
		for _, ck := range resp.Cookies() {
			if ck.Name == auth.SessionCookie {
				session = ck
			}
		}
		if session == nil || session.Value == "attacker-chosen" || !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteStrictMode {
			t.Fatalf("session cookie = %+v", session)
		}
		probe, _ := http.NewRequest("GET", l.srv.URL+"/api/v1/me", nil)
		probe.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "attacker-chosen"})
		if r, _ := http.DefaultClient.Do(probe); r.StatusCode != 401 {
			t.Fatalf("planted session accepted: %d", r.StatusCode)
		}
	})
}

func TestCSRF(t *testing.T) {
	t.Run("SEC-AUTH-02", func(t *testing.T) {
		l := newLive(t, false)
		l.setup(t)
		c, csrf := l.login(t)
		if r := l.call(t, c, "GET", "/me", "", nil); r.StatusCode != 200 {
			t.Fatalf("GET with session = %d", r.StatusCode)
		}
		body := `{"name":"ci","expiresAt":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `","scopes":["read"]}`
		if r := l.call(t, c, "POST", "/me/api-tokens", body, nil); r.StatusCode != 403 {
			t.Fatalf("POST without CSRF = %d", r.StatusCode)
		}
		if r := l.call(t, c, "POST", "/me/api-tokens", body, map[string]string{auth.CSRFHeader: "wrong"}); r.StatusCode != 403 {
			t.Fatalf("POST with wrong CSRF = %d", r.StatusCode)
		}
		if r := l.call(t, c, "POST", "/me/api-tokens", body, map[string]string{auth.CSRFHeader: csrf}); r.StatusCode != 201 {
			t.Fatalf("POST with CSRF = %d", r.StatusCode)
		}
		pre := l.call(t, &http.Client{}, "OPTIONS", "/me/api-tokens", "", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
		if pre.Header.Get("Access-Control-Allow-Origin") != "" || pre.Header.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal("cross-origin preflight granted")
		}
	})
}

func TestLoginThrottling(t *testing.T) {
	t.Run("SEC-AUTH-03", func(t *testing.T) {
		l := newLive(t, false)
		l.setup(t)
		c := l.client()
		for i := range 5 {
			if r := l.call(t, c, "POST", "/auth/login", `{"username":"admin","password":"wrong password!!"}`, nil); r.StatusCode != 401 {
				t.Fatalf("attempt %d = %d", i, r.StatusCode)
			}
		}
		if r := l.call(t, c, "POST", "/auth/login", `{"username":"admin","password":"correct horse battery"}`, nil); r.StatusCode != 429 {
			t.Fatalf("locked account login = %d", r.StatusCode)
		}
		var denied int
		_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'auth.login' AND outcome = 'denied'`).Scan(&denied)
		if denied < 6 {
			t.Fatalf("denied logins audited = %d", denied)
		}
		var hash string
		_ = l.db.SQL.QueryRow(`SELECT password_hash FROM user_credentials`).Scan(&hash)
		if !strings.HasPrefix(hash, "$argon2id$") {
			t.Fatalf("stored hash %q", hash)
		}
		blocked := false
		for range 25 {
			if r := l.call(t, c, "POST", "/auth/login", `{"username":"nobody","password":"wrong password!!"}`, nil); r.StatusCode == 429 {
				blocked = true
				break
			}
		}
		if !blocked {
			t.Fatal("per-IP rate limit never triggered")
		}
	})
}

func TestAPITokens(t *testing.T) {
	t.Run("SEC-AUTH-04", func(t *testing.T) {
		l := newLive(t, false)
		l.setup(t)
		c, csrf := l.login(t)
		h := map[string]string{auth.CSRFHeader: csrf}
		exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		var ro, rw apigen.ApiTokenIssued
		_ = json.NewDecoder(l.call(t, c, "POST", "/me/api-tokens", `{"name":"ro","expiresAt":"`+exp+`","scopes":["read"]}`, h).Body).Decode(&ro)
		_ = json.NewDecoder(l.call(t, c, "POST", "/me/api-tokens", `{"name":"rw","expiresAt":"`+exp+`","scopes":["read","write"]}`, h).Body).Decode(&rw)
		if ro.Token == "" || rw.Token == "" {
			t.Fatal("tokens not returned at creation")
		}
		list, _ := io.ReadAll(l.call(t, c, "GET", "/me/api-tokens", "", nil).Body)
		if strings.Contains(string(list), ro.Token) || strings.Contains(string(list), `"token"`) {
			t.Fatalf("token value listed: %s", list)
		}
		anon := &http.Client{}
		bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
		if r := l.call(t, anon, "GET", "/me", "", bearer(ro.Token)); r.StatusCode != 200 {
			t.Fatalf("read token GET = %d", r.StatusCode)
		}
		if r := l.call(t, anon, "POST", "/me/api-tokens", `{"name":"x","expiresAt":"`+exp+`","scopes":["read"]}`, bearer(ro.Token)); r.StatusCode != 403 {
			t.Fatalf("read-only token POST = %d", r.StatusCode)
		}
		if r := l.call(t, anon, "POST", "/me/api-tokens", `{"name":"x","expiresAt":"`+exp+`","scopes":["read"]}`, bearer(rw.Token)); r.StatusCode != 201 {
			t.Fatalf("write token POST without CSRF = %d", r.StatusCode)
		}
		if r := l.call(t, c, "POST", "/me/api-tokens/"+rw.Id+"/revoke", "", h); r.StatusCode != 204 {
			t.Fatalf("revoke = %d", r.StatusCode)
		}
		if r := l.call(t, anon, "GET", "/me", "", bearer(rw.Token)); r.StatusCode != 401 {
			t.Fatalf("revoked token = %d", r.StatusCode)
		}
		if r := l.call(t, anon, "GET", "/me", "", bearer("samuori_forged")); r.StatusCode != 401 {
			t.Fatalf("forged token = %d", r.StatusCode)
		}
	})
}
