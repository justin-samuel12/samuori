//go:build linux

package rootlesshost

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/isolation"
)

// TestRailpackWorkerPathVM runs a real Railpack plan and BuildKit gateway build through Provider.Run. It needs
// rootless BuildKit, an installed Railpack and network access, so it runs only in the development VM with
// SAMUORI_RAILPACK_E2E=1 (docs/spikes/railpack-builds.md).
func TestRailpackWorkerPathVM(t *testing.T) {
	if os.Getenv("SAMUORI_RAILPACK_E2E") != "1" {
		t.Skip("set SAMUORI_RAILPACK_E2E=1 in the development VM")
	}
	env := func(k string) string { return os.Getenv("SAMUORI_E2E_" + k) }
	settings := map[string]string{"dir": env("DIR") + "/env", "daemon-dir": env("DIR") + "/daemons", "build-user": env("BUILD_USER"), "build-group": env("BUILD_GROUP"),
		"egress-listen": env("EGRESS_LISTEN"), "cgroup-limits": env("CGROUP"), "railpack-dir": env("RAILPACK_DIR")}
	if settings["egress-listen"] == "" {
		settings["egress"] = "unrestricted"
	}
	prov, err := fromSettings(settings, isolation.RootlessHost)
	if err != nil {
		t.Fatal(err)
	}
	p := prov.(*Provider)
	caps, _ := p.Capabilities(context.Background())
	t.Logf("capabilities: providers=%v config=%s", caps.Providers, caps.ConfigDigest)
	if caps.Providers["railpack"] == "" {
		t.Fatal("the worker does not report the installed Railpack")
	}
	var allow []buildplan.EgressRule
	for _, d := range strings.Split(env("ALLOW"), ",") {
		host, port, _ := strings.Cut(d, ":")
		n, _ := strconv.Atoi(port)
		if host != "" {
			allow = append(allow, buildplan.EgressRule{Host: host, Port: n})
		}
	}
	plan := buildplan.Plan{Version: 1, Method: "railpack", Platforms: []string{"linux/" + runtime.GOARCH}, IsolationProfile: string(isolation.RootlessHost),
		Limits: buildplan.DefaultLimits, Cache: buildplan.PlanCache{Mode: "none"}, Egress: buildplan.PlanEgress{Allow: allow},
		Secrets: []buildplan.PlanSecret{{ID: "npm-token", Required: true, Target: "/run/secrets/npm-token", BindingID: "b1", BindingVersion: 1}},
		Railpack: &buildplan.PlanRailpack{Version: caps.Providers["railpack"], Binaries: map[string]string{runtime.GOARCH: env("RAILPACK_SHA256")}, Frontend: env("FRONTEND"),
			Images: strings.Split(env("IMAGES"), ","), Context: ".", CacheKey: "cs-e2e-project", SecretEnv: map[string]string{"NPM_TOKEN": "npm-token"}, TimeoutSeconds: 600,
			BuildCommand: "printenv NPM_TOKEN >/dev/null && echo npm-token-present"}}
	raw, digest := plan.Encode()
	t.Logf("plan digest %s", digest)
	out := filepath.Join(env("DIR"), "out")
	_ = os.RemoveAll(out)
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	canaryValue := "npm_e2e-canary-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	spec := isolation.Spec{AttemptID: "E2E" + strconv.FormatInt(time.Now().Unix(), 10), SourceDir: env("SOURCE"), OutputDir: out, Profile: isolation.RootlessHost,
		Limits: isolation.Limits{WallClock: 30 * time.Minute}, ReleaseSecret: func(_ context.Context, id string) ([]byte, error) {
			if id != "npm-token" {
				return nil, errors.New("unexpected secret " + id)
			}
			return []byte(canaryValue), nil
		}}
	ctx := context.Background()
	e, err := p.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	res, runErr := p.Run(ctx, e, raw)
	t.Logf("run took %s", time.Since(started).Round(time.Second))
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
	if !strings.Contains(logs, "npm-token-present") {
		t.Error("the build command did not receive NPM_TOKEN")
	}
	t.Logf("image %s, records %+v", res.ArtifactDigest, res.Records)
	for _, c := range res.Checks {
		t.Logf("check %s: %s %s", c.ID, c.Outcome, c.Reason)
		if c.ID == isolation.CheckCanary && c.Outcome != "passed" {
			t.Errorf("canary = %s %s", c.Outcome, c.Reason)
		}
	}
	if len(res.Records) != 2 {
		t.Errorf("lineage records = %+v", res.Records)
	}
	cfg := imageConfig(t, res.ArtifactFile)
	t.Logf("image config: user=%q env=%v cmd=%v entrypoint=%v", cfg.Config.User, cfg.Config.Env, cfg.Config.Cmd, cfg.Config.Entrypoint)
	for _, v := range cfg.Config.Env {
		if strings.Contains(strings.ToLower(v), "proxy") || strings.Contains(v, canaryValue) {
			t.Errorf("the image configuration carries %s", v)
		}
	}
	t.Logf("egress and log tail:\n%s", tailOf(logs, 1500))
}

type ociConfig struct {
	Config struct {
		User       string   `json:"User"`
		Env        []string `json:"Env"`
		Cmd        []string `json:"Cmd"`
		Entrypoint []string `json:"Entrypoint"`
	} `json:"config"`
}

// imageConfig reads the single image's configuration from an OCI archive.
func imageConfig(t *testing.T, archive string) ociConfig {
	t.Helper()
	blobs := map[string][]byte{}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Size < 1<<20 {
			b, _ := io.ReadAll(tr)
			blobs[h.Name] = b
		}
	}
	var index struct {
		Manifests []struct{ Digest string } `json:"manifests"`
	}
	_ = json.Unmarshal(blobs["index.json"], &index)
	blob := func(d string) []byte { return blobs["blobs/sha256/"+strings.TrimPrefix(d, "sha256:")] }
	var m struct {
		Config    struct{ Digest string }   `json:"config"`
		Manifests []struct{ Digest string } `json:"manifests"`
	}
	_ = json.Unmarshal(blob(index.Manifests[0].Digest), &m)
	if len(m.Manifests) > 0 {
		_ = json.Unmarshal(blob(m.Manifests[0].Digest), &m)
	}
	var c ociConfig
	if err := json.Unmarshal(blob(m.Config.Digest), &c); err != nil {
		t.Fatalf("image config: %v", err)
	}
	return c
}

func tailOf(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
