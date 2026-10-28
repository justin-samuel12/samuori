package workerjoin

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestSamuoriJoinArguments runs deploy/systemd/samuori-join with arguments it must refuse before doing anything as root.
func TestSamuoriJoinArguments(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	script := "../../deploy/systemd/samuori-join"
	type joinCase struct {
		args []string
		exit int
		want string
	}
	cases := []joinCase{
		{nil, 2, "usage:"},
		{[]string{"controller"}, 2, "usage:"},
		{[]string{"server", "samuoric_x"}, 2, "usage:"},
		{[]string{"controller", "samuoric_x", "--bogus"}, 2, "usage:"},
		{[]string{"controller", "samuoric_x", "--host"}, 2, "usage:"},
		{[]string{"controller", "samuoriwj_x", "--host", "c.example"}, 1, "worker join code"},
		{[]string{"controller", "samuoriw_x", "--host", "c.example"}, 1, "not a controller join code"},
		{[]string{"worker", "samuoric_x"}, 1, "controller join code"},
		{[]string{"worker", "samuoriw_x"}, 1, "not a worker join code"},
		{[]string{"worker", "samuoriwj_x", "--host", "w.example"}, 1, "--host is for controllers"},
		{[]string{"controller", "samuoric_x", "--host", "c.example", "--confirm-dedicated-vm"}, 1, "for workers"},
		{[]string{"controller", "samuoric_x", "--release-signers", "/nonexistent/allowed_signers"}, 1, "does not exist"},
	}
	if os.Geteuid() != 0 {
		cases = append(cases,
			joinCase{[]string{"controller", "samuoric_x", "--host", "c.example"}, 1, "run as root"},
			joinCase{[]string{"worker", "samuoriwj_x", "--confirm-dedicated-vm"}, 1, "run as root"})
	}
	for _, c := range cases {
		out, err := exec.Command("sh", append([]string{script}, c.args...)...).CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != c.exit || !strings.Contains(string(out), c.want) {
			t.Errorf("samuori-join %v = %v %q, want exit %d with %q", c.args, err, out, c.exit, c.want)
		}
	}
}
