//go:build linux

package runtimeprobe

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// confine applies no_new_privs, makes the probe undumpable and empties the capability bounding set, so nothing the
// application starts can regain privileges.
func confine(c *Constraints) error {
	if os.Getuid() != 0 || os.Getpid() != 1 {
		return fmt.Errorf("%w: the probe must start as the step's first process with uid 0 in its namespace", errSandbox)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	c.NoNewPrivileges = true
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return err
	}
	for cap := 0; cap <= 63; cap++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(cap), 0, 0, 0); err != nil && err != unix.EINVAL {
			return fmt.Errorf("dropping capability %d: %w", cap, err)
		}
	}
	c.CapabilitiesDropped = true
	ifs, err := net.Interfaces()
	if err != nil {
		return err
	}
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback == 0 {
			if addrs, _ := i.Addrs(); len(addrs) > 0 {
				return fmt.Errorf("%w: interface %s has addresses", errSandbox, i.Name)
			}
		}
	}
	c.Network = "loopback only"
	return nil
}

// Confine applies the probe's sandbox constraints for another step runner, such as the conversion tool runner.
func Confine() (Constraints, error) {
	var c Constraints
	err := confine(&c)
	return c, err
}

func run(spec Spec) Result {
	res := Result{Outcome: "errored"}
	if err := confine(&res.Constraints); err != nil {
		res.Reason = err.Error()
		return res
	}
	if len(spec.Command) == 0 {
		res.Reason = "the image has no entrypoint or command"
		return res
	}
	uid, gid, err := credentials(spec.User)
	if err != nil {
		res.Reason = err.Error()
		return res
	}
	res.Constraints.UID = uid
	bin, err := exec.LookPath(spec.Command[0])
	if err != nil {
		res.Reason = fmt.Sprintf("the command %q was not found in the image", spec.Command[0])
		return res
	}
	cmd := exec.Command(bin, spec.Command[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
	if err := cmd.Start(); err != nil {
		res.Reason = "the application did not start: " + err.Error()
		return res
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	res = observe(spec, res, exited)
	_ = syscall.Kill(-1, syscall.SIGKILL)
	reap()
	return res
}

func exitCode(err error) *int {
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return &code
}

func observe(spec Spec, res Result, exited chan error) Result {
	deadline := time.After(spec.timeout())
	switch spec.Profile {
	case "cli-smoke":
		select {
		case err := <-exited:
			res.ExitCode = exitCode(err)
			if *res.ExitCode == 0 {
				res.Outcome, res.Reason = "passed", "the command exited successfully"
			} else {
				res.Outcome, res.Reason = "failed", fmt.Sprintf("the command exited with status %d", *res.ExitCode)
			}
		case <-deadline:
			res.Outcome, res.Reason = "failed", fmt.Sprintf("the command did not finish within %s", spec.timeout())
		}
	case "process-start":
		settle := time.Duration(min(max(spec.SettleSeconds, 5), 60)) * time.Second
		select {
		case err := <-exited:
			res.ExitCode = exitCode(err)
			res.Outcome, res.Reason = "failed", fmt.Sprintf("the process exited with status %d instead of staying up", *res.ExitCode)
		case <-time.After(settle):
			res.Outcome, res.Reason = "passed", fmt.Sprintf("the process was still running after %s", settle)
		}
	case "http-startup", "static-serve":
		if spec.Port <= 0 || spec.Port > 65535 {
			res.Reason = "no port to test; set application.port in the build specification"
			return res
		}
		url := fmt.Sprintf("http://127.0.0.1:%d%s", spec.Port, spec.Path)
		client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case err := <-exited:
				res.ExitCode = exitCode(err)
				res.Outcome, res.Reason = "failed", fmt.Sprintf("the server exited with status %d before answering", *res.ExitCode)
				return res
			case <-deadline:
				res.Outcome, res.Reason = "failed", fmt.Sprintf("no HTTP response on port %d within %s", spec.Port, spec.timeout())
				return res
			case <-tick.C:
				resp, err := client.Get(url)
				if err != nil {
					continue
				}
				resp.Body.Close()
				res.HTTPStatus = resp.StatusCode
				ok := resp.StatusCode < 500
				if spec.Profile == "static-serve" {
					ok = resp.StatusCode == http.StatusOK
				}
				if ok {
					res.Outcome, res.Reason = "passed", fmt.Sprintf("GET %s answered %d", spec.Path, resp.StatusCode)
				} else {
					res.Outcome, res.Reason = "failed", fmt.Sprintf("GET %s answered %d", spec.Path, resp.StatusCode)
				}
				return res
			}
		}
	default:
		res.Reason = "unknown runtime test profile " + spec.Profile
	}
	return res
}

// reap collects every killed process; as the namespace's first process the probe inherits orphans.
func reap() {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err == syscall.ECHILD {
			return
		}
		if pid <= 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
}
