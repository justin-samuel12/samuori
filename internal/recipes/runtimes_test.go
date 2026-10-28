package recipes

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
)

func newRuntimeStore(t *testing.T) (*Statuses, *db.DB) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	cat, err := LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	return NewStatuses(d, cat), d
}

// TestOrganizationRuntimeResolution: shipped runtimes win, so an added runtime never changes what an existing spec
// resolves to; drafts and withdrawn runtimes are never resolved; the base-image policy is checked again at approval
// (ADR 0051).
func TestOrganizationRuntimeResolution(t *testing.T) {
	ctx := context.Background()
	s, d := newRuntimeStore(t)
	admin := authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	node25 := "docker.io/library/node:25-bookworm-slim@sha256:" + strings.Repeat("c", 64)
	in := RuntimeInput{Label: "25", BuildImage: node25, RunImage: node25}

	if _, err := s.CreateRuntime(ctx, admin, "node-service", "1.0.0", in, ""); err == nil {
		t.Fatal("a runtime was added without a base-image policy check")
	}
	allowed := true
	s.UseImagePolicy(func(context.Context, string, string) (string, error) {
		if allowed {
			return "", nil
		}
		return "its repository docker.io/library/node is not on the approved list", nil
	})
	o, err := s.CreateRuntime(ctx, admin, "node-service", "1.0.0", in, "")
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(constraint string) (Runtime, error) {
		r, _, err := s.Get(ctx, "o1", "node-service", "1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		return r.ResolveRuntime(constraint)
	}
	if _, err := resolve("25"); err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("a draft resolved: %v", err)
	}
	allowed = false
	var inv *Invalid
	if _, err := s.DecideRuntime(ctx, admin, "node-service", "1.0.0", o.ID, "approve", "reviewed", ""); !errors.As(err, &inv) {
		t.Fatalf("approved although the policy now refuses the image: %v", err)
	}
	allowed = true
	if _, err := s.DecideRuntime(ctx, admin, "node-service", "1.0.0", o.ID, "approve", "reviewed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideRuntime(ctx, admin, "static-site", "1.0.0", o.ID, "withdraw", "wrong recipe", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a runtime was decided through another recipe: %v", err)
	}
	for constraint, want := range map[string]string{"25": "organization", ">=25": "organization", ">=22": "built-in", "": "built-in", "24": "built-in"} {
		rt, err := resolve(constraint)
		if err != nil || rt.Source != want {
			t.Errorf("%q resolved to %+v, %v; want %s", constraint, rt, err, want)
		}
		if want == "organization" && (rt.ID != o.ID || rt.BuildImage != node25 || rt.Version != "25") {
			t.Errorf("%q resolved to %+v", constraint, rt)
		}
	}
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO organization_runtimes (id, org_id, recipe_id, recipe_version, label, build_image, run_image, status, created_by, created_at) VALUES ('later', 'o1', 'node-service', '1.0.0', '22', ?, ?, 'approved', 'admin', ?)`,
		node25, node25, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if rt, _ := resolve("22"); rt.Source != SourceBuiltIn {
		t.Errorf("an organization runtime overrode a shipped one with the same label: %+v", rt)
	}
	_, org, err := s.ListRuntimes(ctx, admin, "node-service", "1.0.0")
	if err != nil || len(org) != 2 || org[0].Superseded || !org[1].Superseded {
		t.Errorf("listing = %+v, %v; want the shipped label marked superseded", org, err)
	}
	if _, err := s.DecideRuntime(ctx, admin, "node-service", "1.0.0", o.ID, "withdraw", "end of life", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve("25"); err == nil || !strings.Contains(err.Error(), "withdrawn by an administrator: end of life") {
		t.Fatalf("a withdrawn runtime resolved: %v", err)
	}
	other := authz.Principal{ID: "x", OrgID: "o2", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o2"}}}
	if _, org, _ := s.ListRuntimes(ctx, other, "node-service", "1.0.0"); len(org) != 0 {
		t.Errorf("another organization sees these runtimes: %+v", org)
	}
}
