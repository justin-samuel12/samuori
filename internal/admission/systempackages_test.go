package admission

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

// TestSystemPackagesAdmission: system packages add their distribution's egress preset, and admission refuses them
// with the field and reason when an image's package manager is unknown.
func TestSystemPackagesAdmission(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.SQLite(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	cat, err := recipes.LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	rec := recipes.NewStatuses(d, cat)
	rec.UseImagePolicy(func(context.Context, string, string) (string, error) { return "", nil })
	admin := authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	unknown := "docker.io/library/node:25@sha256:" + strings.Repeat("d", 64)
	o, err := rec.CreateRuntime(ctx, admin, "node-service", "1.0.0", recipes.RuntimeInput{Label: "25", BuildImage: unknown, RunImage: unknown}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.DecideRuntime(ctx, admin, "node-service", "1.0.0", o.ID, "approve", "reviewed", ""); err != nil {
		t.Fatal(err)
	}
	presets, err := egress.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	eff := policy.Effective{Egress: presets.DefaultPolicy()}
	c := &Checker{recipes: rec}
	check := func(constraint string, sp *buildspec.SystemPackages) ([]string, []buildspec.Issue) {
		doc := &buildspec.Document{Spec: buildspec.Spec{
			Application: &buildspec.Application{Type: "worker", Runtime: &buildspec.Runtime{Language: "nodejs", VersionConstraint: constraint}},
			Build:       buildspec.Build{Method: "recipe", Recipe: &buildspec.Recipe{ID: "node-service", Version: "1.0.0", SystemPackages: sp}},
		}}
		var issues []buildspec.Issue
		semantic := func(rule, ptr, format string, args ...any) {
			issues = append(issues, buildspec.Issue{Rule: rule, Path: ptr})
		}
		c.recipeChecks(ctx, "o1", doc, []string{"linux/amd64"}, &Report{}, semantic, func(string, string, ...any) {})
		got, _ := c.egressPresets(ctx, "o1", doc, eff)
		return got, issues
	}

	if got, issues := check("24", nil); !slices.Equal(got, []string{"npm"}) || len(issues) != 0 {
		t.Fatalf("without system packages = %v %v", got, issues)
	}
	if got, issues := check("24", &buildspec.SystemPackages{Build: []string{"python3"}}); !slices.Equal(got, []string{"npm", "debian"}) || len(issues) != 0 {
		t.Fatalf("with build packages = %v %v", got, issues)
	}
	got, issues := check("25", &buildspec.SystemPackages{Runtime: []string{"curl"}})
	if !slices.Equal(got, []string{"npm"}) || len(issues) != 1 || issues[0].Rule != "system-packages-unsupported" || issues[0].Path != "/spec/build/recipe/systemPackages/runtime" {
		t.Fatalf("packages on an image of unknown distribution = %v %v", got, issues)
	}
}
