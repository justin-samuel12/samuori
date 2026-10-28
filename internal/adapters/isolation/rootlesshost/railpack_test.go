package rootlesshost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/adapters/build/buildkit"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/redact"
)

const (
	builder = "ghcr.io/railwayapp/railpack-builder:mise-2026.10.14@sha256:03df9e9aa1e786c2e0dd12c5409acad3bbd5a0fa8a75c1117c7ddab5c0c3fde4"
	rtImage = "ghcr.io/railwayapp/railpack-runtime:mise-2026.10.14@sha256:02820533713338569ae4d4f0df986c3e7cc6e93457fd06fbb0d2cfa86fba9cdb"
)

// goodPlan has the shape railpack prepare v0.43.0 wrote for examples/orders-api (docs/spikes/railpack-builds.md).
const goodPlan = `{"steps":[
 {"name":"packages:mise","inputs":[{"image":"ghcr.io/railwayapp/railpack-builder:mise-2026.10.14"}],"commands":[{"cmd":"mise install"}]},
 {"name":"install","inputs":[{"step":"packages:mise"}],"commands":[{"cmd":"npm install"}],"variables":{"NODE_ENV":"production"}},
 {"name":"build","inputs":[{"step":"install"},{"local":true,"include":["."]}],"secrets":["*"]},
 {"name":"packages:apt:runtime","inputs":[{"image":"ghcr.io/railwayapp/railpack-runtime:mise-2026.10.14"}],"commands":[{"cmd":"apt-get install -y libatomic1"}]}],
 "deploy":{"base":{"step":"packages:apt:runtime"},"inputs":[{"step":"build","include":["."]}],"startCommand":"npm run start","variables":{"NODE_ENV":"production"}},
 "secrets":["FROM_RAILPACK_JSON"]}`

var rp = &buildplan.PlanRailpack{Version: "v0.43.0", Images: []string{builder, rtImage}, Context: ".", CacheKey: "cs-project-a"}

// TestRailpackPlanContent: Samuori judges the plan by its content, never railpack's exit status, so an empty or partial
// plan fails the build; the plan it builds names only approved digest-pinned images and exactly the bound secrets
// (SEC-RPK-05, ADR 0047).
func TestRailpackPlanContent(t *testing.T) {
	success := []byte(`{"success":true}`)
	for name, c := range map[string]struct {
		plan, info, want string
	}{
		"no plan file":           {"", `{"success":true}`, "wrote no plan"},
		"planning failed":        {goodPlan, `{"success":false,"logs":[{"Level":"error","Msg":"Railpack could not determine how to build the app."}]}`, "could not determine how to build"},
		"near-empty plan":        {`{"$schema":"https://schema.railpack.com","deploy":{}}`, `{"success":true}`, "no build steps"},
		"steps without commands": {`{"steps":[{"name":"build"}],"deploy":{"base":{"step":"build"},"startCommand":"x"}}`, "", "runs no commands"},
		"no start command":       {strings.Replace(goodPlan, `"startCommand":"npm run start",`, "", 1), "", "no start command"},
		"no image base":          {strings.Replace(goodPlan, `"base":{"step":"packages:apt:runtime"},`, "", 1), "", "no base"},
		"a missing step":         {strings.Replace(goodPlan, `{"step":"install"},`, `{"step":"compile"},`, 1), "", `missing step "compile"`},
		"an unapproved image":    {strings.Replace(goodPlan, "ghcr.io/railwayapp/railpack-runtime:mise-2026.10.14", "docker.io/library/node:22", 1), "", `"docker.io/library/node:22", which is not an approved`},
		"another builder tag":    {strings.Replace(goodPlan, "railpack-builder:mise-2026.10.14", "railpack-builder:mise-2026.11.1", 1), "", "not an approved Railpack image"},
		"unreadable info":        {goodPlan, "{", "unreadable railpack-info.json"},
	} {
		if _, err := CheckRailpackPlan([]byte(c.plan), []byte(c.info), rp, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, c.want)
		}
	}

	built, err := CheckRailpackPlan([]byte(goodPlan), success, rp, []string{"NPM_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Steps []struct {
			Name   string `json:"name"`
			Inputs []struct {
				Image string `json:"image"`
			} `json:"inputs"`
		} `json:"steps"`
		Secrets []string `json:"secrets"`
	}
	if err := json.Unmarshal(built, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].Inputs[0].Image != builder || plan.Steps[3].Inputs[0].Image != rtImage {
		t.Errorf("images are not pinned to the approved digests: %s", built)
	}
	if strings.Join(plan.Secrets, ",") != "NPM_TOKEN" {
		t.Errorf("plan secrets = %v; want exactly the bound build secrets", plan.Secrets)
	}

	run, err := WithProxySecrets(built)
	if err != nil {
		t.Fatal(err)
	}
	var withProxy struct {
		Steps []struct {
			Name      string            `json:"name"`
			Secrets   []string          `json:"secrets"`
			Variables map[string]string `json:"variables"`
		} `json:"steps"`
		Deploy struct {
			Variables map[string]string `json:"variables"`
		} `json:"deploy"`
		Secrets []string `json:"secrets"`
	}
	if err := json.Unmarshal(run, &withProxy); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(withProxy.Secrets, ","), "NPM_TOKEN,HTTP_PROXY,HTTPS_PROXY") {
		t.Errorf("run plan secrets = %v", withProxy.Secrets)
	}
	for _, s := range withProxy.Steps {
		for k := range s.Variables {
			if strings.Contains(strings.ToLower(k), "proxy") {
				t.Errorf("step %s carries %s as a variable, which Railpack copies into the image", s.Name, k)
			}
		}
		if s.Name == "install" && !strings.Contains(strings.Join(s.Secrets, ","), "HTTPS_PROXY") {
			t.Errorf("the install step does not receive the proxy: %v", s.Secrets)
		}
	}
	for k := range withProxy.Deploy.Variables {
		if strings.Contains(strings.ToLower(k), "proxy") {
			t.Errorf("the image configuration gets %s", k)
		}
	}
}

