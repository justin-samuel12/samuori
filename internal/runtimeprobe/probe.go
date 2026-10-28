// Package runtimeprobe runs one runtime test of a built image inside a BuildKit build step with no network
// (SEC-RTS-01). It starts as root in the step's user namespace, removes its own privileges, starts the application
// as a non-root user, observes it, kills every process it started and only then writes its result, so the
// application cannot alter the result (docs/architecture/validation.md#runtime-tests).
package runtimeprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FallbackUID runs applications whose image user is root or unset.
const FallbackUID = 10001

// ResultPath is where the probe writes its result inside the step.
const ResultPath = "/.samuori-result/result.json"

type Spec struct {
	Profile        string   `json:"profile"`
	Command        []string `json:"command"`
	User           string   `json:"user"`
	Port           int      `json:"port"`
	Path           string   `json:"path"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
	SettleSeconds  int      `json:"settleSeconds"`
}

type Constraints struct {
	NoNewPrivileges     bool   `json:"noNewPrivileges"`
	CapabilitiesDropped bool   `json:"capabilitiesDropped"`
	UID                 int    `json:"uid"`
	Network             string `json:"network"`
}

type Result struct {
	Outcome     string      `json:"outcome"`
	Reason      string      `json:"reason"`
	Constraints Constraints `json:"constraints"`
	ExitCode    *int        `json:"exitCode,omitempty"`
	HTTPStatus  int         `json:"httpStatus,omitempty"`
	DurationMs  int64       `json:"durationMs"`
}

func (s Spec) timeout() time.Duration {
	return time.Duration(min(max(s.TimeoutSeconds, 1), 600)) * time.Second
}

// Main is the entry point for "runtime-probe <spec JSON>". It always writes a result and exits zero, so a broken
// application never looks like a broken probe.
func Main(arg string) int {
	var spec Spec
	res := Result{Outcome: "errored"}
	start := time.Now()
	if err := json.Unmarshal([]byte(arg), &spec); err != nil {
		res.Reason = "invalid probe specification"
	} else {
		res = run(spec)
	}
	res.DurationMs = time.Since(start).Milliseconds()
	if err := write(res); err != nil {
		fmt.Fprintln(os.Stderr, "runtime-probe:", err)
		return 1
	}
	return 0
}

func write(res Result) error {
	dir := filepath.Dir(ResultPath)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	raw, _ := json.Marshal(res)
	f, err := os.OpenFile(ResultPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(raw)
	return err
}

// credentials maps the image user to a non-root uid and gid, reading the image's own passwd and group files.
func credentials(user string) (int, int, error) {
	name, group, _ := strings.Cut(user, ":")
	uid, gid := -1, -1
	if n, err := strconv.Atoi(name); err == nil {
		uid = n
	} else if name != "" && name != "root" {
		u, g, ok := lookup("/etc/passwd", name)
		if !ok {
			return 0, 0, fmt.Errorf("the image user %q does not exist in the image", name)
		}
		uid, gid = u, g
	}
	if group != "" {
		if n, err := strconv.Atoi(group); err == nil {
			gid = n
		} else if _, g, ok := lookup("/etc/group", group); ok {
			gid = g
		} else {
			return 0, 0, fmt.Errorf("the image group %q does not exist in the image", group)
		}
	}
	if uid <= 0 {
		uid = FallbackUID
		if gid <= 0 {
			gid = FallbackUID
		}
	}
	if gid < 0 {
		gid = uid
	}
	if gid == 0 {
		gid = FallbackUID
	}
	return uid, gid, nil
}

// lookup returns the id fields of name in a passwd- or group-format file.
func lookup(file, name string) (int, int, bool) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 3 && f[0] == name {
			id, err := strconv.Atoi(f[2])
			if err != nil {
				return 0, 0, false
			}
			gid := id
			if len(f) >= 4 && strings.HasSuffix(file, "passwd") {
				if g, err := strconv.Atoi(f[3]); err == nil {
					gid = g
				}
			}
			return id, gid, true
		}
	}
	return 0, 0, false
}

var errSandbox = errors.New("the runtime test sandbox is not as required")
