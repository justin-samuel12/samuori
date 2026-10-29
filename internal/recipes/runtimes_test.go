package recipes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

func newRuntimeStore(t *testing.T) (*Statuses, *db.DB) {
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

// TestShippedRuntimeStatus: only organization administrators deprecate or withdraw a shipped runtime; a withdrawn
// one is never resolved, not even through an organization runtime with the same label; deprecated ones still resolve;
// approving again clears the decision; other organizations are unaffected (ADR 0060).
func TestShippedRuntimeStatus(t *testing.T) {
	ctx := context.Background()
	s, d := newRuntimeStore(t)
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO organizations (id, name, created_at) VALUES ('o2', 'other', ?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	admin := authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	dev := authz.Principal{ID: "dev", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Developer, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	resolve := func(org, constraint string) (Runtime, error) {
		r, _, err := s.Get(ctx, org, "node-service", "1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		return r.ResolveRuntime(constraint)
	}
	set := func(p authz.Principal, label, status, reason string) error {
		return s.SetShippedRuntimeStatus(ctx, p, "node-service", "1.0.0", label, status, reason, "")
	}

	if err := set(dev, "22", Withdrawn, "end of life"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer withdrew a shipped runtime: %v", err)
	}
	if err := set(admin, "21", Withdrawn, "end of life"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unshipped label = %v", err)
	}
	var inv *Invalid
	if err := set(admin, "22", Withdrawn, " "); !errors.As(err, &inv) {
		t.Fatalf("a withdrawal without a reason = %v", err)
	}
	if err := set(admin, "22", Withdrawn, "end of life"); err != nil {
		t.Fatal(err)
	}
	if rt, err := resolve("o1", ">=22"); err != nil || rt.Version != "24" {
		t.Errorf(">=22 resolved to %+v, %v; want 24", rt, err)
	}
	if _, err := resolve("o1", "22"); err == nil || !strings.Contains(err.Error(), "runtime 22 of Node.js application has been withdrawn by an administrator: end of life") {
		t.Errorf("a withdrawn shipped runtime resolved: %v", err)
	}
	node22 := "docker.io/library/node:22-bookworm-slim@sha256:" + strings.Repeat("d", 64)
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO organization_runtimes (id, org_id, recipe_id, recipe_version, label, build_image, run_image, status, created_by, created_at) VALUES ('same', 'o1', 'node-service', '1.0.0', '22', ?, ?, 'approved', 'admin', ?)`,
		node22, node22, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if rt, err := resolve("o1", "22"); err == nil {
		t.Errorf("an organization runtime replaced a withdrawn shipped one: %+v", rt)
	}
	if rt, err := resolve("o2", "22"); err != nil || rt.Version != "22" || rt.Status != Approved {
		t.Errorf("another organization is affected: %+v, %v", rt, err)
	}
	if shipped, _, _ := s.ListRuntimes(ctx, dev, "node-service", "1.0.0"); len(shipped) != 1 || shipped[0].Version != "24" {
		t.Errorf("a developer lists %+v", shipped)
	}
	if shipped, _, _ := s.ListRuntimes(ctx, admin, "node-service", "1.0.0"); len(shipped) != 2 || shipped[0].Status != Withdrawn || shipped[0].StatusBy != "admin" || shipped[0].StatusAt == nil {
		t.Errorf("an administrator lists %+v", shipped)
	}

	if err := set(admin, "24", Deprecated, "move to 26"); err != nil {
		t.Fatal(err)
	}
	if rt, err := resolve("o1", ">=22"); err != nil || rt.Version != "24" || rt.Status != Deprecated || rt.Reason != "move to 26" {
		t.Errorf("a deprecated runtime = %+v, %v", rt, err)
	}
	if err := set(admin, "22", Approved, "extended support"); err != nil {
		t.Fatal(err)
	}
	if rt, err := resolve("o1", "22"); err != nil || rt.Source != SourceBuiltIn || rt.Status != Approved {
		t.Errorf("approving again did not restore 22: %+v, %v", rt, err)
	}
	var rows, audited int
	_ = d.SQL.QueryRow(`SELECT COUNT(*) FROM shipped_runtime_status WHERE org_id = 'o1'`).Scan(&rows)
	_ = d.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'recipe.runtime.status'`).Scan(&audited)
	if rows != 1 || audited != 3 {
		t.Errorf("rows = %d, audited = %d; want 1 and 3", rows, audited)
	}
}
