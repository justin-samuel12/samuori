package secrets

import (
	"context"
	"errors"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

type fixture struct {
	db                              *db.DB
	s                               *Service
	admin, dev, outsider, publisher authz.Principal
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.SQLite(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, q := range []string{
		`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`,
		`INSERT INTO teams (id, org_id, name, created_at) VALUES ('t1', 'o1', 'payments', ?)`,
		`INSERT INTO teams (id, org_id, name, created_at) VALUES ('t2', 'o1', 'other', ?)`,
		`INSERT INTO projects (id, org_id, team_id, name, publication_mode, created_at, updated_at) VALUES ('p1', 'o1', 't1', 'orders', 'manual', ?, ?)`,
		`INSERT INTO projects (id, org_id, team_id, name, publication_mode, created_at, updated_at) VALUES ('p2', 'o1', 't2', 'billing', 'manual', ?, ?)`,
	} {
		args := []any{now}
		if q[12:20] == "projects" {
			args = append(args, now)
		}
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(q), args...); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewService(d, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	admin := authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	dev := authz.Principal{ID: "dev", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Developer, ScopeType: authz.ScopeTeam, ScopeID: "t1"}}}
	outsider := authz.Principal{ID: "out", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Developer, ScopeType: authz.ScopeTeam, ScopeID: "t2"}}}
	publisher := authz.Principal{ID: "pub", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Publisher, ScopeType: authz.ScopeTeam, ScopeID: "t1"}}}

	return fixture{d, s, admin, dev, outsider, publisher}
}

func TestSecretGrants(t *testing.T) {
	t.Run("SEC-SCR-08", func(t *testing.T) {
		ctx := context.Background()
		f := newFixture(t)
		s, admin, dev, outsider, publisher := f.s, f.admin, f.dev, f.outsider, f.publisher
		local, err := s.LocalProvider(ctx, "o1")
		if err != nil {
			t.Fatal(err)
		}
		sec, err := s.CreateSecret(ctx, admin, "npm-token", "text", "", []byte("v1"), "")
		if err != nil {
			t.Fatal(err)
		}
		req := GrantRequest{ProjectID: "p1", ProviderID: local.ID, ProviderReference: sec.ID, Usage: "build", Reason: "private packages"}
		if _, err := s.CreateGrant(ctx, dev, req, ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("a developer granted a key: %v", err)
		}
		g, err := s.CreateGrant(ctx, admin, req, "")
		if err != nil {
			t.Fatal(err)
		}
		other, err := s.CreateGrant(ctx, admin, GrantRequest{ProjectID: "p2", ProviderID: local.ID, ProviderReference: sec.ID, Usage: "build", Reason: "billing"}, "")
		if err != nil {
			t.Fatal(err)
		}

		if _, err := s.BindGrant(ctx, outsider, "p1", "npm_token", g.ID, "", "", ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("a developer of another team bound a key of this project: %v", err)
		}
		if _, err := s.BindGrant(ctx, publisher, "p1", "npm_token", g.ID, "", "", ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("a publisher bound a key: %v", err)
		}
		if _, err := s.BindGrant(ctx, dev, "p1", "npm_token", other.ID, "", "", ""); err == nil {
			t.Error("a developer bound a key granted to another project")
		}
		if _, err := s.Bind(ctx, dev, BindRequest{ProjectID: "p1", LogicalID: "npm_token", ProviderID: local.ID, ProviderReference: sec.ID, Reason: "direct"}, ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("a developer bound a secret without a grant: %v", err)
		}
		b, err := s.BindGrant(ctx, dev, "p1", "npm_token", g.ID, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if b.ProviderReference != sec.ID || b.GrantID != g.ID || b.Status != Active {
			t.Fatalf("binding = %+v", b)
		}
		if _, err := s.Value(ctx, "o1", b.ID, b.Version, "build"); err != nil {
			t.Fatalf("the granted binding cannot be released: %v", err)
		}

		if _, _, err := s.ListGrants(ctx, outsider, "p1"); !errors.Is(err, ErrForbidden) {
			t.Errorf("a developer of another team listed this project's grants: %v", err)
		}
		grants, uses, err := s.ListGrants(ctx, dev, "p1")
		if err != nil || len(grants) != 1 || len(uses) != 1 || uses[0].GrantID != g.ID {
			t.Fatalf("grants = %+v, uses = %+v, %v", grants, uses, err)
		}

		if err := s.RevokeGrant(ctx, dev, "p1", g.ID, "no longer needed", ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("a developer revoked a grant: %v", err)
		}
		if err := s.RevokeGrant(ctx, admin, "p1", g.ID, "no longer needed", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Value(ctx, "o1", b.ID, b.Version, "build"); err == nil {
			t.Error("a binding made from a revoked grant was still released")
		}
		if _, err := s.BindGrant(ctx, dev, "p1", "npm_token", g.ID, "", "", ""); err == nil {
			t.Error("a revoked grant was bound")
		}
	})
}
