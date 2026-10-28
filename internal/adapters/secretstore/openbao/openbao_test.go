package openbao

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/secrets"
)

func TestAppRoleLogin(t *testing.T) {
	var logins []map[string]string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/approle/login":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			logins = append(logins, body)
			_, _ = w.Write([]byte(`{"auth":{"client_token":"t","lease_duration":3600}}`))
		case "/v1/auth/token/lookup-self":
			_, _ = w.Write([]byte(`{"data":{"display_name":"approle","policies":["samuori"]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	c := New()
	for _, tc := range []struct {
		role, credential string
		withSecretID     bool
	}{{"bound-by-cidr", "", false}, {"with-secret", "s3cret", true}, {" pasted ", "s3cret\n", true}} {
		cfg := secrets.ExternalConfig{Address: srv.URL, AuthMethod: "approle", RoleID: tc.role, CACertPEM: ca}
		if _, err := c.Check(context.Background(), secrets.ProviderVault, cfg, []byte(tc.credential)); err != nil {
			t.Fatalf("%s: %v", tc.role, err)
		}
		got := logins[len(logins)-1]
		if _, has := got["secret_id"]; has != tc.withSecretID || got["role_id"] != strings.TrimSpace(tc.role) || got["secret_id"] != strings.TrimSpace(tc.credential) {
			t.Errorf("%s: login body = %v", tc.role, got)
		}
	}
}
