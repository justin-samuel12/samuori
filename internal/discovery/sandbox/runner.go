package sandbox

import (
	"regexp"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"samuori.invalid/samuori/internal/discovery"
	"syscall"
	"time"
)

// Limits bound one discovery subprocess (TM-15).
type Limits struct {
	Timeout           time.Duration
	CPUSeconds        int
	AddressSpaceBytes int64
	MaxInputBytes     int64
	MaxOutputBytes    int64
	MaxFindings       int
	MaxEvidence       int
	MaxStringBytes    int
}

func DefaultLimits() Limits {
	return Limits{Timeout: 60 * time.Second, CPUSeconds: 30, AddressSpaceBytes: 4 << 30, MaxInputBytes: 64 << 20, MaxOutputBytes: 8 << 20,
		MaxFindings: 500, MaxEvidence: 200, MaxStringBytes: 4096}
}

// Sandbox describes the isolation a run actually had; callers surface it rather than assume it.
type Sandbox struct {
	Platform   string `json:"platform"`
	Seccomp    bool   `json:"seccomp"`
	Namespaces bool   `json:"namespaces"`
}

// Runner executes discovery.Analyse in a separate, restricted process (ADR 0024). Command is the program and arguments
// that enter ChildMain; samuori-server uses its own executable with the "discover" argument.
type Runner struct {
	Command []string
	Limits  Limits
	// Namespaces requests user and network namespaces; Linux kernels that refuse them fall back to seccomp only.
	Namespaces bool
}

var ErrSandbox = errors.New("discovery sandbox failed")

func (r Runner) Run(ctx context.Context, in discovery.Input) (discovery.Result, Sandbox, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return discovery.Result{}, Sandbox{}, err
	}
	if int64(len(payload)) > r.Limits.MaxInputBytes {
		return discovery.Result{}, Sandbox{}, fmt.Errorf("discovery input is %d bytes, over the %d limit", len(payload), r.Limits.MaxInputBytes)
	}
	out, sb, err := r.spawn(ctx, payload, r.Namespaces)
	if err != nil && r.Namespaces && sandboxPlatform == "linux" && errors.Is(err, errStart) {
		out, sb, err = r.spawn(ctx, payload, false)
	}
	if err != nil {
		return discovery.Result{}, sb, err
	}
	res, err := validate(out, in, r.Limits)
	if err != nil {
		return discovery.Result{}, sb, fmt.Errorf("%w: invalid output: %v", ErrSandbox, err)
	}
	if !sb.Seccomp {
		res.Warnings = append(res.Warnings, discovery.Warning{Code: "sandbox-degraded", Message: "Discovery ran without process and network denial because this system is not Linux. Use this only for development."})
	}
	return res, sb, nil
}

var errStart = errors.New("could not start discovery process")

// Exec runs the command in the same restricted subprocess with payload on stdin and files inherited from file
// descriptor 3, for analyses other than discovery. Output is returned unvalidated: callers must treat it as
// untrusted data.
func (r Runner) Exec(ctx context.Context, payload []byte, files ...*os.File) ([]byte, Sandbox, error) {
	out, sb, err := r.spawn(ctx, payload, r.Namespaces, files...)
	if err != nil && r.Namespaces && sandboxPlatform == "linux" && errors.Is(err, errStart) {
		out, sb, err = r.spawn(ctx, payload, false, files...)
	}
	return out, sb, err
}

// ChildLimits reads the limits a child process was started with.
func ChildLimits() Limits {
	l := DefaultLimits()
	fmt.Sscan(os.Getenv("SAMUORI_DISCOVERY_CPU"), &l.CPUSeconds)
	fmt.Sscan(os.Getenv("SAMUORI_DISCOVERY_AS"), &l.AddressSpaceBytes)
	return l
}

// Isolated returns process attributes that place a child in new user and network namespaces where the platform
// supports them.
func Isolated() *syscall.SysProcAttr { return childAttr(true) }

// Restrict applies the child's resource limits, no_new_privs and the seccomp filter (Linux); call it before
// reading untrusted input.
func Restrict(l Limits) error { return restrict(l) }

func (r Runner) spawn(ctx context.Context, payload []byte, namespaces bool, files ...*os.File) ([]byte, Sandbox, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Limits.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Command[0], r.Command[1:]...)
	cmd.Env = []string{"GOMAXPROCS=2", fmt.Sprintf("GOMEMLIMIT=%d", r.Limits.AddressSpaceBytes/4), fmt.Sprintf("SAMUORI_DISCOVERY_CPU=%d", r.Limits.CPUSeconds), fmt.Sprintf("SAMUORI_DISCOVERY_AS=%d", r.Limits.AddressSpaceBytes)}
	cmd.Dir = "/"
	cmd.SysProcAttr = childAttr(namespaces)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.ExtraFiles = files
	stdout := &capped{max: r.Limits.MaxOutputBytes}
	stderr := &capped{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Second
	sb := Sandbox{Platform: sandboxPlatform, Seccomp: sandboxPlatform == "linux", Namespaces: namespaces && sandboxPlatform == "linux"}
	if err := cmd.Start(); err != nil {
		return nil, sb, fmt.Errorf("%w: %v", errStart, err)
	}
	err := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return nil, sb, fmt.Errorf("%w: exceeded the %s time limit", ErrSandbox, r.Limits.Timeout)
	case stdout.over:
		return nil, sb, fmt.Errorf("%w: output exceeded %d bytes", ErrSandbox, r.Limits.MaxOutputBytes)
	case err != nil:
		return nil, sb, fmt.Errorf("%w: %v: %s", ErrSandbox, err, clip(stderr.buf.String()))
	}
	return stdout.buf.Bytes(), sb, nil
}

