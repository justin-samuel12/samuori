package rootlesshost

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/adapters/build/buildkit"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/engine"
	"samuori.invalid/samuori/internal/isolation"
)

const (
	cnbBuilder = "docker.io/paketobuildpacks/ubuntu-noble-builder:0.0.205@sha256:39dd864355d104e1f1b64a4238f6871bf0514ec3fc1427b13544c68452793dd9"
	cnbRun     = "docker.io/paketobuildpacks/ubuntu-noble-run:0.0.150@sha256:2b7308aea56815c1f376ab84cb1df5338797d480e32d899753e168cd367ab060"
)

func cnbPlan() *buildplan.PlanBuildpacks {
	return &buildplan.PlanBuildpacks{Version: "v0.21.26", PlatformAPI: buildplan.CNBPlatformAPI, Builder: cnbBuilder, RunImage: cnbRun, Context: ".",
		Env:      map[string]string{"BP_NODE_VERSION": "22.* && curl evil"},
		Bindings: []buildplan.PlanBinding{{Name: "npmrc", Type: "npmrc", Key: ".npmrc", SecretID: "npm-token"}}}
}

// TestBuildpacksDockerfile: the lifecycle runs from the approved builder pinned by digest, as its non-root CNB user,
// with secrets mounted by logical ID as binding files for the lifecycle step only; configuration values never enter
// the Dockerfile and the final stage holds only the exported layout, report and SBOM (SEC-CNB-01, SEC-CNB-02).
func TestBuildpacksDockerfile(t *testing.T) {
	df, err := BuildpacksDockerfile(cnbPlan())
	if err != nil {
		t.Fatal(err)
	}
	text := string(df)
	for _, want := range []string{
		"FROM " + cnbBuilder + " AS samuori-build\n",
		"ENV CNB_PLATFORM_API=0.14 CNB_EXPERIMENTAL_MODE=warn\n",
		"COPY --from=samuorirun --chown=${CNB_USER_ID}:${CNB_GROUP_ID} / /layout-repo/index.docker.io/paketobuildpacks/ubuntu-noble-run/sha256/2b7308aea56815c1f376ab84cb1df5338797d480e32d899753e168cd367ab060/\n",
		"USER ${CNB_USER_ID}:${CNB_GROUP_ID}\n",
		`"$(id -u)\" -ne 0`,
		"--mount=type=secret,id=npm-token,target=/platform/bindings/npmrc/.npmrc,uid=${CNB_USER_ID},gid=${CNB_GROUP_ID},mode=0400,required=true",
		`"-run-image=docker.io/paketobuildpacks/ubuntu-noble-run@sha256:2b7308aea56815c1f376ab84cb1df5338797d480e32d899753e168cd367ab060"`,
		"FROM scratch\nCOPY --from=samuori-build /layout-repo/samuori.invalid/app/build/ /layout/\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the Dockerfile lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "curl evil") || strings.Contains(text, "22.*") {
		t.Error("a configuration value reached the Dockerfile text")
	}
	if strings.Count(text, "--mount=type=secret") != 1 || strings.Contains(text, "type=cache") {
		t.Errorf("unexpected mounts:\n%s", text)
	}

	for name, mutate := range map[string]func(*buildplan.PlanBuildpacks){
		"a builder pinned by tag": func(b *buildplan.PlanBuildpacks) {
			b.Builder = "docker.io/paketobuildpacks/ubuntu-noble-builder:latest"
		},
		"a run image without digest": func(b *buildplan.PlanBuildpacks) { b.RunImage = "docker.io/paketobuildpacks/ubuntu-noble-run" },
		"an injected builder":        func(b *buildplan.PlanBuildpacks) { b.Builder = cnbBuilder + "\nRUN id" },
		"a binding path escape":      func(b *buildplan.PlanBuildpacks) { b.Bindings[0].Key = "../../etc/passwd" },
		"a binding over its type":    func(b *buildplan.PlanBuildpacks) { b.Bindings[0].Key = "type" },
		"a mount option injection":   func(b *buildplan.PlanBuildpacks) { b.Bindings[0].SecretID = "npm-token,target=/x" },
		"a CNB variable":             func(b *buildplan.PlanBuildpacks) { b.Env = map[string]string{"CNB_USER_ID": "0"} },
		"a context outside":          func(b *buildplan.PlanBuildpacks) { b.Context = "../x" },
	} {
		b := cnbPlan()
		mutate(b)
		if _, err := BuildpacksDockerfile(b); err == nil {
			t.Errorf("%s was rendered", name)
		}
	}
}

