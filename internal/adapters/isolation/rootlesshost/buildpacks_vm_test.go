//go:build linux

package rootlesshost

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/workeragent"
)

// TestBuildpacksWorkerPathVM runs the CNB lifecycle from a real Paketo builder through Provider.Run. It needs rootless
// BuildKit and network access, so it runs only in the development VM with SAMUORI_CNB_E2E=1
// (docs/spikes/buildpacks-builds.md).
func TestBuildpacksWorkerPathVM(t *testing.T) {
	if os.Getenv("SAMUORI_CNB_E2E") != "1" {
		t.Skip("set SAMUORI_CNB_E2E=1 in the development VM")
	}
	env := func(k string) string { return os.Getenv("SAMUORI_E2E_" + k) }
	settings := map[string]string{"dir": env("DIR") + "/env", "daemon-dir": env("DIR") + "/daemons", "build-user": env("BUILD_USER"), "build-group": env("BUILD_GROUP"),
		"egress-listen": env("EGRESS_LISTEN"), "cgroup-limits": env("CGROUP")}
	if settings["egress-listen"] == "" {
		settings["egress"] = "unrestricted"
	}
	prov, err := fromSettings(settings, isolation.RootlessHost)
	if err != nil {
		t.Fatal(err)
	}
	p := prov.(*Provider)
	caps, _ := p.Capabilities(context.Background())
	t.Logf("capabilities: engines=%v providers=%v", caps.Engines, caps.Providers)
	if caps.Providers["buildpacks"] != buildplan.CNBPlatformAPI {
		t.Fatal("the worker does not report buildpacks")
	}
	agent := workeragent.New(workeragent.Config{StateDir: t.TempDir(), ProvidersDir: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agent.UseProvider(p)
	wire := agent.Capabilities(context.Background())
	t.Logf("worker protocol capabilities: engines=%v profiles=%v providers=%v", wire.GetEngines(), wire.GetIsolationProfiles(), wire.GetProviders())
	if wire.GetProviders()["buildpacks"] != buildplan.CNBPlatformAPI {
		t.Fatal("the worker agent does not report buildpacks to the controller")
	}
	var allow []buildplan.EgressRule
	for _, d := range strings.Split(env("ALLOW"), ",") {
		host, port, _ := strings.Cut(d, ":")
		n, _ := strconv.Atoi(port)
		if host != "" {
			allow = append(allow, buildplan.EgressRule{Host: host, Port: n})
		}
	}
	plan := buildplan.Plan{Version: 1, Method: "buildpacks", Platforms: []string{"linux/" + runtime.GOARCH}, IsolationProfile: string(isolation.RootlessHost),
		Limits: buildplan.DefaultLimits, Cache: buildplan.PlanCache{Mode: "none"}, Egress: buildplan.PlanEgress{Allow: allow},
		Secrets: []buildplan.PlanSecret{{ID: "npm-token", Required: true, BindingID: "b1", BindingVersion: 1}},
		Buildpacks: &buildplan.PlanBuildpacks{Version: "v0.21.26", PlatformAPI: buildplan.CNBPlatformAPI, Builder: env("BUILDER"), RunImage: env("RUN_IMAGE"), Context: ".",
			Env:      map[string]string{"BP_NODE_VERSION": "22.*"},
			Bindings: []buildplan.PlanBinding{{Name: "npmrc", Type: "npmrc", Key: ".npmrc", SecretID: "npm-token"}}, TimeoutSeconds: 1800}}
	raw, digest := plan.Encode()
	t.Logf("plan digest %s", digest)
	out := filepath.Join(env("DIR"), "out")
	_ = os.RemoveAll(out)
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	canaryValue := "e2e-canary-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	npmrc := "//registry.npmjs.org/:_authToken=" + canaryValue + "\n"
	spec := isolation.Spec{AttemptID: "E2E" + strconv.FormatInt(time.Now().Unix(), 10), SourceDir: env("SOURCE"), OutputDir: out, Profile: isolation.RootlessHost,
		Limits: isolation.Limits{WallClock: 30 * time.Minute}, ReleaseSecret: func(_ context.Context, id string) ([]byte, error) {
			if id != "npm-token" {
				return nil, errors.New("unexpected secret " + id)
			}
			return []byte(npmrc), nil
		}}
	ctx := context.Background()
	e, err := p.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	res, runErr := p.Run(ctx, e, raw)
	t.Logf("run took %s", time.Since(started).Round(time.Second))
	for _, r := range res.Records {
		if b, err := os.ReadFile(r.File); err == nil {
			_ = os.WriteFile(filepath.Join(env("DIR"), filepath.Base(r.File)), b, 0o600)
		}
	}
	if res.ArtifactFile != "" {
		_ = copyFile(res.ArtifactFile, filepath.Join(env("DIR"), "image.tar"), 0o600)
	}
	if derr := p.Destroy(ctx, e); derr != nil {
		t.Errorf("destroy: %v", derr)
	}
	logs := string(res.Logs)
	_ = os.WriteFile(filepath.Join(env("DIR"), "run.log"), res.Logs, 0o600)
	if runErr != nil {
		t.Fatalf("run: %v\n%s", runErr, tailOf(logs, 4000))
	}
	if strings.Contains(logs, canaryValue) {
		t.Error("the secret value reached the log")
	}
	t.Logf("image %s", res.ArtifactDigest)
	for _, r := range res.Records {
		t.Logf("record %s %s", r.Kind, r.Digest)
	}
	canaried := false
	for _, c := range res.Checks {
		t.Logf("check %s: %s %s", c.ID, c.Outcome, c.Reason)
		if c.ID == isolation.CheckCanary {
			canaried = true
			if c.Outcome != "passed" {
				t.Errorf("canary = %s %s", c.Outcome, c.Reason)
			}
		}
	}
	if !canaried {
		t.Error("no canary scan ran")
	}
	if len(res.Records) != 2 {
		t.Errorf("lineage records = %+v", res.Records)
	}
	cfg := imageConfig(t, filepath.Join(env("DIR"), "image.tar"))
	t.Logf("image config: user=%q env=%v cmd=%v entrypoint=%v", cfg.Config.User, cfg.Config.Env, cfg.Config.Cmd, cfg.Config.Entrypoint)
	if u, _, _ := strings.Cut(cfg.Config.User, ":"); u == "" || u == "0" || u == "root" {
		t.Errorf("the image runs as %q", cfg.Config.User)
	}
	for _, v := range cfg.Config.Env {
		if strings.Contains(strings.ToLower(v), "proxy") || strings.Contains(v, canaryValue) {
			t.Errorf("the image configuration carries %s", v)
		}
	}
	if n := countLayers(t, filepath.Join(env("DIR"), "image.tar"), ".npmrc"); n != 0 {
		t.Errorf("%d non-empty layer entries are named .npmrc", n)
	}
	t.Logf("egress and log tail:\n%s", tailOf(logs, 2500))
}

// countLayers counts non-empty layer entries whose path ends in name, across every gzip or plain layer blob.
func countLayers(t *testing.T, archive, name string) int {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(h.Name, "blobs/") {
			n += layerEntries(tr, name)
		}
	}
}

func layerEntries(r io.Reader, name string) int {
	raw, err := io.ReadAll(r)
	if err != nil {
		return 0
	}
	var lr io.Reader = bytes.NewReader(raw)
	if gz, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
		lr = gz
	}
	n := 0
	tr := tar.NewReader(lr)
	for {
		h, err := tr.Next()
		if err != nil {
			return n
		}
		if (strings.HasSuffix(h.Name, "/"+name) || h.Name == name) && h.Size > 0 {
			n++
		}
	}
}
