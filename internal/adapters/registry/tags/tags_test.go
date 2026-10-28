package tags

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestResolveTag runs against a registry that, like Docker Hub, answers anonymous pulls only with a bearer token
// from its realm, and serves the multi-platform index for a tag.
func TestResolveTag(t *testing.T) {
	index := "sha256:" + strings.Repeat("1", 64)
	var host string
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") != "repository:library/node:pull" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"token":"anonymous-pull"}`))
		case r.Header.Get("Authorization") != "Bearer anonymous-pull":
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+host+`/token",service="test",scope="repository:library/node:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/v2/library/node/manifests/18-alpine":
			methods = append(methods, r.Method)
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				http.Error(w, "index not accepted", http.StatusNotAcceptable)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", index)
			w.Header().Set("Content-Length", "512")
		case r.URL.Path == "/v2/library/node/manifests/slow":
			time.Sleep(2 * time.Second)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	host = strings.TrimPrefix(srv.URL, "http://")
	r := &Resolver{Timeout: 500 * time.Millisecond, AllowPrivate: true, PlainHTTP: true}
	ctx := context.Background()

	got, err := r.ResolveTag(ctx, host+"/library/node:18-alpine")
	if err != nil || got != index {
		t.Fatalf("resolved = %q, %v", got, err)
	}
	if len(methods) != 1 || methods[0] != http.MethodHead {
		t.Errorf("manifest requests = %v; want one HEAD", methods)
	}
	if _, err := r.ResolveTag(ctx, host+"/library/node:missing"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unknown tag = %v", err)
	}
	if _, err := r.ResolveTag(ctx, host+"/library/node:slow"); err == nil {
		t.Error("the timeout was not applied")
	}
	if _, err := (&Resolver{PlainHTTP: true}).ResolveTag(ctx, host+"/library/node:18-alpine"); err == nil {
		t.Error("a loopback registry was reached without AllowPrivate")
	}
	if _, err := r.ResolveTag(ctx, "node@sha256:"+strings.Repeat("2", 64)); err == nil {
		t.Error("a digest reference was resolved")
	}
	if Endpoint("docker.io") != "registry-1.docker.io" || Endpoint("ghcr.io") != "ghcr.io" {
		t.Error("Docker Hub's API host is wrong")
	}
}
