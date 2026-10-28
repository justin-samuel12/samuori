package toolrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestToolGetsAnEmptyEnvironment (SEC-K8S-02): the tool sees only HOME, TMPDIR and an unusable PATH, never the
// worker's environment, and its output and result are captured even when it fails.
func TestToolGetsAnEmptyEnvironment(t *testing.T) {
	env, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env binary")
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "worker-credential")
	dir := t.TempDir()
	out, work := filepath.Join(dir, "out"), filepath.Join(dir, "work")
	res := execute(Spec{Args: []string{env}, Mkdirs: []string{"manifests"}, TimeoutSeconds: 10}, out, work, false)
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	r, stdout, _, err := ReadMeta(out)
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("meta = %+v %v", r, err)
	}
	vars := strings.Fields(string(stdout))
	sort.Strings(vars)
	want := []string{"HOME=" + filepath.Join(work, "tmp"), "PATH=/nonexistent", "TMPDIR=" + filepath.Join(work, "tmp")}
	if strings.Join(vars, " ") != strings.Join(want, " ") {
		t.Errorf("tool environment = %v", vars)
	}
	if _, err := os.Stat(filepath.Join(out, "manifests")); err != nil {
		t.Error("output directory not created")
	}

	res = execute(Spec{Args: []string{"/bin/sh", "-c", "exec /bin/sleep 5"}, TimeoutSeconds: 1}, filepath.Join(dir, "out2"), work, false)
	if !res.TimedOut {
		t.Errorf("a tool past its limit was not stopped: %+v", res)
	}
}

// TestRunnerConfigAndPrune (SEC-M2K-04): the tool runs in work/run with HOME and TMPDIR beside it, reads the job's
// configuration from work/files without being able to change it, and only the kept paths leave the step: the
// rest, a copy of the input for example, is removed without following symbolic links.
func TestRunnerConfigAndPrune(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	out, work := filepath.Join(dir, "out"), filepath.Join(dir, "work")
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `pwd > "$1/m2k/app/deploy/yamls/cwd"; /bin/cat "$2/files/cfg.yml" > "$1/m2k/app/deploy/yamls/cfg"; ` +
		`/bin/mkdir -p "$1/m2k/app/source" "$1/m2k/app/scripts"; echo secret > "$1/m2k/app/source/.env"; echo x > "$1/m2k/app/scripts/push.sh"; ` +
		`echo q > "$1/m2k/.m2k-qa/m2kqacache.yaml"; echo c > "$1/m2k/.m2k-qa/m2kconfig.yaml"; /bin/ln -s "$3" "$1/m2k/app/link"; ` +
		`echo changed > "$2/files/cfg.yml" 2>/dev/null; true`
	res := execute(Spec{Args: []string{sh, "-c", script, "sh", out, work, outside}, Mkdirs: []string{"m2k", "m2k/.m2k-qa", "m2k/app/deploy/yamls"},
		Files: map[string]string{"cfg.yml": "move2kube: {}\n"}, Keep: []string{"m2k/.m2k-qa/m2kqacache.yaml", "m2k/app/deploy/yamls"}, TimeoutSeconds: 10}, out, work, false)
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	var kept []string
	_ = filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(out, p)
			kept = append(kept, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(kept)
	want := []string{".samuori/result.json", ".samuori/stderr", ".samuori/stdout", "m2k/.m2k-qa/m2kqacache.yaml", "m2k/app/deploy/yamls/cfg", "m2k/app/deploy/yamls/cwd"}
	if strings.Join(kept, " ") != strings.Join(want, " ") {
		t.Errorf("kept %v", kept)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep me" {
		t.Error("the prune followed a symbolic link out of the output")
	}
	if b, _ := os.ReadFile(filepath.Join(out, "m2k/app/deploy/yamls/cfg")); string(b) != "move2kube: {}\n" {
		t.Errorf("the tool read %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "files", "cfg.yml")); string(b) != "move2kube: {}\n" {
		t.Error("the tool changed its configuration file")
	}
	if b, _ := os.ReadFile(filepath.Join(out, "m2k/app/deploy/yamls/cwd")); !sameDir(strings.TrimSpace(string(b)), filepath.Join(work, "run")) {
		t.Errorf("the tool ran in %s", b)
	}
	if res := execute(Spec{Args: []string{sh}, Files: map[string]string{"../escape": "x"}, TimeoutSeconds: 1}, out, work, false); res.Error == "" {
		t.Error("a configuration file name with a path was accepted")
	}
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// TestRealKompose runs the official Kompose release binary through the runner when SAMUORI_KOMPOSE_BINARY names it
// (docs/architecture/kubernetes-conversion.md#evidence). It checks the runner, not the isolation: confinement and
// --network=none apply only inside a Linux conversion step.
func TestRealKompose(t *testing.T) {
	bin := os.Getenv("SAMUORI_KOMPOSE_BINARY")
	if bin == "" {
		t.Skip("set SAMUORI_KOMPOSE_BINARY to a kompose v1.38.0 binary")
	}
	dir := t.TempDir()
	in, _ := filepath.Abs("../../examples/compose-shop")
	out := filepath.Join(dir, "out")
	res := execute(Spec{Args: []string{bin, "convert", "-f", filepath.Join(in, "compose.yml"), "-o", filepath.Join(out, "manifests"), "--provider", "kubernetes",
		"--build", "none", "--push-image=false", "--with-kompose-annotation=false", "--replicas", "1", "--volumes", "persistentVolumeClaim", "--namespace", "shop"},
		Mkdirs: []string{"manifests"}, TimeoutSeconds: 60}, out, filepath.Join(dir, "work"), false)
	_, _, stderr, _ := ReadMeta(out)
	t.Logf("exit %d, timed out %t, %d ms\n%s", res.ExitCode, res.TimedOut, res.DurationMs, stderr)
	if res.ExitCode != 0 {
		t.Fatalf("kompose failed: %+v", res)
	}
	entries, _ := os.ReadDir(filepath.Join(out, "manifests"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("files: %v", names)
	if len(names) != 8 {
		t.Errorf("kompose wrote %d files", len(names))
	}
}
