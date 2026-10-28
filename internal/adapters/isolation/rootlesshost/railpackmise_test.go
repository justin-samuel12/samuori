package rootlesshost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/adapters/build/buildkit"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/isolation"
)

// Railpack runs mise with only PATH set, so with strict egress planning works only through the staged, verified mise
// and its proxy wrapper (docs/spikes/e2e-providers.md).
func TestRailpackMiseStaging(t *testing.T) {
	dir := t.TempDir()
	install, src := filepath.Join(dir, "providers"), filepath.Join(dir, "src")
	for _, d := range []string{install, src} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sum := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
	binary := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(install, "railpack"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	mise := []byte("#!/bin/sh\necho \"$HTTPS_PROXY|$NO_PROXY|${0##*/}|$*\"\n")
	railpackMise["v9.9.9"] = struct {
		Version string
		SHA256  map[string]string
	}{"2099.1.1", map[string]string{runtime.GOARCH: sum(mise)}}
	t.Cleanup(func() { delete(railpackMise, "v9.9.9") })
	const proxy = "http://samuori:token@10.231.0.1:3128"
	run := func(proxy string) (*railpackRun, error) {
		r := &railpackRun{p: &Provider{cfg: Config{RailpackDir: install}}, e: &env{spec: isolation.Spec{SourceDir: src}}, proxy: proxy, noPx: []string{"10.231.0.1:5000"},
			rp: &buildplan.PlanRailpack{Version: "v9.9.9", Context: ".", Binaries: map[string]string{runtime.GOARCH: sum(binary)}}, dir: filepath.Join(t.TempDir(), "railpack")}
		_, err := r.stage()
		return r, err
	}
	staged := func(r *railpackRun, name string) string { return filepath.Join(r.dir, "mise", "mise", name) }

	if _, err := run(proxy); !errors.Is(err, isolation.ErrInfrastructure) || !strings.Contains(err.Error(), "no pinned mise") {
		t.Errorf("strict egress without a staged mise = %v", err)
	}
	if _, err := run(""); err != nil {
		t.Errorf("unrestricted egress without a staged mise = %v", err)
	}

	if err := os.WriteFile(filepath.Join(install, "mise"), append([]byte("tampered"), mise...), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := run(proxy)
	if !errors.Is(err, isolation.ErrInfrastructure) || !strings.Contains(err.Error(), "is not the one pinned") {
		t.Errorf("a mismatching mise = %v", err)
	}
	if _, err := os.Stat(staged(r, ".samuori/mise")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the mismatching mise was left in place")
	}

	if err := os.WriteFile(filepath.Join(install, "mise"), mise, 0o755); err != nil {
		t.Fatal(err)
	}
	if r, err = run(proxy); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(staged(r, "mise-2099.1.1"), "latest", "node@22")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != proxy+"|10.231.0.1:5000|mise|latest node@22" {
		t.Errorf("mise through the wrapper saw %q", got)
	}

	if r, err = run(""); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(staged(r, "mise-2099.1.1")); string(got) != string(mise) {
		t.Error("without a proxy the verified mise is not staged under Railpack's name")
	}
}

// sudo logs the planning command line to the journal, so the egress proxy credential reaches planning in a file.
func TestRailpackPlanningKeepsTheProxyOffTheCommandLine(t *testing.T) {
	const proxy = "http://samuori:secret-token@10.231.0.1:3128"
	r := &railpackRun{p: &Provider{opts: buildkit.Options{RunAs: []string{"sudo", "-u", "samuori-build"}}, cfg: Config{Tools: buildkit.Tools{Rootlesskit: "rootlesskit"}}},
		e: &env{}, proxy: proxy, noPx: []string{"10.231.0.1:5000"}, dir: t.TempDir()}
	argv := strings.Join(r.prepareArgv("/x/railpack", []string{"prepare", "app"}), " ")
	if strings.Contains(argv, "secret-token") {
		t.Errorf("the proxy credential is on the command line: %s", argv)
	}
	if !strings.Contains(argv, r.proxyEnvFile()) {
		t.Errorf("planning does not read the proxy file: %s", argv)
	}
	exports, err := proxyExports(proxy, r.noPx)
	if err != nil || !strings.Contains(exports, "export HTTPS_PROXY='"+proxy+"'") || !strings.Contains(exports, "export NO_PROXY='10.231.0.1:5000'") {
		t.Errorf("exports = %q, %v", exports, err)
	}
	if _, err := proxyExports("http://x'; rm -rf /", nil); err == nil {
		t.Error("a quote in the proxy URL was accepted")
	}
}
