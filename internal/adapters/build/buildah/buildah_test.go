package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/engine"
)

// fake records each invocation's arguments and environment, one per line, and writes an archive where asked.
const fake = `#!/bin/sh
log="$(dirname "$0")/calls.log"
{ printf 'ARGS'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'; env | sort | sed 's/^/ENV /'; } >> "$log"
prev=""
for a in "$@"; do
  case "$prev" in --tag) echo archive > "${a#oci-archive:}" ;; esac
  case "$a" in type=local,dest=*) mkdir -p "${a#type=local,dest=}" && echo '{"outcome":"passed"}' > "${a#type=local,dest=}/result.json" ;; esac
  prev="$a"
done
case " $* " in *" pull "*) echo 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef ;; esac
exit ${FAKE_EXIT:-0}
`

func setup(t *testing.T) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "buildah")
	if err := os.WriteFile(bin, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls.log")
	r, err := Start(filepath.Join(dir, "build"), Options{Binary: bin, Mirrors: map[string]string{"docker.io": "10.0.0.5:5000"}, InsecureMirrors: true})
	if err != nil {
		t.Fatal(err)
	}
	return r, log
}

func calls(t *testing.T, log string) []string {
	raw, _ := os.ReadFile(log)
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "ARGS") {
			out = append(out, l)
		}
	}
	return out
}