func writeBlob(t *testing.T, dir string, raw []byte) string {
	t.Helper()
	sum := sha256.Sum256(raw)
	h := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", h), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + h
}

// cnbLayout writes an OCI layout shaped like the lifecycle's export, with one layer holding content.
func cnbLayout(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	_ = tw.WriteHeader(&tar.Header{Name: "layers/app/config", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	ld := writeBlob(t, dir, layer.Bytes())
	cfg, _ := json.Marshal(map[string]any{"os": "linux", "architecture": "arm64", "config": map[string]any{"User": "1002:1001", "Entrypoint": []string{"/cnb/process/web"},
		"Labels": map[string]string{"io.buildpacks.build.metadata": `{"buildpacks":[{"id":"paketo-buildpacks/node-engine","version":"8.11.0"}],"processes":[{"type":"web","command":["node"],"args":["server.js"]}]}`}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{ld}}})
	cd := writeBlob(t, dir, cfg)
	man, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(cfg)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": ld, "size": layer.Len()}}})
	md := writeBlob(t, dir, man)
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": md, "size": len(man)}}})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), idx, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestBuildpacksLayout: the worker packs only a complete, digest-consistent single-image layout, the result goes
// through the same canary scan as every method, and the build metadata comes from the image's own labels
// (SEC-CNB-02, SEC-CNB-03).
func TestBuildpacksLayout(t *testing.T) {
	dir := cnbLayout(t, "token=canary-value-1234567890")
	archive := filepath.Join(t.TempDir(), "image.tar")
	if err := PackLayout(dir, archive); err != nil {
		t.Fatal(err)
	}
	if c := canary(archive, map[string][]byte{"npm-token": []byte("canary-value-1234567890")}); c.Outcome != "blocked" {
		t.Errorf("a secret in a buildpacks layer was not found: %+v", c)
	}
	if c := canary(archive, map[string][]byte{"egress-proxy": []byte("http://u:p@10.0.0.1:3128")}); c.Outcome != "passed" {
		t.Errorf("canary on a clean image = %+v", c)
	}
	meta, err := BuildpacksMetadata(archive, cnbPlan(), t.TempDir())
	if err != nil || !strings.Contains(string(meta), `"processes":[{"type":"web"`) || !strings.Contains(string(meta), `"user":"1002:1001"`) || !strings.Contains(string(meta), cnbBuilder) {
		t.Errorf("metadata = %s, %v", meta, err)
	}

	for name, mutate := range map[string]func(dir string){
		"a tampered blob": func(dir string) {
			entries, _ := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
			for _, e := range entries {
				p := filepath.Join(dir, "blobs", "sha256", e.Name())
				raw, _ := os.ReadFile(p)
				if bytes.Contains(raw, []byte("canary")) {
					_ = os.WriteFile(p, bytes.Replace(raw, []byte("canary"), []byte("CANARY"), 1), 0o600)
				}
			}
		},
		"an extra file": func(dir string) { _ = os.WriteFile(filepath.Join(dir, "blobs", "sha256", "notes"), []byte("x"), 0o600) },
		"a symlinked blob": func(dir string) {
			_ = os.Symlink("/etc/passwd", filepath.Join(dir, "blobs", "sha256", strings.Repeat("e", 64)))
		},
		"a missing layer": func(dir string) {
			entries, _ := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
			for _, e := range entries {
				raw, _ := os.ReadFile(filepath.Join(dir, "blobs", "sha256", e.Name()))
				if bytes.Contains(raw, []byte("canary")) {
					_ = os.Remove(filepath.Join(dir, "blobs", "sha256", e.Name()))
				}
			}
		},
		"two images": func(dir string) {
			raw, _ := os.ReadFile(filepath.Join(dir, "index.json"))
			var idx map[string][]any
			_ = json.Unmarshal(raw, &idx)
			idx["manifests"] = append(idx["manifests"], idx["manifests"][0])
			raw, _ = json.Marshal(idx)
			_ = os.WriteFile(filepath.Join(dir, "index.json"), raw, 0o600)
		},
	} {
		d := cnbLayout(t, "token=canary-value-1234567890")
		mutate(d)
		if err := PackLayout(d, filepath.Join(t.TempDir(), "image.tar")); err == nil {
			t.Errorf("%s was packed", name)
		}
	}
}

// TestBuildpacksCapability: a BuildKit worker offers buildpacks with nothing installed, reporting the CNB platform API
// it drives; a Buildah worker does not and refuses to run the method (SEC-CNB-04).
func TestBuildpacksCapability(t *testing.T) {
	for eng, want := range map[string]string{engine.BuildKit: buildplan.CNBPlatformAPI, engine.Buildah: ""} {
		p, err := NewFromConfig(Config{Dir: t.TempDir(), Tools: buildkit.DefaultTools(), UnrestrictedEgress: true, Engine: eng, RailpackDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if got := p.providers()["buildpacks"]; got != want {
			t.Errorf("%s worker reports buildpacks %q, want %q", eng, got, want)
		}
		if eng == engine.Buildah {
			if _, _, err := p.buildpacks(context.Background(), &env{}, buildplan.Plan{Buildpacks: cnbPlan()}, &buildkit.Request{}, t.TempDir()); err == nil {
				t.Error("a Buildah worker accepted a buildpacks build")
			}
		}
	}
}

type pullRunner struct {
	engine.Runner
	t    *testing.T
	reqs []engine.Request
}

func (r *pullRunner) Build(_ context.Context, req engine.Request) (engine.Result, error) {
	r.reqs = append(r.reqs, req)
	return engine.Result{}, PackLayout(cnbLayout(r.t, "run image"), req.OutputTar)
}

// TestBuildpacksRequest: the run image is pulled by digest without secrets, non-secret configuration and binding
// types are the only files in the platform context, and the lifecycle solve keeps no shared cache whatever the
// plan's cache mode (SEC-CNB-02, SEC-CNB-07).
func TestBuildpacksRequest(t *testing.T) {
	p, err := NewFromConfig(Config{Dir: t.TempDir(), Tools: buildkit.DefaultTools(), UnrestrictedEgress: true, RailpackDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	run := &pullRunner{t: t}
	e := &env{runner: run, spec: isolation.Spec{SourceDir: t.TempDir()}}
	plan := buildplan.Plan{Method: "buildpacks", Platforms: []string{"linux/arm64"}, Buildpacks: cnbPlan(), Cache: buildplan.PlanCache{Mode: "scoped", Scope: "cs-project-a"}}
	req := buildkit.Request{CacheImportDir: "/cache/in", CacheExportDir: "/cache/out", BuildArgs: map[string]string{"X": "y"},
		Secrets: func(context.Context, string) ([]byte, error) { return []byte("never-read"), nil }}
	out := t.TempDir()
	if _, _, err := p.buildpacks(context.Background(), e, plan, &req, out); err != nil {
		t.Fatal(err)
	}
	if len(run.reqs) != 1 || run.reqs[0].Secrets != nil || run.reqs[0].CacheImportDir != "" || !strings.Contains(readFile(t, filepath.Join(run.reqs[0].DockerfileDir, "Dockerfile")), "FROM "+cnbRun) {
		t.Errorf("run image pull = %+v", run.reqs)
	}
	if req.CacheImportDir != "" || req.CacheExportDir != "" || req.BuildArgs != nil || req.OutputDir == "" {
		t.Errorf("lifecycle request = %+v", req)
	}
	platform := req.NamedContexts["samuoriplatform"]
	if got := readFile(t, filepath.Join(platform, "env", "BP_NODE_VERSION")); got != "22.* && curl evil" {
		t.Errorf("env file = %q", got)
	}
	if got := readFile(t, filepath.Join(platform, "bindings", "npmrc", "type")); got != "npmrc" {
		t.Errorf("binding type = %q", got)
	}
	_ = filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && strings.Contains(readFile(t, p), "never-read") {
			t.Errorf("%s holds a secret value", p)
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(req.NamedContexts["samuorirun"], "index.json")); err != nil {
		t.Errorf("the run image layout was not unpacked: %v", err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
