package render

import (
	"encoding/json"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/recipedef"
)

const pin = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func recipePlan(id string) buildplan.Plan {
	return buildplan.Plan{Version: 1, Method: "recipe", Recipe: &buildplan.PlanRecipe{ID: id, Version: "1.0.0",
		BuildImage: "docker.io/library/node:24" + pin, RunImage: "docker.io/library/node:24-slim" + pin,
		Install: buildspec.Command{Argv: []string{"npm", "ci"}}}}
}

func renderFails(t *testing.T, raw []byte) error {
	t.Helper()
	p, err := buildplan.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Dockerfile(p)
	if err == nil {
		t.Fatal("render succeeded")
	}
	return err
}

func TestSkewIsDistinguishedFromPlanContent(t *testing.T) {
	unknownRecipe, _ := recipePlan("rust-service").Encode()
	if err := renderFails(t, unknownRecipe); !Skew(unknownRecipe, err) {
		t.Errorf("an unknown recipe must count as skew: %v", err)
	}
	newerVersion := recipePlan("node-service")
	newerVersion.Recipe.Version = "9.0.0"
	raw, _ := newerVersion.Encode()
	if err := renderFails(t, raw); !Skew(raw, err) {
		t.Errorf("an unknown built-in recipe version must count as skew: %v", err)
	}

	custom := recipePlan("custom-thing")
	custom.Recipe.Custom = &recipedef.Definition{ID: "custom-thing", Version: "1.0.0", RunsAsRoot: "test",
		Stages: []recipedef.Stage{{Name: "final", Image: "docker.io/library/alpine:3" + pin, Steps: []recipedef.Step{{Workdir: "/app"}}}}}
	raw, _ = custom.Encode()
	newer := strings.Replace(string(raw), `{"workdir":"/app"}`, `{"healthcheck":{"cmd":["true"]}}`, 1)
	if newer == string(raw) {
		t.Fatal("fixture did not add a step of a newer kind")
	}
	if err := renderFails(t, []byte(newer)); !Skew([]byte(newer), err) {
		t.Errorf("a step kind this release does not know must count as skew: %v", err)
	}

	var withField map[string]any
	badArg := recipePlan("node-service")
	badArg.BuildArgs = map[string]string{"1bad": "x"}
	raw, _ = badArg.Encode()
	err := renderFails(t, raw)
	if Skew(raw, err) {
		t.Errorf("an invalid build argument is the plan's content, not skew: %v", err)
	}
	_ = json.Unmarshal(raw, &withField)
	withField["sbomPolicy"] = map[string]any{"mode": "strict"}
	raw, _ = json.Marshal(withField)
	if err := renderFails(t, raw); !Skew(raw, err) {
		t.Errorf("a render failure of a plan with fields this release does not know must count as skew: %v", err)
	}
}

// TestSystemPackagesRenderChecksThePlan: the worker re-checks package names and managers, so a plan cannot turn
// them into shell text.
func TestSystemPackagesRenderChecksThePlan(t *testing.T) {
	for name, sp := range map[string]buildplan.PlanSystemPackages{
		"shell text":      {Build: []string{"curl;id"}, BuildManager: "apt"},
		"option":          {Runtime: []string{"--allow-untrusted"}, RuntimeManager: "apk"},
		"unknown manager": {Build: []string{"curl"}, BuildManager: "dnf"},
		"no manager":      {Runtime: []string{"curl"}},
	} {
		p := recipePlan("node-service")
		p.Recipe.SystemPackages = &sp
		if df, err := Dockerfile(p); err == nil {
			t.Errorf("%s rendered:\n%s", name, df)
		}
	}
}

func frontendPlan() buildplan.Plan {
	p := recipePlan("python-service")
	p.Recipe.BuildImage, p.Recipe.RunImage = "docker.io/library/python:3.12"+pin, "docker.io/library/python:3.12-slim"+pin
	p.Recipe.Install = buildspec.Command{Shell: "pip install -r requirements.txt"}
	p.Recipe.Start = buildspec.Command{Argv: []string{"uvicorn", "app.main:app", "--port", "8000"}}
	p.Recipe.AppType, p.Recipe.Port, p.Recipe.Path = "http-service", 8080, "backend"
	p.Recipe.Frontend = &buildplan.PlanFrontend{Path: "frontend", Image: "docker.io/library/node:22" + pin, Node: "22",
		Install: buildspec.Command{Shell: "npm ci"}, Build: buildspec.Command{Shell: "npm run build"}, Output: "dist",
		APIPrefixes: []string{"/api", "/auth"}, BackendPort: 8000, Launcher: buildplan.LauncherContract}
	return p
}

// TestFrontendRendersLauncherImage: the frontend builds in its own stage with no network for its build command, and
// the final image runs the launcher, which starts the application (ADR 0055).
func TestFrontendRendersLauncherImage(t *testing.T) {
	df, err := Dockerfile(frontendPlan())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"FROM docker.io/library/node:22" + pin + " AS samuori-frontend\nWORKDIR /src\nCOPY frontend .\n" +
			`RUN ["/bin/sh","-c","npm ci"]` + "\n" + `RUN --network=none ["/bin/sh","-c","npm run build"]` + "\n",
		"COPY backend .\n",
		"COPY --from=samuori-frontend /src/dist /srv/frontend\nCOPY --from=samuoriweb /samuori-web /usr/local/bin/samuori-web\n",
		`CMD ["/usr/local/bin/samuori-web","--listen",":8080","--root","/srv/frontend","--proxy","/api","--proxy","/auth","--backend","http://127.0.0.1:8000","--","uvicorn","app.main:app","--port","8000"]`,
	} {
		if !strings.Contains(string(df), want) {
			t.Errorf("missing %q in:\n%s", want, df)
		}
	}

	for name, mutate := range map[string]func(*buildplan.PlanFrontend, *buildplan.PlanRecipe){
		"unpinned image": func(f *buildplan.PlanFrontend, _ *buildplan.PlanRecipe) { f.Image = "node:22" },
		"escaping path":  func(f *buildplan.PlanFrontend, _ *buildplan.PlanRecipe) { f.Path = "../etc" },
		"escaping out":   func(f *buildplan.PlanFrontend, _ *buildplan.PlanRecipe) { f.Output = "dist/../../x" },
		"prefix text":    func(f *buildplan.PlanFrontend, _ *buildplan.PlanRecipe) { f.APIPrefixes = []string{"/api\nRUN id"} },
		"no prefixes":    func(f *buildplan.PlanFrontend, _ *buildplan.PlanRecipe) { f.APIPrefixes = nil },
		"no start":       func(_ *buildplan.PlanFrontend, r *buildplan.PlanRecipe) { r.Start = buildspec.Command{} },
		"bad port":       func(f *buildplan.PlanFrontend, _ *buildplan.PlanRecipe) { f.BackendPort = 0 },
		"no image port":  func(_ *buildplan.PlanFrontend, r *buildplan.PlanRecipe) { r.Port = 0 },
	} {
		p := frontendPlan()
		mutate(p.Recipe.Frontend, p.Recipe)
		if df, err := Dockerfile(p); err == nil {
			t.Errorf("%s rendered:\n%s", name, df)
		}
	}
}