// TestRailpackSecretsAndCacheKey: build steps get bound secrets by logical ID and the proxy only, values never
// appear in the plan, and the frontend receives the controller's cache scope as cache-key (SEC-RPK-02, SEC-RPK-03).
func TestRailpackSecretsAndCacheKey(t *testing.T) {
	plan := buildplan.Plan{Method: "railpack", Railpack: &buildplan.PlanRailpack{SecretEnv: map[string]string{"NPM_TOKEN": "npm-token", "OPTIONAL_KEY": "optional-key"}},
		Secrets: []buildplan.PlanSecret{{ID: "npm-token", Required: true, BindingID: "b1", BindingVersion: 3}, {ID: "optional-key"}}}
	value := []byte("npm_s3cr3t-canary-value")
	release := func(_ context.Context, id string) ([]byte, error) {
		if id == "npm-token" {
			return value, nil
		}
		return nil, isolation.ErrSecretUnbound
	}
	mask := redact.New()
	src := railpackSecrets(secretSource(plan, release, mask, nil), plan.Railpack.SecretEnv, "http://samuori:tok@10.0.0.1:3128", []string{"mirror:5000"})
	ctx := context.Background()
	if v, err := src(ctx, "NPM_TOKEN"); err != nil || string(v) != string(value) {
		t.Errorf("NPM_TOKEN = %q, %v", v, err)
	}
	if v, _ := src(ctx, "HTTPS_PROXY"); string(v) != "http://samuori:tok@10.0.0.1:3128" {
		t.Errorf("HTTPS_PROXY = %q", v)
	}
	if v, _ := src(ctx, "NO_PROXY"); string(v) != "mirror:5000" {
		t.Errorf("NO_PROXY = %q", v)
	}
	for _, id := range []string{"npm-token", "AWS_SECRET_ACCESS_KEY"} {
		if _, err := src(ctx, id); err == nil || !strings.Contains(err.Error(), "unauthorized secret mount") {
			t.Errorf("secret %s outside the plan's variables = %v", id, err)
		}
	}
	if _, err := src(ctx, "OPTIONAL_KEY"); !errors.Is(err, buildkit.ErrSecretNotFound) {
		t.Errorf("an unbound optional secret = %v", err)
	}
	if got := boundEnv(plan); strings.Join(got, ",") != "NPM_TOKEN" {
		t.Errorf("declared secrets = %v; an unbound optional secret must not be declared", got)
	}
	if strings.Contains(string(mask.Mask(value)), string(value)) {
		t.Error("a delivered value is not masked in logs")
	}
	if h := secretsHash(plan); strings.Contains(h, "s3cr3t") || len(h) != 64 {
		t.Errorf("secrets-hash = %q", h)
	}
	built, _ := CheckRailpackPlan([]byte(goodPlan), nil, rp, boundEnv(plan))
	if strings.Contains(string(built), string(value)) {
		t.Error("a secret value is in the plan")
	}

	attrs := buildkit.Attrs(buildkit.Request{Frontend: "ghcr.io/railwayapp/railpack-frontend:v0.43.0@sha256:355ef61fae6f2551103cd8b10c710cb07167dd353d561f15c69b8d7ac1c676cc",
		Filename: "railpack-plan.json", FrontendOpts: map[string]string{"build-arg:cache-key": "cs-project-a"}, Proxy: "http://samuori:tok@10.0.0.1:3128",
		BuildArgs: map[string]string{"X": "y"}, Platforms: []string{"linux/arm64"}})
	if attrs["build-arg:cache-key"] != "cs-project-a" || attrs["source"] == "" || attrs["filename"] != "railpack-plan.json" || attrs["platform"] != "linux/arm64" {
		t.Errorf("frontend attributes = %v", attrs)
	}
	for k := range attrs {
		if strings.Contains(strings.ToLower(k), "proxy") || k == "build-arg:X" {
			t.Errorf("the Railpack frontend receives %s", k)
		}
	}
}