func TestBuild(t *testing.T) {
	r, log := setup(t)
	ctxDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte("FROM scratch\n"), 0o600)
	out := filepath.Join(t.TempDir(), "image.tar")
	_, err := r.Build(context.Background(), engine.Request{ContextDir: ctxDir, DockerfileDir: ctxDir, Filename: "Dockerfile", Target: "final",
		BuildArgs: map[string]string{"VERSION": "1; rm -rf /", "A": "$(id)"}, Platforms: []string{"linux/arm64"}, OutputTar: out,
		Proxy: "http://10.0.0.1:3128/s/tok", NoProxy: []string{"10.0.0.5:5000"},
		Labels: map[string]string{"com.example.b": "x y", "com.example.a": "1"}, Annotations: map[string]string{"org.opencontainers.image.created": "2026-10-26T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "archive\n" {
		t.Fatalf("archive = %q", b)
	}
	c := calls(t, log)
	if len(c) != 1 {
		t.Fatalf("calls = %v", c)
	}
	for _, want := range []string{"[build] [--isolation] [oci] [--network] [private]", "[--pull=always] [--layers=false] [--http-proxy=true]",
		"[--target] [final]", "[--platform] [linux/arm64]", "[--build-arg] [A=$(id)] [--build-arg] [VERSION=1; rm -rf /]", "[--] [" + r.path("context") + "]", "[--file] [" + r.path("context/Dockerfile") + "]", "[--storage-driver] [overlay]",
		"[--label] [com.example.a=1] [--label] [com.example.b=x y] [--annotation] [org.opencontainers.image.created=2026-10-26T00:00:00Z]"} {
		if !strings.Contains(c[0], want) {
			t.Errorf("missing %q in %s", want, c[0])
		}
	}
	raw, _ := os.ReadFile(log)
	env := string(raw)
	if !strings.Contains(env, "ENV HTTP_PROXY=http://10.0.0.1:3128/s/tok") || !strings.Contains(env, "ENV NO_PROXY=10.0.0.5:5000") {
		t.Fatalf("proxy not in the environment:\n%s", env)
	}
	if strings.Contains(c[0], "HTTP_PROXY") {
		t.Fatal("the proxy became a build argument")
	}
	reg, _ := os.ReadFile(filepath.Join(r.dir, "registries.conf"))
	if !strings.Contains(string(reg), `short-name-mode = "disabled"`) || !strings.Contains(string(reg), `location = "10.0.0.5:5000"`) {
		t.Fatalf("registries.conf =\n%s", reg)
	}
	conf, _ := os.ReadFile(filepath.Join(r.dir, "containers.conf"))
	if !strings.Contains(string(conf), `cgroup_manager = "cgroupfs"`) || !strings.Contains(string(conf), `runtime = "runc"`) {
		t.Fatalf("containers.conf =\n%s", conf)
	}

	t.Run("a failed build is a build error", func(t *testing.T) {
		_ = os.WriteFile(r.opts.Binary, []byte(strings.Replace(fake, "exit ${FAKE_EXIT:-0}", "exit 1", 1)), 0o755)
		if _, err := r.Build(context.Background(), engine.Request{ContextDir: ctxDir, DockerfileDir: ctxDir, Filename: "Dockerfile", OutputTar: out}); !errors.Is(err, engine.ErrBuild) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMirrorsMustBeHosts(t *testing.T) {
	for _, m := range []string{"10.0.0.5:5000\"\n[[registry]]", "evil host", "http://10.0.0.5"} {
		if _, err := Start(t.TempDir(), Options{Mirrors: map[string]string{"docker.io": m}}); err == nil {
			t.Errorf("mirror %q accepted", m)
		}
	}
}

func TestScopeArguments(t *testing.T) {
	r := &Runner{opts: Options{Binary: "buildah", RunAs: []string{"sudo", "-n", "-u", "samuori-build", "--"}, UserRuntime: "/run/user/999", Scope: &Scope{Unit: "samuori-bh-x", MemoryBytes: 1 << 30, Tasks: 512}}, dir: "/b"}
	argv := strings.Join(r.command("build"), " ")
	for _, want := range []string{"sudo -n -u samuori-build -- env XDG_RUNTIME_DIR=/run/user/999", "systemd-run --user --scope --quiet --unit=samuori-bh-x", "MemoryMax=1073741824", "MemorySwapMax=0", "TasksMax=512",
		"/sys/fs/cgroup/user.slice/user-999.slice/user@999.service/app.slice/samuori-bh-x.scope/buildah", "samuori-bh-x.scope/memory.events /b/out/memory.events", "env -i HOME=/b/run", "buildah --root /b/root --runroot /b/run --storage-driver overlay build"} {
		if !strings.Contains(argv, want) {
			t.Errorf("missing %q in %s", want, argv)
		}
	}
}

func TestRuntimeTest(t *testing.T) {
	r, log := setup(t)
	layout := t.TempDir()
	manifest := "sha256:" + strings.Repeat("a", 64)
	_ = os.MkdirAll(filepath.Join(layout, "blobs", "sha256"), 0o700)
	_ = os.WriteFile(filepath.Join(layout, "blobs", "sha256", strings.Repeat("a", 64)), []byte(`{"schemaVersion":2}`), 0o600)
	df, probe, result := t.TempDir(), t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(df, "Dockerfile"), []byte("FROM samuoriimage\n"), 0o600)
	if _, err := r.Test(context.Background(), engine.TestRequest{DockerfileDir: df, Layout: layout, Manifest: manifest, ProbeDir: probe, OutputDir: result, Platform: "linux/arm64"}); err != nil {
		t.Fatal(err)
	}
	c := calls(t, log)
	if len(c) != 2 || !strings.Contains(c[0], "[pull] [--quiet] [--policy] [always] [oci:"+r.path("test-layout")+":samuoriimage]") {
		t.Fatalf("calls = %v", c)
	}
	for _, want := range []string{"[--network] [none]", "[--pull=never]", "[samuoriimage=container-image://0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef]", "[samuoriprobe=" + r.path("test-probe") + "]", "[--platform] [linux/arm64]"} {
		if !strings.Contains(c[1], want) {
			t.Errorf("missing %q in %s", want, c[1])
		}
	}
	var idx struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	raw, _ := os.ReadFile(filepath.Join(r.path("test-layout"), "index.json"))
	if err := json.Unmarshal(raw, &idx); err != nil || len(idx.Manifests) != 1 || idx.Manifests[0].Digest != manifest || idx.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != "samuoriimage" {
		t.Fatalf("index = %s", raw)
	}
	if b, err := os.ReadFile(filepath.Join(result, "result.json")); err != nil || !strings.Contains(string(b), "passed") {
		t.Fatalf("result = %q %v", b, err)
	}
	if err := (&Runner{gid: -1, dir: t.TempDir()}).copyLayout(layout, filepath.Join(t.TempDir(), "x"), "sha256:../../etc"); err == nil {
		t.Fatal("a crafted manifest digest was accepted")
	}
}

// TestPlantedPathsStayInside replaces the build's outputs and staging paths with links to the worker's own files, as
// a process running as the build user could. The worker must neither read those files nor write outside the build.
func TestPlantedPathsStayInside(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "worker-credentials")
	if err := os.WriteFile(secret, []byte("worker-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	ctxDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte("FROM scratch\n"), 0o600)
	plant := func(t *testing.T, r *Runner, script string) {
		body := strings.Replace(fake, "exit ${FAKE_EXIT:-0}", script+"\nexit 0", 1)
		if err := os.WriteFile(r.opts.Binary, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	leaked := func(t *testing.T, path string) {
		t.Helper()
		if b, _ := os.ReadFile(path); strings.Contains(string(b), "worker-only") {
			t.Fatalf("%s holds the worker's file", path)
		}
	}

	t.Run("a symbolic link as the image", func(t *testing.T) {
		r, _ := setup(t)
		plant(t, r, `rm -f "`+r.path("out/image.tar")+`"; ln -s "`+secret+`" "`+r.path("out/image.tar")+`"`)
		out := filepath.Join(t.TempDir(), "image.tar")
		if _, err := r.Build(context.Background(), engine.Request{ContextDir: ctxDir, DockerfileDir: ctxDir, Filename: "Dockerfile", OutputTar: out}); err == nil {
			t.Fatal("the build succeeded")
		}
		leaked(t, out)
	})

	t.Run("a hard link to a worker file as the image", func(t *testing.T) {
		r, _ := setup(t)
		r.opts.RunAs = []string{"env"}
		plant(t, r, `rm -f "`+r.path("out/image.tar")+`"; ln "`+secret+`" "`+r.path("out/image.tar")+`" || exit 3`)
		out := filepath.Join(t.TempDir(), "image.tar")
		if _, err := r.Build(context.Background(), engine.Request{ContextDir: ctxDir, DockerfileDir: ctxDir, Filename: "Dockerfile", OutputTar: out}); !errors.Is(err, ErrUntrustedOutput) {
			t.Fatalf("err = %v", err)
		}
		leaked(t, out)
	})

	t.Run("links in the test result", func(t *testing.T) {
		r, _ := setup(t)
		res := r.path("test-result")
		plant(t, r, `case " $* " in *" build "*) ln -s "`+secret+`" "`+res+`/leak.json"; ln -s "`+outside+`" "`+res+`/dir" ;; esac`)
		layout := t.TempDir()
		manifest := writeLayout(t, layout)
		got := t.TempDir()
		if _, err := r.Test(context.Background(), engine.TestRequest{Layout: layout, Manifest: manifest, DockerfileDir: ctxDir, ProbeDir: t.TempDir(), OutputDir: got}); err != nil {
			t.Fatal(err)
		}
		leaked(t, filepath.Join(got, "leak.json"))
		if _, err := os.Lstat(filepath.Join(got, "dir")); err == nil {
			t.Fatal("a linked directory was copied")
		}
	})

	t.Run("a staging directory replaced by a link", func(t *testing.T) {
		r, _ := setup(t)
		if err := os.Symlink(outside, r.path("test-layout")); err != nil {
			t.Fatal(err)
		}
		layout := t.TempDir()
		manifest := writeLayout(t, layout)
		_, err := r.Test(context.Background(), engine.TestRequest{Layout: layout, Manifest: manifest, DockerfileDir: ctxDir, ProbeDir: t.TempDir(), OutputDir: t.TempDir()})
		if err == nil {
			t.Fatal("staging followed the link")
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("the worker wrote outside the build: %v", entries)
		}
	})
}

func writeLayout(t *testing.T, dir string) string {
	t.Helper()
	_ = os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "blobs", "sha256", strings.Repeat("a", 64)), []byte(`{"schemaVersion":2}`), 0o600)
	return "sha256:" + strings.Repeat("a", 64)
}
