package render

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/recipedef"
	"samuori.invalid/samuori/internal/recipes"
)

var update = flag.Bool("update", false, "rewrite the golden files")

type command struct {
	name                      string
	install, build, start, tc buildspec.Command
}

var commands = []command{
	{name: "argv", install: argvCmd("npm", "ci"), build: argvCmd("npm", "run", "build"), start: argvCmd("node", "server.js")},
	{name: "shell", install: shellCmd("npm ci --omit=dev && echo done"), build: shellCmd("npm run build"), start: shellCmd(`exec node "server.js"`)},
	{name: "install", install: shellCmd("pip install -r requirements.txt")},
	{name: "none"},
	{name: "build-start", build: argvCmd("dotnet", "publish", "-c", "Release", "-o", "out"), start: argvCmd("sh", "-c", `echo "a\b" $HOME`)},
	{name: "test-only", tc: argvCmd("npm", "test")},
}

var apps = []struct {
	kind string
	port int
}{{"http-service", 8080}, {"http-service", 0}, {"worker", 8080}, {"cli-task", 0}, {"static-site", 8080}, {"", 3000}}

var secretSets = map[string][]buildplan.PlanSecret{
	"none": nil,
	"mixed": {
		{ID: "npm_token", Required: true, Target: "/run/secrets/npm_token", BindingID: "b1", BindingVersion: 2},
		{ID: "pip.conf", Target: "/home/app/.config/pip/pip.conf"},
	},
}

var argSets = map[string]map[string]string{
	"none": nil,
	"some": {"Z_LAST": "z", "A_FIRST": "a", "mid_name": "m", strings.Repeat("L", 100): "long"},
}

func argvCmd(a ...string) buildspec.Command { return buildspec.Command{Argv: a} }
func shellCmd(s string) buildspec.Command   { return buildspec.Command{Shell: s} }

func outcome(p buildplan.Plan) (string, []byte) {
	raw, _ := p.Encode()
	df, err := Dockerfile(p)
	if err != nil {
		return fmt.Sprintf("error skew=%t: %v", Skew(raw, err), err), nil
	}
	sum := sha256.Sum256(df)
	return hex.EncodeToString(sum[:8]), df
}

func builtinMatrix(t *testing.T) (map[string]buildplan.Plan, []string) {
	cat, err := recipes.LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]buildplan.Plan{}
	for _, r := range cat.All() {
		for _, rt := range r.Runtimes {
			for _, app := range apps {
				images := rt.Images(app.kind)
				for _, c := range commands {
					for sn, secrets := range secretSets {
						for an, args := range argSets {
							name := fmt.Sprintf("%s@%s/%s/%s:%d/%s/secrets=%s/args=%s", r.ID, r.Version, rt.Version, app.kind, app.port, c.name, sn, an)
							cases[name] = buildplan.Plan{Version: 1, Method: "recipe", BuildArgs: args, Secrets: secrets,
								Recipe: &buildplan.PlanRecipe{ID: r.ID, Version: r.Version, Runtime: rt.Version, BuildImage: images[0], RunImage: images[len(images)-1],
									Install: c.install, Build: c.build, Start: c.start, Test: c.tc, AppType: app.kind, Port: app.port}}
						}
					}
				}
			}
		}
	}
	node := func(mut func(*buildplan.PlanRecipe), p buildplan.Plan) buildplan.Plan {
		p.Version, p.Method = 1, "recipe"
		p.Recipe = &buildplan.PlanRecipe{ID: "node-service", Version: "1.0.0", BuildImage: "docker.io/library/node:24" + pin, RunImage: "docker.io/library/node:24-slim" + pin,
			Install: argvCmd("npm", "ci"), AppType: "http-service", Port: 3000}
		if mut != nil {
			mut(p.Recipe)
		}
		return p
	}
	badSecret := []buildplan.PlanSecret{{ID: "ok", Target: "/run/secrets/ok"}, {ID: "bad", Target: "/run/../etc/shadow"}, {ID: "Bad!", Target: "/x"}}
	cases["error/unpinned-build-image"] = node(func(r *buildplan.PlanRecipe) { r.BuildImage = "docker.io/library/node:24" }, buildplan.Plan{})
	cases["error/unpinned-run-image-unknown-recipe"] = node(func(r *buildplan.PlanRecipe) { r.ID, r.RunImage = "rust-service", "rust:1" }, buildplan.Plan{})
	cases["error/unknown-recipe"] = node(func(r *buildplan.PlanRecipe) { r.ID = "rust-service" }, buildplan.Plan{})
	cases["error/bad-arg"] = node(nil, buildplan.Plan{BuildArgs: map[string]string{"1bad": "x", "good": "y"}})
	cases["error/arg-too-long"] = node(nil, buildplan.Plan{BuildArgs: map[string]string{strings.Repeat("a", 129): "x"}})
	cases["error/bad-arg-unknown-recipe"] = node(func(r *buildplan.PlanRecipe) { r.ID = "rust-service" }, buildplan.Plan{BuildArgs: map[string]string{"bad-name": "x"}})
	cases["error/bad-secret-with-install"] = node(nil, buildplan.Plan{Secrets: badSecret})
	cases["error/bad-secret-build-only"] = node(func(r *buildplan.PlanRecipe) { r.Install, r.Build = buildspec.Command{}, shellCmd("make") }, buildplan.Plan{Secrets: badSecret})
	cases["error/bad-secret-no-commands"] = node(func(r *buildplan.PlanRecipe) { r.Install = buildspec.Command{} }, buildplan.Plan{Secrets: badSecret})
	cases["error/bad-secret-powershell-no-install"] = node(func(r *buildplan.PlanRecipe) {
		r.ID, r.Install, r.Build = "powershell-task", buildspec.Command{}, shellCmd("never rendered")
	}, buildplan.Plan{Secrets: badSecret})
	cases["edge/high-port"] = node(func(r *buildplan.PlanRecipe) { r.Port = 70000 }, buildplan.Plan{})
	cases["edge/secret-target-root-chars"] = node(nil, buildplan.Plan{Secrets: []buildplan.PlanSecret{{ID: "a.b_c-d", Target: "/a/b.c_d-e/f", Required: true}}})
	cases["edge/short-image-pin"] = node(func(r *buildplan.PlanRecipe) { r.BuildImage, r.RunImage = "node"+pin, "node"+pin }, buildplan.Plan{})
	cases["error/not-a-recipe"] = buildplan.Plan{Version: 1, Method: "dockerfile"}
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	slices.Sort(names)
	return cases, names
}