type capped struct {
	buf  bytes.Buffer
	max  int64
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if int64(c.buf.Len()+len(p)) > c.max {
		c.over = true
		return 0, errors.New("output limit")
	}
	return c.buf.Write(p)
}

// ChildMain is the subprocess entry point: restrict first, then read untrusted input, analyse, and reply.
func ChildMain(stdin io.Reader, stdout io.Writer) error {
	l := ChildLimits()
	if err := restrict(l); err != nil {
		return err
	}
	var in discovery.Input
	if err := json.NewDecoder(io.LimitReader(stdin, l.MaxInputBytes)).Decode(&in); err != nil {
		return fmt.Errorf("reading input: %w", err)
	}
	return json.NewEncoder(stdout).Encode(discovery.Analyse(in))
}

var kinds = map[string]bool{"": true, "http-service": true, "static-site": true, "worker": true, "cli-task": true, "database": true, "other": true}

// validate treats subprocess output as untrusted data: strict decoding, bounded sizes, known enums and paths that
// exist in the snapshot.
func validate(raw []byte, in discovery.Input, l Limits) (discovery.Result, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var res discovery.Result
	if err := dec.Decode(&res); err != nil {
		return discovery.Result{}, err
	}
	paths := map[string]bool{".": true}
	for _, f := range in.Files {
		paths[f.Path] = true
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			paths[d] = true
		}
	}
	if len(res.Findings) > l.MaxFindings || len(res.Dockerfiles) > l.MaxFindings || len(res.Warnings) > l.MaxFindings {
		return discovery.Result{}, errors.New("too many results")
	}
	long := func(ss ...string) bool {
		for _, s := range ss {
			if len(s) > l.MaxStringBytes {
				return true
			}
		}
		return false
	}
	for _, f := range res.Findings {
		if !paths[f.SourceSubpath] || !kinds[f.ApplicationKind] || len(f.Evidence) > l.MaxEvidence || len(f.Warnings) > l.MaxEvidence {
			return discovery.Result{}, fmt.Errorf("finding for %q is out of bounds", clip(f.SourceSubpath))
		}
		if f.Confidence != "high" && f.Confidence != "medium" && f.Confidence != "low" {
			return discovery.Result{}, errors.New("unknown confidence level")
		}
		if long(f.DetectorID, f.DetectorVersion, f.Language, f.Framework, f.PackageManager) {
			return discovery.Result{}, errors.New("oversized field")
		}
		for _, e := range f.Evidence {
			if e.Weight < discovery.Contradicting || e.Weight > discovery.Strong || long(e.Kind, e.Path, e.Pointer, e.Observation) || (e.Path != "" && !paths[e.Path]) {
				return discovery.Result{}, errors.New("invalid evidence")
			}
		}
		for k, v := range f.SuggestedCommands {
			if long(k, v) {
				return discovery.Result{}, errors.New("oversized command")
			}
		}
		if err := predictions(f.NetworkNeeds, f.Readiness, paths, l, long); err != nil {
			return discovery.Result{}, err
		}
	}
	for _, d := range res.Dockerfiles {
		if !paths[d.Path] {
			return discovery.Result{}, errors.New("dockerfile path not in snapshot")
		}
		for _, m := range d.SecretMounts {
			if !logicalSecretID.MatchString(m.ID) {
				return discovery.Result{}, errors.New("invalid secret id")
			}
		}
		if err := predictions(d.NetworkNeeds, d.Readiness, paths, l, long); err != nil {
			return discovery.Result{}, err
		}
	}
	return res, nil
}

var (
	needKinds   = map[string]bool{"package-manager": true, "registry": true, "downloader": true, "url": true}
	severities  = map[string]bool{discovery.WillFail: true, discovery.MayFail: true, discovery.Advice: true}
	fixers      = map[string]bool{discovery.Developer: true, discovery.Administrator: true}
	confidences = map[string]bool{discovery.Certain: true, discovery.Likely: true, discovery.Possible: true}
	presetID    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	hostPort    = regexp.MustCompile(`^(?:[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?|\[[0-9a-f:.]{2,45}\]):[0-9]{1,5}$`)
)

// predictions bounds build readiness output (ADR 0050): known enums, host:port destinations and snapshot paths.
func predictions(needs []discovery.NetworkNeed, rs []discovery.Readiness, paths map[string]bool, l Limits, long func(...string) bool) error {
	if len(needs) > l.MaxEvidence || len(rs) > l.MaxEvidence {
		return errors.New("too many predictions")
	}
	for _, n := range needs {
		if !needKinds[n.Kind] || !severities[n.Severity] || n.Severity == discovery.Advice || !confidences[n.Confidence] || (n.Fixer != "" && !fixers[n.Fixer]) ||
			(n.Preset != "" && !presetID.MatchString(n.Preset)) || !paths[n.Path] || n.Line < 0 || len(n.Destinations) > 16 || long(n.Cause, n.Fix) {
			return errors.New("invalid network need")
		}
		if n.Preset == "" && len(n.Destinations) == 0 {
			return errors.New("network need without destination")
		}
		for _, d := range n.Destinations {
			if !hostPort.MatchString(d) {
				return errors.New("invalid destination")
			}
		}
	}
	for _, r := range rs {
		if !severities[r.Severity] || !fixers[r.Fixer] || !confidences[r.Confidence] || r.Blocking || (r.Path != "" && !paths[r.Path]) || r.Line < 0 ||
			long(r.Code, r.Message, r.Fix, r.Alternative) {
			return errors.New("invalid readiness item")
		}
	}
	return nil
}

var logicalSecretID = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

func clip(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
