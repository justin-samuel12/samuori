package builds

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/engine/render"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/sources"
)

// TestSystemPackagesPlan: system packages are installed with each image's own package manager in its stage, the
// distribution's egress preset joins the plan, and an image whose distribution is unknown is refused.
func TestSystemPackagesPlan(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
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
	plan := func(recipe, kind, lang, constraint string, sp *buildspec.SystemPackages) (Plan, error) {
		doc := &buildspec.Document{Spec: buildspec.Spec{
			Application: &buildspec.Application{Type: kind, Port: 8080, Runtime: &buildspec.Runtime{Language: lang, VersionConstraint: constraint}},
			Build: buildspec.Build{Method: "recipe", Recipe: &buildspec.Recipe{ID: recipe, Version: "1.0.0",
				Build: buildspec.Command{Shell: "npm run build"}, Start: buildspec.Command{Argv: []string{"node", "server.js"}}, SystemPackages: sp}},
		}}
		return resolvePlan(ctx, doc, "sha256:spec", sources.Snapshot{Digest: "sha256:snap"}, sources.Manifest{}, rec, "o1", eff, nil)
	}

	plain, err := plan("node-service", "http-service", "nodejs", "24", nil)
	if err != nil || plain.Recipe.SystemPackages != nil || slices.Contains(plain.Egress.Presets, "debian@1.0.0") {
		t.Fatalf("a spec without system packages changed: %+v %v", plain, err)
	}

	node, err := plan("node-service", "http-service", "nodejs", "24", &buildspec.SystemPackages{Build: []string{"python3", "g++"}, Runtime: []string{"libvips42"}})
	if err != nil {
		t.Fatal(err)
	}
	if sp := node.Recipe.SystemPackages; sp.BuildManager != "apt" || sp.RuntimeManager != "apt" || !slices.Contains(node.Egress.Presets, "debian@1.0.0") {
		t.Fatalf("node plan = %+v, presets %v", sp, node.Egress.Presets)
	}
	df, err := render.Dockerfile(node)
	if err != nil {
		t.Fatal(err)
	}
	stages := strings.Split(string(df), "\nFROM ")
	if len(stages) != 3 ||
		!strings.Contains(stages[1], `USER 0
RUN ["/bin/sh","-c","apt-get update \u0026\u0026 DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends python3 g++ \u0026\u0026 rm -rf /var/lib/apt/lists/*"]
WORKDIR /app`) ||
		!strings.Contains(stages[2], "--no-install-recommends libvips42 ") || strings.Contains(stages[2], "python3") ||
		!strings.HasSuffix(strings.TrimSpace(stages[2]), `CMD ["node","server.js"]`) || !strings.Contains(stages[2], "USER 10001") {
		t.Fatalf("node Dockerfile:\n%s", df)
	}

	site, err := plan("static-site", "static-site", "nodejs", "24", &buildspec.SystemPackages{Runtime: []string{"curl"}})
	if err != nil {
		t.Fatal(err)
	}
	if sp := site.Recipe.SystemPackages; sp.BuildManager != "" || sp.RuntimeManager != "apk" || !slices.Contains(site.Egress.Presets, "alpine@1.0.0") || slices.Contains(site.Egress.Presets, "debian@1.0.0") {
		t.Fatalf("static site plan = %+v, presets %v", sp, site.Egress.Presets)
	}
	if df, err = render.Dockerfile(site); err != nil || !strings.Contains(string(df), "USER 0\nRUN [\"/bin/sh\",\"-c\",\"apk add --no-cache curl\"]\nCOPY --from=build") {
		t.Fatalf("static site Dockerfile (%v):\n%s", err, df)
	}

	dotnet, err := plan("dotnet-service", "worker", "csharp", "9.0", &buildspec.SystemPackages{Runtime: []string{"libgdiplus"}})
	if err != nil || dotnet.Recipe.SystemPackages.RuntimeManager != "apt" {
		t.Fatalf("dotnet images are declared Debian: %+v %v", dotnet.Recipe, err)
	}

	admin := authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	unknown := "docker.io/library/node:25@sha256:" + strings.Repeat("d", 64)
	o, err := rec.CreateRuntime(ctx, admin, "node-service", "1.0.0", recipes.RuntimeInput{Label: "25", BuildImage: unknown, RunImage: unknown}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.DecideRuntime(ctx, admin, "node-service", "1.0.0", o.ID, "approve", "reviewed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := plan("node-service", "http-service", "nodejs", "25", nil); err != nil {
		t.Fatalf("an unknown distribution matters only with system packages: %v", err)
	}
	if _, err := plan("node-service", "http-service", "nodejs", "25", &buildspec.SystemPackages{Build: []string{"git"}}); err == nil || !strings.Contains(err.Error(), "package manager") {
		t.Fatalf("packages for an image of unknown distribution = %v", err)
	}
}