func customMatrix(t *testing.T) map[string]buildplan.Plan {
	d, err := recipedef.Parse([]byte(`
id: tooling-base
version: 1.0.0
displayName: Tooling base
platforms: [linux/amd64]
secrets: [npm_token, optional_token]
parameters:
  - {name: tag, type: string, default: base}
  - {name: port, type: port, default: "8080"}
stages:
  - name: build
    image: docker.io/library/ubuntu:24.04` + pin + `
    steps:
      - workdir: /src
      - copySource: {from: ., to: .}
      - run: {shell: "apt-get update", network: egress, secrets: [npm_token, optional_token]}
      - run: {argv: [make, "${tag}"], secrets: [npm_token]}
  - name: final
    from: build
    steps:
      - copyFromStage: {stage: build, from: /src/out, to: /app, owner: "10001:10001"}
      - env: {name: MODE, value: production}
      - label: {name: org.opencontainers.image.version, value: "${tag}"}
      - expose: "${port}"
      - user: "10001"
      - cmd: [/app/run, "${tag}"]
`))
	if err != nil {
		t.Fatal(err)
	}
	secrets := []buildplan.PlanSecret{{ID: "npm_token", Required: true, Target: "/run/secrets/npm_token", BindingID: "b1"}, {ID: "optional_token", Target: "/run/secrets/optional_token"}}
	plan := func(params, args map[string]string) buildplan.Plan {
		return buildplan.Plan{Version: 1, Method: "recipe", BuildArgs: args, Secrets: secrets,
			Recipe: &buildplan.PlanRecipe{ID: d.ID, Version: d.Version, Custom: &d, Parameters: params}}
	}
	return map[string]buildplan.Plan{
		"custom/defaults":   plan(map[string]string{"tag": "base", "port": "8080"}, map[string]string{"tag": "base", "port": "8080"}),
		"custom/extra-args": plan(map[string]string{"tag": "release-1", "port": "9000"}, map[string]string{"tag": "release-1", "port": "9000", "EXTRA": "x"}),
	}
}

// TestRenderGolden pins the rendered Dockerfile of every built-in recipe, runtime and plan shape byte for byte.
func TestRenderGolden(t *testing.T) {
	cases, names := builtinMatrix(t)
	for n, p := range customMatrix(t) {
		cases[n] = p
		names = append(names, n)
	}
	slices.Sort(names)
	var index strings.Builder
	full := map[string][]byte{}
	for _, n := range names {
		got, df := outcome(cases[n])
		fmt.Fprintf(&index, "%s\t%s\n", n, got)
		if df != nil && (strings.Contains(n, "/argv/secrets=mixed/args=some") && strings.Contains(n, "/http-service:8080/") || strings.HasPrefix(n, "custom/") || strings.HasPrefix(n, "edge/")) {
			full[n] = df
		}
	}
	path := filepath.Join("testdata", "golden.txt")
	if *update {
		var all strings.Builder
		all.WriteString(index.String())
		for _, n := range names {
			if df, ok := full[n]; ok {
				fmt.Fprintf(&all, "\n=== %s\n%s", n, df)
			}
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(all.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantIndex, wantFull, _ := strings.Cut(string(want), "\n=== ")
	if wantIndex != index.String() {
		wl, gl := strings.Split(wantIndex, "\n"), strings.Split(index.String(), "\n")
		for i := range max(len(wl), len(gl)) {
			var w, g string
			if i < len(wl) {
				w = wl[i]
			}
			if i < len(gl) {
				g = gl[i]
			}
			if w != g {
				t.Errorf("golden line %d:\n want %s\n  got %s", i+1, w, g)
			}
		}
	}
	blocks := strings.Split("=== "+wantFull, "\n=== ")
	if len(blocks) != len(full) {
		t.Errorf("golden has %d full Dockerfiles, the matrix %d", len(blocks), len(full))
	}
	for _, block := range blocks {
		n, df, _ := strings.Cut(strings.TrimPrefix(block, "=== "), "\n")
		if got, ok := full[n]; !ok || string(got) != df {
			t.Errorf("%s:\nwant\n%s\ngot\n%s", n, df, got)
		}
	}
	t.Logf("%d plans compared", len(names))
}
