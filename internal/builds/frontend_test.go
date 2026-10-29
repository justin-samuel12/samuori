package builds

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/engine/render"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

// TestFrontendPlan: a frontend gets a pinned Node.js image from the Node.js recipe, its defaults, the npm egress
// preset beside the application's own, and a launcher requirement; custom recipes cannot take one (ADR 0055).
func TestFrontendPlan(t *testing.T) {
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
	presets, err := egress.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	eff := policy.Effective{Egress: presets.DefaultPolicy()}
	doc := &buildspec.Document{Spec: buildspec.Spec{
		Application: &buildspec.Application{Type: "http-service", Port: 8080, Runtime: &buildspec.Runtime{Language: "python"}},
		Build: buildspec.Build{Method: "recipe", Recipe: &buildspec.Recipe{ID: "python-service", Version: "1.0.0", Path: "backend",
			Install: buildspec.Command{Shell: "pip install -r requirements.txt"}, Start: buildspec.Command{Argv: []string{"uvicorn", "app.main:app"}},
			Frontend: &buildspec.Frontend{Path: "frontend", Node: "22"}}},
	}}
	p, err := resolvePlan(ctx, doc, "sha256:spec", sources.Snapshot{Digest: "sha256:snap"}, sources.Manifest{}, rec, "o1", eff, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := p.Recipe.Frontend
	if f == nil || !strings.Contains(f.Image, "node:22") || !strings.Contains(f.Image, "@sha256:") || f.Node != "22" || f.Output != "dist" ||
		f.Install.Shell != "npm ci" || f.Build.Shell != "npm run build" || !slices.Equal(f.APIPrefixes, []string{"/api"}) || f.BackendPort != 8000 ||
		f.Launcher != buildplan.LauncherContract || p.Recipe.Path != "backend" {
		t.Fatalf("frontend plan = %+v (path %q)", f, p.Recipe.Path)
	}
	if !slices.Contains(p.Egress.Presets, "npm@1.0.0") || !slices.Contains(p.Egress.Presets, "pypi@1.0.0") {
		t.Fatalf("presets = %v", p.Egress.Presets)
	}
	if _, err := render.Dockerfile(p); err != nil {
		t.Fatal(err)
	}

	doc.Spec.Build.Recipe.Frontend.Node = "8"
	if _, err := resolvePlan(ctx, doc, "sha256:spec", sources.Snapshot{Digest: "sha256:snap"}, sources.Manifest{}, rec, "o1", eff, nil); err == nil {
		t.Error("a Node.js version without a runtime was planned")
	}
}
