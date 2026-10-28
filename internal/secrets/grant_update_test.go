package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestUpdateGrant: widening keeps every binding active at its version; narrowing disables only the bindings the new
// scope no longer covers, audited; a version change gives kept bindings a new version; only administrators edit.
func TestUpdateGrant(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	s, admin, dev := f.s, f.admin, f.dev
	for _, p := range []string{"pool-a", "pool-b"} {
		if _, err := f.db.SQL.ExecContext(ctx, f.db.Rebind(`INSERT INTO worker_pools (id, org_id, name, location, isolation_profile, trust_domain, trusted_workloads_only, profile_approved, created_at) VALUES (?, 'o1', ?, 'default', 'rootless-dedicated-vm', 'd', ?, ?, ?)`),
			p, p, false, true, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	local, err := s.LocalProvider(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := s.CreateSecret(ctx, admin, "npm-token", "text", "", []byte("v1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rotate(ctx, admin, sec.ID, 1, []byte("v2"), ""); err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGrant(ctx, admin, GrantRequest{ProjectID: "p1", ProviderID: local.ID, ProviderReference: sec.ID, Usage: "build", PoolIDs: []string{"pool-a"}, Reason: "private packages"}, "")
	if err != nil {
		t.Fatal(err)
	}
	one, err := s.BindGrant(ctx, dev, "p1", "npm-token", g.ID, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.BindGrant(ctx, dev, "p1", "npm-token-ci", g.ID, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	direct, err := s.Bind(ctx, admin, BindRequest{ProjectID: "p1", LogicalID: "npm-direct", ProviderID: local.ID, ProviderReference: sec.ID, Usage: "both", Reason: "direct"}, "")
	if err != nil {
		t.Fatal(err)
	}
	change := GrantChange{Name: "npm", Usage: "both", PoolIDs: []string{"pool-a", "pool-b"}, Reason: "tests need it too"}

	if _, err := s.UpdateGrant(ctx, dev, "p1", g.ID, change, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer edited a grant: %v", err)
	}
	if _, err := s.UpdateGrant(ctx, admin, "p2", g.ID, change, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a grant was edited through another project: %v", err)
	}
	if _, err := s.UpdateGrant(ctx, admin, "p1", g.ID, GrantChange{Name: "npm", Usage: "build", PoolIDs: []string{"nope"}, Reason: "x pools"}, ""); err == nil {
		t.Fatal("an unknown pool was accepted")
	}

	t.Run("widening keeps bindings", func(t *testing.T) {
		u, err := s.UpdateGrant(ctx, admin, "p1", g.ID, change, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(u.Disabled) != 0 || u.Grant.Name != "npm" || u.Grant.Usage != "both" || len(u.Grant.PoolIDs) != 2 {
			t.Fatalf("widen = %+v", u)
		}
		for _, id := range []string{one.ID, two.ID} {
			b, _ := s.binding(ctx, "o1", id)
			if b.Status != Active || b.Version != 1 || b.Usage != "both" || len(b.PoolIDs) != 2 {
				t.Fatalf("binding after widening = %+v", b)
			}
		}
	})

	t.Run("narrowing disables only bindings that no longer fit", func(t *testing.T) {
		if _, err := f.db.SQL.ExecContext(ctx, f.db.Rebind(`UPDATE secret_bindings SET usage = 'build', pool_ids = '["pool-a"]' WHERE id = ?`), two.ID); err != nil {
			t.Fatal(err)
		}
		u, err := s.UpdateGrant(ctx, admin, "p1", g.ID, GrantChange{Name: "npm", Usage: "build", PoolIDs: []string{"pool-a"}, Reason: "builds only on pool a"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(u.Disabled) != 1 || u.Disabled[0].BindingID != one.ID {
			t.Fatalf("disabled = %+v", u.Disabled)
		}
		b1, _ := s.binding(ctx, "o1", one.ID)
		b2, _ := s.binding(ctx, "o1", two.ID)
		bd, _ := s.binding(ctx, "o1", direct.ID)
		if b1.Status != Disabled || !strings.Contains(b1.Reason, "grant narrowed: builds only on pool a") {
			t.Fatalf("non-fitting binding = %+v", b1)
		}
		if b2.Status != Active || b2.Version != 1 || bd.Status != Active || bd.Usage != "both" {
			t.Fatalf("fitting or unrelated binding changed: %+v %+v", b2, bd)
		}
		var n int
		if err := f.db.SQL.QueryRowContext(ctx, f.db.Rebind(`SELECT COUNT(*) FROM audit_events WHERE action = 'secret.unbind' AND resource_id = ?`), one.ID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("disabled binding audit = %d %v", n, err)
		}
		var why string
		if err := f.db.SQL.QueryRowContext(ctx, f.db.Rebind(`SELECT reason FROM audit_events WHERE action = 'secret.grant.update' ORDER BY at DESC LIMIT 1`)).Scan(&why); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(why, "usage both -> build") || !strings.Contains(why, "1 disabled") {
			t.Fatalf("grant update audit = %q", why)
		}
	})

	t.Run("a version change keeps fitting bindings with a new version", func(t *testing.T) {
		u, err := s.UpdateGrant(ctx, admin, "p1", g.ID, GrantChange{Name: "npm", Usage: "build", VersionPolicy: "1", PoolIDs: []string{"pool-a"}, Reason: "pin the old token"}, "")
		if err != nil || len(u.Disabled) != 0 || u.Grant.VersionPolicy != "1" {
			t.Fatalf("pin = %+v %v", u, err)
		}
		b2, _ := s.binding(ctx, "o1", two.ID)
		if b2.Status != Active || b2.Version != 2 || b2.VersionPolicy != "1" {
			t.Fatalf("binding after a version change = %+v", b2)
		}
		if _, err := s.UpdateGrant(ctx, admin, "p1", g.ID, GrantChange{Name: "npm", Usage: "build", VersionPolicy: "9", Reason: "no such version"}, ""); err == nil {
			t.Fatal("a missing version was accepted")
		}
	})

	t.Run("a revoked grant cannot be edited", func(t *testing.T) {
		if err := s.RevokeGrant(ctx, admin, "p1", g.ID, "done", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateGrant(ctx, admin, "p1", g.ID, change, ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("revoked grant edit = %v", err)
		}
	})
}

func TestUpdateAllowedSecret(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	s, admin, dev := f.s, f.admin, f.dev
	s.UseExternal(ProviderVault, &fakeKV{keys: map[string]string{"sa_password": "v"}})
	vault, err := s.CreateProvider(ctx, admin, "vault", ProviderVault, ExternalConfig{Address: "https://vault.example:8200", Mount: "docker", AuthMethod: "approle", RoleID: "samuori"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AllowSecret(ctx, admin, vault.ID, "operations/mssql-base", "SQL Server", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAllowedSecret(ctx, dev, a.ID, "mine", "", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer edited an allowed secret: %v", err)
	}
	var inv *Invalid
	if _, err := s.UpdateAllowedSecret(ctx, admin, a.ID, " ", "", ""); !errors.As(err, &inv) {
		t.Fatalf("an empty label was accepted: %v", err)
	}
	got, err := s.UpdateAllowedSecret(ctx, admin, a.ID, "SQL Server base", "TDE and logins", "")
	if err != nil || got.Label != "SQL Server base" || got.Note != "TDE and logins" || got.Reference != "operations/mssql-base" {
		t.Fatalf("edit = %+v %v", got, err)
	}
	var n int
	if err := f.db.SQL.QueryRowContext(ctx, f.db.Rebind(`SELECT COUNT(*) FROM audit_events WHERE action = 'secret.allow.update' AND resource_id = ?`), a.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit = %d %v", n, err)
	}
	if err := s.RemoveAllowedSecret(ctx, admin, a.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAllowedSecret(ctx, admin, a.ID, "again", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a removed entry was edited: %v", err)
	}
}

func TestCovers(t *testing.T) {
	for _, c := range []struct {
		usage      string
		pools      []string
		innerUsage string
		innerPools []string
		want       bool
	}{
		{"both", nil, "test", []string{"a"}, true},
		{"build", nil, "both", nil, false},
		{"build", []string{"a"}, "build", nil, false},
		{"build", []string{"a", "b"}, "build", []string{"b"}, true},
		{"build", []string{"a"}, "build", []string{"a", "b"}, false},
	} {
		if got := Covers(c.usage, c.pools, c.innerUsage, c.innerPools); got != c.want {
			t.Errorf("Covers(%v) = %v", c, got)
		}
	}
}
