package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// installArgs runs install.sh's option parsing (from set -eu to the end of its while loop, which has no side
// effects) with args and returns what it records in /etc/samuori/install-args and what it decided.
func installArgs(t *testing.T, args ...string) (recorded []string, vars string) {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/systemd/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	start := strings.Index(script, "set -eu\n")
	loop := strings.Index(script, "while [ $# -gt 0 ]; do")
	end := strings.Index(script[loop:], "\ndone\n")
	if start < 0 || loop < 0 || end < 0 {
		t.Fatal("install.sh no longer has the option loop this test reads")
	}
	snippet := script[start:loop+end] + "\ndone\n" + `printf '%s' "$keep"; echo "role=$role keep_local_worker=$keep_local_worker action=${action:-} rest=$*"`
	out, err := exec.Command("sh", append([]string{"-c", snippet, "install.sh"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines[:len(lines)-1], lines[len(lines)-1]
}

func TestKeepLocalWorkerIsRecorded(t *testing.T) {
	recorded, vars := installArgs(t, "--release", "/tmp/r", "--role", "controller", "--host", "ctrl1.example", "--confirm-dedicated-vm", "--keep-local-worker",
		"init", "--key-file", "/var/lib/samuori/keys/secrets.key", "--s3-bucket", "b")
	if want := "role=controller keep_local_worker=yes action=init rest=--key-file /var/lib/samuori/keys/secrets.key --s3-bucket b"; vars != want {
		t.Fatalf("parsed %q; want %q", vars, want)
	}
	if got := strings.Join(recorded, " "); got != "--role controller --host ctrl1.example --confirm-dedicated-vm --keep-local-worker" {
		t.Fatalf("install-args = %q; want the role, host, confirmation and --keep-local-worker, and no init options", got)
	}
	// self-update.sh reruns install.sh with install-args, one option per line.
	_, again := installArgs(t, append([]string{"--release", "/var/lib/samuori-release/current"}, recorded...)...)
	if again != "role=controller keep_local_worker=yes action= rest=" {
		t.Fatalf("an upgrade from install-args parsed %q; want a controller upgrade that keeps its worker", again)
	}
}

func TestRegistryMirrorsAreRecorded(t *testing.T) {
	recorded, _ := installArgs(t, "--release", "/tmp/r", "--confirm-dedicated-vm", "--registry-mirror", "mcr.microsoft.com", "--registry-mirror", "quay.io")
	want := "--confirm-dedicated-vm --registry-mirror mcr.microsoft.com --registry-mirror quay.io"
	if got := strings.Join(recorded, " "); got != want {
		t.Fatalf("install-args = %q; want %q", got, want)
	}
	again, _ := installArgs(t, append([]string{"--release", "/var/lib/samuori-release/current"}, recorded...)...)
	if got := strings.Join(again, " "); got != want {
		t.Fatalf("an upgrade from install-args records %q; want the same mirrors %q", got, want)
	}
}
