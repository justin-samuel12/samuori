package imagesources

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/secrets"
)

type fixture struct {
	ctx                context.Context
	d                  *db.DB
	st                 *Store
	org, teamA, teamB  string
	projectA, projectB string
	admin, dev         authz.Principal
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(dir, "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.NewService(d, filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{ctx: ctx, d: d, st: NewStore(d, sec), org: ids.New(), teamA: ids.New(), teamB: ids.New(), projectA: ids.New(), projectB: ids.New()}
	now := time.Now().UTC()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id, name, created_at) VALUES (?, 'o', ?)`, []any{f.org, now}},
		{`INSERT INTO teams (id, org_id, name, created_at) VALUES (?, ?, 'payments', ?), (?, ?, 'billing', ?)`, []any{f.teamA, f.org, now, f.teamB, f.org, now}},
		{`INSERT INTO projects (id, org_id, team_id, name, publication_mode, created_at, updated_at) VALUES (?, ?, ?, 'a', 'manual', ?, ?), (?, ?, ?, 'b', 'manual', ?, ?)`, []any{f.projectA, f.org, f.teamA, now, now, f.projectB, f.org, f.teamB, now, now}},
	} {
		if _, err := d.SQL.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	f.admin = authz.Principal{ID: ids.New(), OrgID: f.org, Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: f.org}}}
	f.dev = authz.Principal{ID: ids.New(), OrgID: f.org, Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Developer, ScopeType: authz.ScopeTeam, ScopeID: f.teamA}}}
	return f
}

func TestSources(t *testing.T) {
	f := setup(t)
	if _, err := f.st.Create(f.ctx, f.dev, Source{Name: "x"}, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("developer created a source: %v", err)
	}
	base := Source{Name: "corp", Upstream: "registry.corp.example", Mirror: "mirror-a.samuori.internal:5000", Patterns: []string{"payments/*"}, TeamIDs: []string{f.teamA}}
	for name, mod := range map[string]func(*Source){
		"bare *":                 func(s *Source) { s.Patterns = []string{"*"} },
		"traversal":              func(s *Source) { s.Patterns = []string{"../x"} },
		"same host":              func(s *Source) { s.Mirror = s.Upstream },
		"two teams, sharing off": func(s *Source) { s.TeamIDs = []string{f.teamA, f.teamB} },
		"unknown team":           func(s *Source) { s.TeamIDs = []string{ids.New()} },
	} {
		s := base
		mod(&s)
		if _, err := f.st.Create(f.ctx, f.admin, s, ""); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	a, err := f.st.Create(f.ctx, f.admin, base, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("OD-14 a mirror serves one team while sharing is off", func(t *testing.T) {
		other := base
		other.Name, other.TeamIDs, other.Patterns = "corp-billing", []string{f.teamB}, []string{"billing/*"}
		if _, err := f.st.Create(f.ctx, f.admin, other, ""); err == nil {
			t.Fatal("a second team was added to the same mirror")
		}
		other.Mirror = "mirror-b.samuori.internal:5000"
		if _, err := f.st.Create(f.ctx, f.admin, other, ""); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("developers see only their teams' sources", func(t *testing.T) {
		list, _ := f.st.List(f.ctx, f.dev)
		if len(list) != 1 || list[0].ID != a.ID {
			t.Fatalf("developer sees %+v", list)
		}
	})
	t.Run("admission", func(t *testing.T) {
		for _, c := range []struct {
			project, image   string
			private, allowed bool
		}{
			{f.projectA, "registry.corp.example/payments/base@sha256:" + strings.Repeat("a", 64), true, true},
			{f.projectA, "registry.corp.example/billing/base:1", true, false},
			{f.projectB, "registry.corp.example/payments/base:1", true, false},
			{f.projectA, "docker.io/library/node:22", false, false},
		} {
			private, allowed, err := f.st.Admit(f.ctx, f.org, c.project, c.image)
			if err != nil || private != c.private || allowed != c.allowed {
				t.Errorf("%s for %s = %t %t %v", c.image, c.project, private, allowed, err)
			}
		}
	})
}

// verify checks a token's ES256 signature against the certificate in its x5c header and returns its claims.
func verify(t *testing.T, token, trusted string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	hraw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var h struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	_ = json.Unmarshal(hraw, &h)
	der, _ := base64.StdEncoding.DecodeString(h.X5C[0])
	cert, err := x509.ParseCertificate(der)
	if err != nil || h.Alg != "ES256" {
		t.Fatalf("header %s: %v", hraw, err)
	}
	b, _ := pem.Decode([]byte(trusted))
	if b == nil || string(b.Bytes) != string(der) {
		t.Fatal("the token is not signed by the trusted certificate")
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(cert.PublicKey.(*ecdsa.PublicKey), sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the signature does not verify")
	}
	craw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(craw, &claims)
	return claims
}

func TestIssue(t *testing.T) {
	f := setup(t)
	if _, err := f.st.Create(f.ctx, f.admin, Source{Name: "corp", Upstream: "registry.corp.example", Mirror: "mirror-a:5000", Patterns: []string{"payments/*"}, TeamIDs: []string{f.teamA}}, ""); err != nil {
		t.Fatal(err)
	}
	req := Request{OrgID: f.org, TeamID: f.teamA, Subject: "attempt-1", Mirror: "mirror-a:5000"}
	if _, err := f.st.Issue(f.ctx, func() Request { r := req; r.Scopes = []string{"repository:payments/base:pull"}; return r }()); err == nil {
		t.Fatal("a token was signed without a key")
	}
	cert, err := f.st.RotateKey(f.ctx, f.admin, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("SEC-REG-04 grants only the team's repositories", func(t *testing.T) {
		r := req
		r.Scopes = []string{"repository:payments/base:pull", "repository:billing/base:pull", "repository:payments/base:push", "registry:catalog:*", "repository:payments/base:pull,push"}
		tok, err := f.st.Issue(f.ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		claims := verify(t, tok.Value, cert)
		access, _ := json.Marshal(claims["access"])
		if string(access) != `[{"actions":["pull"],"name":"payments/base","type":"repository"}]` || len(tok.Denied) != 4 {
			t.Fatalf("access %s, denied %v", access, tok.Denied)
		}
		if claims["aud"] != Service || claims["iss"] != Issuer || claims["sub"] != "attempt-1" {
			t.Fatalf("claims = %v", claims)
		}
		exp := time.Unix(int64(claims["exp"].(float64)), 0)
		if time.Until(exp) > TokenTTL+time.Second {
			t.Fatalf("token lives until %s", exp)
		}
	})
	t.Run("SEC-REG-07 another team, another mirror, a disabled source", func(t *testing.T) {
		for name, r := range map[string]Request{
			"other team":   {OrgID: f.org, TeamID: f.teamB, Mirror: "mirror-a:5000", Scopes: []string{"repository:payments/base:pull"}},
			"other mirror": {OrgID: f.org, TeamID: f.teamA, Mirror: "mirror-b:5000", Scopes: []string{"repository:payments/base:pull"}},
		} {
			tok, err := f.st.Issue(f.ctx, r)
			if err != nil || len(tok.Granted) != 0 {
				t.Errorf("%s: granted %v, %v", name, tok.Granted, err)
			}
		}
		list, _ := f.st.List(f.ctx, f.admin)
		if err := f.st.SetStatus(f.ctx, f.admin, list[0].ID, false, ""); err != nil {
			t.Fatal(err)
		}
		r := req
		r.Scopes = []string{"repository:payments/base:pull"}
		if tok, _ := f.st.Issue(f.ctx, r); len(tok.Granted) != 0 {
			t.Fatal("a disabled source still grants")
		}
	})
	t.Run("rotation keeps the old certificate in the bundle for a day", func(t *testing.T) {
		if _, err := f.st.RotateKey(f.ctx, f.admin, ""); err != nil {
			t.Fatal(err)
		}
		bundle, _ := f.st.TrustBundle(f.ctx, f.org)
		if strings.Count(bundle, "BEGIN CERTIFICATE") != 2 || !strings.Contains(bundle, strings.TrimSpace(cert)) {
			t.Fatalf("bundle has %d certificates", strings.Count(bundle, "BEGIN CERTIFICATE"))
		}
	})
}
