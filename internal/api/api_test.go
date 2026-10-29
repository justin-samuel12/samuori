package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/apiclient"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/health"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/tenancy"
)

type headerAuth map[string]authz.Principal

func (h headerAuth) Authenticate(r *http.Request) (authz.Principal, bool, error) {
	p, ok := h[r.Header.Get("X-Test-Principal")]
	return p, ok, nil
}

type env struct {
	srv   *httptest.Server
	db    *db.DB
	store *tenancy.Store
	auth  headerAuth
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	h := health.NewRegistry(time.Second)
	h.Register(health.Check{Name: "database", Critical: true, Run: d.Ping})
	e := &env{db: d, store: tenancy.NewStore(d), auth: headerAuth{}}
	handler, err := Handler(New(testDeps(t, d, e.store, slog.New(slog.NewTextHandler(io.Discard, nil)), h, t.TempDir(), false, sources.DefaultConfig())), e.auth)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(handler)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) do(t *testing.T, as, method, path, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+"/api/v1"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if as != "" {
		req.Header.Set("X-Test-Principal", as)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

type opCase struct {
	method, path string
	op           *openapi3.Operation
	headers      map[string]string
}

func allOperations(t *testing.T) []opCase {
	t.Helper()
	spec, err := apigen.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	var out []opCase
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			h := map[string]string{}
			for _, p := range append(item.Parameters, op.Parameters...) {
				if p.Value != nil && p.Value.In == "header" && p.Value.Required {
					h[p.Value.Name] = "0123456789abcdef"
				}
			}
			out = append(out, opCase{method, fillPath(path), op, h})
		}
	}
	if len(out) != 312 {
		t.Fatalf("operations = %d, want 312", len(out))
	}
	return out
}

func fillPath(p string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(p, '{')
		if i < 0 {
			b.WriteString(p)
			return b.String()
		}
		j := strings.IndexByte(p, '}')
		b.WriteString(p[:i])
		switch p[i+1 : j] {
		case "revision", "index":
			b.WriteString("1")
		default:
			b.WriteString("01JAB3K0000000000000000001")
		}
		p = p[j+1:]
	}
}

func TestAnonymousCallersRejected(t *testing.T) {
	e := newEnv(t)
	for _, c := range allOperations(t) {
		code, _ := e.do(t, "", c.method, c.path, "{}", c.headers)
		public := c.op.Extensions["x-samuori-public"] == true
		signed := c.op.Extensions["x-samuori-signed"] == true
		if !public && code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", c.method, c.path, code)
		}
		if public && signed && code != http.StatusUnauthorized {
			t.Errorf("signed %s %s = %d without a signature, want 401", c.method, c.path, code)
		}
		if public && !signed && code == http.StatusUnauthorized {
			t.Errorf("public %s %s = 401", c.method, c.path)
		}
	}
}

func TestRoleRestrictedOperations(t *testing.T) {
	t.Run("SEC-RBAC-08", func(t *testing.T) {
		e := newEnv(t)
		e.auth["dev"] = authz.Principal{ID: "u-dev", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Developer, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
		checked := 0
		for _, c := range allOperations(t) {
			roles, any := declaredRoles(c.op)
			if any || c.op.Extensions["x-samuori-public"] == true || authz.HasAnyRole(e.auth["dev"], roles) {
				continue
			}
			checked++
			if code, _ := e.do(t, "dev", c.method, c.path, "{}", c.headers); code != http.StatusForbidden {
				t.Errorf("developer %s %s (%v) = %d, want 403", c.method, c.path, roles, code)
			}
		}
		if checked < 30 {
			t.Fatalf("only %d role-restricted operations checked", checked)
		}
	})
}

func TestEveryOperationIsRouted(t *testing.T) {
	e := newEnv(t)
	var all []authz.Binding
	for _, r := range []authz.Role{authz.Developer, authz.Maintainer, authz.Publisher, authz.Operator, authz.SecurityReviewer, authz.Administrator} {
		all = append(all, authz.Binding{Role: r, ScopeType: authz.ScopeOrganization, ScopeID: "o1"})
	}
	e.auth["root"] = authz.Principal{ID: "u-root", OrgID: "o1", Kind: authz.User, Bindings: all}
	for _, c := range allOperations(t) {
		code, body := e.do(t, "root", c.method, c.path, "{}", c.headers)
		if c.op.Extensions["x-samuori-signed"] == true && code == http.StatusUnauthorized {
			continue
		}
		switch code {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusMethodNotAllowed:
			t.Errorf("%s %s = %d: %s", c.method, c.path, code, body)
		}
	}
}

func TestHealthViaGeneratedClientMatchesSchema(t *testing.T) {
	e := newEnv(t)
	c, err := apiclient.NewClientWithResponses(e.srv.URL + "/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.GetHealthWithResponse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON200 == nil || resp.JSON200.Status != apiclient.HealthStatusOk {
		t.Fatalf("health = %d %s", resp.StatusCode(), resp.Body)
	}
	assertMatchesContract(t, http.MethodGet, "/system/health", resp.StatusCode(), resp.HTTPResponse.Header, resp.Body)
}

func assertMatchesContract(t *testing.T, method, path string, status int, header http.Header, body []byte) {
	t.Helper()
	spec, _ := apigen.GetSwagger()
	spec.Servers = nil
	router, err := legacy.NewRouter(spec)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, nil)
	route, params, err := router.FindRoute(req)
	if err != nil {
		t.Fatal(err)
	}
	err = openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 status, Header: header, Body: io.NopCloser(strings.NewReader(string(body))),
	})
	if err != nil {
		t.Fatalf("%s %s response violates the contract: %v\n%s", method, path, err, body)
	}
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}