// TestRailpackBinaryChecksum: the worker runs only a copy of the binary whose SHA-256 matches the approval, and
// refuses to run a mismatching one (SEC-RPK-01).
func TestRailpackBinaryChecksum(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "providers")
	src := filepath.Join(dir, "src")
	for _, d := range []string{install, src} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	binary := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(install, "railpack"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "VERSION"), []byte("0.43.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "package.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, ok := installedRailpack(install); !ok || v != "v0.43.0" {
		t.Errorf("installed railpack = %q, %v", v, ok)
	}
	sum := sha256.Sum256(binary)
	good := "sha256:" + hex.EncodeToString(sum[:])
	p := &Provider{cfg: Config{RailpackDir: install}}
	for _, c := range []struct {
		want    string
		refused bool
	}{{"sha256:" + strings.Repeat("0", 64), true}, {good, false}} {
		work := t.TempDir()
		r := &railpackRun{p: p, e: &env{spec: isolation.Spec{SourceDir: src}}, rp: &buildplan.PlanRailpack{Context: ".", Binaries: map[string]string{runtime.GOARCH: c.want}}, dir: filepath.Join(work, "railpack")}
		bin, err := r.stage()
		if c.refused {
			if !errors.Is(err, isolation.ErrInfrastructure) || !strings.Contains(err.Error(), "is not the approved one") {
				t.Errorf("a mismatching binary = %v", err)
			}
			if _, serr := os.Stat(filepath.Join(r.dir, "bin", "railpack")); !errors.Is(serr, os.ErrNotExist) {
				t.Error("the mismatching copy was left in place")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(bin)
		if string(got) != string(binary) {
			t.Error("the staged binary is not the verified copy")
		}
		if _, err := os.Stat(filepath.Join(r.dir, "app", "package.json")); err != nil {
			t.Errorf("the context was not staged: %v", err)
		}
	}
	r := &railpackRun{p: p, e: &env{}, rp: &buildplan.PlanRailpack{Binaries: map[string]string{}}, dir: t.TempDir()}
	if _, err := r.stage(); !errors.Is(err, isolation.ErrUnsupported) {
		t.Errorf("no approved binary for this architecture = %v", err)
	}
}
