package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"samuori.invalid/samuori/api/openapi"
	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/authz"
)

func TestContractIsServedToSignedInCallers(t *testing.T) {
	e := newEnv(t)
	e.auth["nobody"] = authz.Principal{ID: "u-1", OrgID: "o1", Kind: authz.User}
	e.auth["robot"] = authz.Principal{ID: "s-1", OrgID: "o1", Kind: authz.Service, ReadOnly: true}

	for _, path := range []string{"/openapi.yml", "/openapi.json"} {
		if code, _ := e.do(t, "", http.MethodGet, path, "", nil); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d, want 401", path, code)
		}
	}

	get := func(as, path string, h map[string]string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1"+path, nil)
		req.Header.Set("X-Test-Principal", as)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	yml := get("nobody", "/openapi.yml", nil)
	var body bytes.Buffer
	_, _ = body.ReadFrom(yml.Body)
	if yml.StatusCode != http.StatusOK || yml.Header.Get("Content-Type") != "application/yaml; charset=utf-8" || !bytes.Equal(body.Bytes(), openapi.Document) {
		t.Fatalf("yml = %d %q, %d bytes", yml.StatusCode, yml.Header.Get("Content-Type"), body.Len())
	}
	if yml.Header.Get("Cache-Control") != "private, max-age=300" || yml.Header.Get("ETag") == "" {
		t.Fatalf("yml cache headers = %q %q", yml.Header.Get("Cache-Control"), yml.Header.Get("ETag"))
	}
	if again := get("nobody", "/openapi.yml", map[string]string{"If-None-Match": yml.Header.Get("ETag")}); again.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304", again.StatusCode)
	}

	js := get("robot", "/openapi.json", nil)
	var doc struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err := json.NewDecoder(js.Body).Decode(&doc); err != nil || js.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("json = %d %q: %v", js.StatusCode, js.Header.Get("Content-Type"), err)
	}
	if doc.OpenAPI != "3.0.3" || !bytes.Contains(doc.Paths["/openapi.json"], []byte(`"operationId":"getApiContractJson"`)) || js.Header.Get("ETag") == yml.Header.Get("ETag") {
		t.Fatalf("json contract = %q, %d paths, etag %q", doc.OpenAPI, len(doc.Paths), js.Header.Get("ETag"))
	}
}

func TestDeprecationHeadersOnlyOnMarkedOperations(t *testing.T) {
	spec, err := apigen.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	me := spec.Paths.Find("/me").Get
	me.Deprecated = true
	me.Extensions["x-samuori-deprecated"] = "2026-10-28"
	me.Extensions["x-samuori-sunset"] = "2027-01-31"
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	p := authz.Principal{ID: "u-1", OrgID: "o1", Kind: authz.User}
	h, err := guardSpec(spec, ok, headerAuth{"u": p}, newLimiter(RateLimit{}))
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string) http.Header {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
		r.Header.Set("X-Test-Principal", "u")
		h.ServeHTTP(w, r)
		return w.Header()
	}
	got := call("/me")
	if got.Get("Deprecation") != "@1793145600" || got.Get("Sunset") != "Sun, 31 Jan 2027 00:00:00 GMT" {
		t.Fatalf("deprecated operation headers = %q %q", got.Get("Deprecation"), got.Get("Sunset"))
	}
	if got := call("/teams"); got.Get("Deprecation") != "" || got.Get("Sunset") != "" {
		t.Fatalf("unmarked operation carries %q %q", got.Get("Deprecation"), got.Get("Sunset"))
	}
}

func TestDeprecationDatesAreChecked(t *testing.T) {
	op := func(deprecated bool, ext map[string]any) *openapi3.Operation {
		return &openapi3.Operation{Deprecated: deprecated, Extensions: ext}
	}
	for name, o := range map[string]*openapi3.Operation{
		"no date":            op(true, map[string]any{}),
		"bad date":           op(true, map[string]any{"x-samuori-deprecated": "28/10/2026"}),
		"short notice":       op(true, map[string]any{"x-samuori-deprecated": "2026-10-28", "x-samuori-sunset": "2026-11-15"}),
		"dates, not flagged": op(false, map[string]any{"x-samuori-deprecated": "2026-10-28"}),
	} {
		if _, _, err := operationDeprecation(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	spec, _ := apigen.GetSwagger()
	if _, err := deprecations(spec); err != nil {
		t.Fatalf("published contract: %v", err)
	}
}
