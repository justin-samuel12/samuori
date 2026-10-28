// Package buildah runs rootless Buildah as the dedicated build user, one process and one fresh storage root per
// build (D05, docs/spikes/buildah-isolation.md). Spec text reaches Buildah only as separate arguments, never through
// a shell. Buildah reads secrets from files, so it is never given any (OD-26).
package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"samuori.invalid/samuori/internal/engine"
)

// Options run Buildah as a dedicated build user, whose traffic the worker firewall confines (ADR 0025).
type Options struct {
	Binary string
	// RunAs prefixes every command, for example sudo -n -u samuori-build --.
	RunAs []string
	// Group is shared by the worker and the build user, and must be the build user's primary group.
	Group string
	// UserRuntime is the build user's runtime directory, such as /run/user/999.
	UserRuntime string
	// Mirrors maps upstream registries to pull-through mirrors; InsecureMirrors allows plain HTTP to them.
	Mirrors         map[string]string
	InsecureMirrors bool
	// Proxy is the egress proxy for RUN steps, passed as environment so it never enters image history. NoProxy
	// lists hosts Buildah reaches directly, such as the mirrors.
	Proxy   string
	NoProxy []string
	Scope   *Scope
}

// Scope bounds the build's memory and process count through the build user's delegated cgroup, as for BuildKit.
type Scope struct {
	Unit        string
	MemoryBytes uint64
	Tasks       int
}

func (s *Scope) path(runtime string) string {
	uid := filepath.Base(runtime)
	return "/user.slice/user-" + uid + ".slice/user@" + uid + ".service/app.slice/" + s.Unit + ".scope"
}

// Runner touches its build directory only through root: the build user can write there, so a path it planted must
// never lead the worker outside it.
type Runner struct {
	opts Options
	dir  string
	root *os.Root
	gid  int
	log  *capped
}

// ErrSecrets explains why a secret-bearing build cannot run on this engine.
var ErrSecrets = errors.New("the Buildah engine delivers no secrets; secret-bearing builds need a BuildKit pool (OD-26)")

// ErrPins explains why a build with base images pinned at admission cannot run on this engine (ADR 0049).
var ErrPins = errors.New("the Buildah engine cannot build pinned base images; builds whose tags are pinned at admission need a BuildKit pool")

func Start(dir string, o Options) (*Runner, error) {
	if o.Binary == "" {
		o.Binary = "buildah"
	}
	r := &Runner{opts: o, dir: dir, gid: -1, log: &capped{max: 64 << 10}}
	if o.Group != "" {
		g, err := user.LookupGroup(o.Group)
		if err != nil {
			return nil, fmt.Errorf("buildah: group %s: %w", o.Group, err)
		}
		if r.gid, err = strconv.Atoi(g.Gid); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	r.root = root
	for _, d := range []string{".", "root", "run", "out"} {
		if err := r.root.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
		if err := r.share(d); err != nil {
			return nil, err
		}
	}
	if err := r.writeConfig(); err != nil {
		return nil, err
	}
	// Rootless slirp4netns writes its log below the runtime directory (spike B2).
	if rt := r.runtimeDir(); rt != "" {
		argv := append(append([]string{}, o.RunAs...), "env", "mkdir", "-p", filepath.Join(rt, "libpod", "tmp"))
		if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("buildah: preparing the runtime directory: %v: %s", err, out)
		}
	}
	return r, nil
}

func (r *Runner) runtimeDir() string {
	if r.opts.UserRuntime != "" {
		return r.opts.UserRuntime
	}
	if len(r.opts.RunAs) == 0 {
		return os.Getenv("XDG_RUNTIME_DIR")
	}
	return ""
}

func (r *Runner) path(name string) string { return filepath.Join(r.dir, name) }

// share makes name, relative to the build directory, usable by the build user through the shared group; without a
// group it stays private.
func (r *Runner) share(name string) error {
	if r.gid < 0 {
		return nil
	}
	f, err := r.root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return r.shareFile(f)
}

func (r *Runner) shareFile(f *os.File) error {
	if r.gid < 0 {
		return nil
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	mode := os.FileMode(0o2770)
	if !st.IsDir() {
		mode = 0o640
		if st.Mode().Perm()&0o100 != 0 {
			mode = 0o750
		}
	}
	if err := f.Chown(-1, r.gid); err != nil {
		return err
	}
	return f.Chmod(mode)
}

// stage copies a tree the worker wrote privately into this build's shared directory, so the build user reads a copy
// and the worker's own files keep their permissions. Only directories and regular files are copied.
func (r *Runner) stage(src, name string) (string, error) {
	if err := r.root.RemoveAll(name); err != nil {
		return "", err
	}
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(name, rel)
		switch {
		case info.IsDir():
			if err := r.root.MkdirAll(target, 0o700); err != nil {
				return err
			}
			return r.share(target)
		case info.Mode().IsRegular():
			mode := os.FileMode(0o600)
			if info.Mode().Perm()&0o100 != 0 {
				mode = 0o700
			}
			return r.copyIn(p, target, mode)
		}
		return nil
	})
	return r.path(name), err
}

// copyIn copies a worker file to name in the build directory, which must not exist yet.
func (r *Runner) copyIn(src, name string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := r.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Chmod(mode)
	}
	if err == nil {
		err = r.shareFile(out)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func (r *Runner) writeIn(name string, body []byte) error {
	if err := r.root.WriteFile(name, body, 0o600); err != nil {
		return err
	}
	return r.share(name)
}

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)

func (r *Runner) writeConfig() error {
	// cgroupfs keeps step cgroups below the build's own cgroup and limits instead of new systemd units.
	conf := "[engine]\nruntime = \"runc\"\ncgroup_manager = \"cgroupfs\"\nevents_logger = \"none\"\n[network]\ndefault_rootless_network_cmd = \"slirp4netns\"\n"
	var reg strings.Builder
	reg.WriteString("unqualified-search-registries = []\nshort-name-mode = \"disabled\"\n")
	upstreams := make([]string, 0, len(r.opts.Mirrors))
	for u := range r.opts.Mirrors {
		upstreams = append(upstreams, u)
	}
	sort.Strings(upstreams)
	for _, u := range upstreams {
		m := r.opts.Mirrors[u]
		if !hostPattern.MatchString(u) || !hostPattern.MatchString(m) {
			return fmt.Errorf("buildah: mirror %s for %s is not a host", m, u)
		}
		location := u
		if u == "docker.io" {
			location = "registry-1.docker.io"
		}
		fmt.Fprintf(&reg, "[[registry]]\nprefix = %q\nlocation = %q\n[[registry.mirror]]\nlocation = %q\ninsecure = %t\n", u, location, m, r.opts.InsecureMirrors)
	}
	for name, body := range map[string]string{"containers.conf": conf, "registries.conf": reg.String()} {
		if err := r.writeIn(name, []byte(body)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) env() []string {
	env := []string{"-i", "HOME=" + r.path("run"), "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "CONTAINERS_CONF=" + r.path("containers.conf"),
		"CONTAINERS_REGISTRIES_CONF=" + r.path("registries.conf"), "TMPDIR=" + r.path("run")}
	if rt := r.runtimeDir(); rt != "" {
		env = append(env, "XDG_RUNTIME_DIR="+rt)
	}
	if r.opts.Proxy != "" {
		env = append(env, "HTTP_PROXY="+r.opts.Proxy, "HTTPS_PROXY="+r.opts.Proxy, "http_proxy="+r.opts.Proxy, "https_proxy="+r.opts.Proxy)
		if len(r.opts.NoProxy) > 0 {
			np := strings.Join(r.opts.NoProxy, ",")
			env = append(env, "NO_PROXY="+np, "no_proxy="+np)
		}
	}
	return env
}

// command builds the argv for one Buildah invocation as the build user, inside the scope when limits are set.
func (r *Runner) command(args ...string) []string {
	argv := append([]string{}, r.opts.RunAs...)
	if s := r.opts.Scope; s != nil && r.opts.UserRuntime != "" {
		argv = append(argv, "env", "XDG_RUNTIME_DIR="+r.opts.UserRuntime, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(r.opts.UserRuntime, "bus"),
			"systemd-run", "--user", "--scope", "--quiet", "--unit="+s.Unit,
			"-p", "Delegate=yes", "-p", fmt.Sprintf("MemoryMax=%d", s.MemoryBytes), "-p", "MemorySwapMax=0", "-p", fmt.Sprintf("TasksMax=%d", s.Tasks), "--",
			// Steps create cgroups beside Buildah's own; moving Buildah into a child of the scope keeps them inside. The
			// shell waits so that the scope, and its memory events, outlive Buildah long enough to be recorded.
			"sh", "-c", `mkdir -p "$1" && echo 0 > "$1/cgroup.procs" && events="$2" && out="$3" && shift 3 && "$@"; rc=$?; cat "$events" > "$out" 2>/dev/null; exit $rc`,
			"samuori-cgroup", filepath.Join("/sys/fs/cgroup", s.path(r.opts.UserRuntime), "buildah"), filepath.Join("/sys/fs/cgroup", s.path(r.opts.UserRuntime), "memory.events"), r.path("out/memory.events"))
	}
	argv = append(argv, "env")
	argv = append(argv, r.env()...)
	argv = append(argv, r.opts.Binary, "--root", r.path("root"), "--runroot", r.path("run"), "--storage-driver", "overlay")
	return append(argv, args...)
}

func (r *Runner) run(ctx context.Context, out io.Writer, args ...string) error {
	argv := r.command(args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	return cmd.Run()
}

// Build runs one build into an OCI archive. Build arguments, targets and paths are separate arguments.
func (r *Runner) Build(ctx context.Context, req engine.Request) (engine.Result, error) {
	if req.Frontend != "" {
		return engine.Result{}, errors.New("buildah: gateway frontends need BuildKit")
	}
	if len(req.ImageContexts) > 0 {
		return engine.Result{}, ErrPins
	}
	if req.OutputDir != "" || len(req.NamedContexts) > 0 {
		return engine.Result{}, errors.New("buildah: directory output and named contexts need BuildKit")
	}
	r.opts.Proxy, r.opts.NoProxy = req.Proxy, req.NoProxy
	contextDir, err := r.stage(req.ContextDir, "context")
	if err != nil {
		return engine.Result{}, err
	}
	dockerfile := filepath.Join(contextDir, req.Filename)
	if rel, err := filepath.Rel(req.ContextDir, req.DockerfileDir); err == nil && (rel == "." || filepath.IsLocal(rel)) {
		dockerfile = filepath.Join(contextDir, rel, req.Filename)
	} else {
		dir, err := r.stage(req.DockerfileDir, "dockerfile")
		if err != nil {
			return engine.Result{}, err
		}
		dockerfile = filepath.Join(dir, req.Filename)
	}
	archive := r.path("out/image.tar")
	args := []string{"build", "--isolation", "oci", "--network", "private", "--pull=always", "--layers=false", "--http-proxy=true",
		"--file", dockerfile, "--tag", "oci-archive:" + archive}
	if req.Target != "" {
		args = append(args, "--target", req.Target)
	}
	if len(req.Platforms) > 0 {
		args = append(args, "--platform", strings.Join(req.Platforms, ","))
	}
	keys := make([]string, 0, len(req.BuildArgs))
	for k := range req.BuildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+req.BuildArgs[k])
	}
	for _, m := range []struct {
		flag string
		kv   map[string]string
	}{{"--label", req.Labels}, {"--annotation", req.Annotations}} {
		for _, k := range slices.Sorted(maps.Keys(m.kv)) {
			args = append(args, m.flag, k+"="+m.kv[k])
		}
	}
	args = append(args, "--", contextDir)
	out := &capped{max: 4 << 20}
	err = r.run(ctx, out, args...)
	res := engine.Result{Log: out.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		return res, fmt.Errorf("%w: %v\n%s", engine.ErrBuild, err, tail(res.Log))
	}
	if err := r.moveOut("out/image.tar", req.OutputTar); err != nil {
		return res, err
	}
	return res, nil
}

// ErrUntrustedOutput means a file the build left for the worker was not one the build could have written.
var ErrUntrustedOutput = errors.New("buildah: the build left an output the worker will not read")

// moveOut copies a file the build user wrote, at name in the build directory, into the worker's own file. Anything
// but a regular file is refused, and so is a file the worker owns while the build runs as another user: the build
// user can only have linked it there.
func (r *Runner) moveOut(name, dst string) error {
	in, err := r.root.Open(name)
	if err != nil {
		return fmt.Errorf("buildah: the build wrote no %s: %w", name, err)
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrUntrustedOutput, name)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && len(r.opts.RunAs) > 0 && int(sys.Uid) == os.Geteuid() {
		return fmt.Errorf("%w: %s belongs to the worker", ErrUntrustedOutput, name)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Test loads the image from the layout into this build's storage and runs the validation Dockerfile against it,
// exporting the final filesystem. It never receives secrets and has no network.
func (r *Runner) Test(ctx context.Context, req engine.TestRequest) (engine.Result, error) {
	out := &capped{max: 1 << 20}
	layout := r.path("test-layout")
	if err := r.copyLayout(req.Layout, "test-layout", req.Manifest); err != nil {
		return engine.Result{}, err
	}
	dockerfileDir, err := r.stage(req.DockerfileDir, "test-dockerfile")
	if err != nil {
		return engine.Result{}, err
	}
	probeDir, err := r.stage(req.ProbeDir, "test-probe")
	if err != nil {
		return engine.Result{}, err
	}
	result := r.path("test-result")
	if err := r.root.RemoveAll("test-result"); err != nil {
		return engine.Result{}, err
	}
	if err := r.root.Mkdir("test-result", 0o700); err != nil {
		return engine.Result{}, err
	}
	if err := r.share("test-result"); err != nil {
		return engine.Result{}, err
	}
	var pulled bytes.Buffer
	if err := r.run(ctx, io.MultiWriter(out, &pulled), "pull", "--quiet", "--policy", "always", "oci:"+layout+":samuoriimage"); err != nil {
		return engine.Result{Log: out.String()}, fmt.Errorf("%w: loading the image: %v", engine.ErrBuild, err)
	}
	lines := strings.Fields(pulled.String())
	if len(lines) == 0 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(lines[len(lines)-1]) {
		return engine.Result{Log: out.String()}, errors.New("buildah: the image did not load")
	}
	args := []string{"build", "--isolation", "oci", "--network", "none", "--pull=never", "--layers=false",
		"--build-context", "samuoriimage=container-image://" + lines[len(lines)-1], "--build-context", "samuoriprobe=" + probeDir,
		"--file", filepath.Join(dockerfileDir, "Dockerfile"), "--output", "type=local,dest=" + result}
	if req.Platform != "" {
		args = append(args, "--platform", req.Platform)
	}
	args = append(args, "--", dockerfileDir)
	err = r.run(ctx, out, args...)
	res := engine.Result{Log: out.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		return res, fmt.Errorf("%w: %v", engine.ErrBuild, err)
	}
	return res, r.copyTree("test-result", req.OutputDir)
}

// copyLayout writes a layout holding only the chosen manifest, named samuoriimage so that it can be pulled by name.
func (r *Runner) copyLayout(src, dst, manifest string) error {
	hex, ok := strings.CutPrefix(manifest, "sha256:")
	if !ok || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(hex) {
		return errors.New("buildah: invalid manifest digest")
	}
	if _, err := r.stage(filepath.Join(src, "blobs"), filepath.Join(dst, "blobs")); err != nil {
		return err
	}
	if err := r.share(dst); err != nil {
		return err
	}
	info, err := os.Stat(filepath.Join(src, "blobs", "sha256", hex))
	if err != nil {
		return errors.New("buildah: the manifest is not in the layout")
	}
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []map[string]any{{
		"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": manifest, "size": info.Size(),
		"annotations": map[string]string{"org.opencontainers.image.ref.name": "samuoriimage"}}}})
	for name, body := range map[string][]byte{"index.json": index, "oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`)} {
		if err := r.writeIn(filepath.Join(dst, name), body); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies regular files the test exported, below name in the build directory, into the worker's directory;
// anything else is ignored.
func (r *Runner) copyTree(name, dst string) error {
	return fs.WalkDir(r.root.FS(), name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(name, filepath.FromSlash(p))
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			return r.moveOut(filepath.FromSlash(p), target)
		}
		return nil
	})
}

// OutOfMemory reports whether the kernel killed a process in the build's scope for exceeding the memory limit. The
// scope's cgroup is gone once Buildah exits, so the events recorded at exit are read first.
func (r *Runner) OutOfMemory() bool {
	s := r.opts.Scope
	if s == nil || r.opts.UserRuntime == "" {
		return false
	}
	recorded, _ := r.root.ReadFile("out/memory.events")
	live, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup", s.path(r.opts.UserRuntime), "memory.events"))
	for _, raw := range [][]byte{recorded, live} {
		for _, line := range strings.Split(string(raw), "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "oom_kill" && f[1] != "0" {
				return true
			}
		}
	}
	argv := append(append([]string{}, r.opts.RunAs...), "env", "XDG_RUNTIME_DIR="+r.opts.UserRuntime, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(r.opts.UserRuntime, "bus"),
		"systemctl", "--user", "show", "--property=Result", "--value", s.Unit+".scope")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = "/"
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "oom-kill"
}

// Stop kills anything still running for this build and removes its storage. Storage holds files owned by the build
// user's subordinate IDs, so removal runs inside its user namespace.
func (r *Runner) Stop() error {
	if len(r.opts.RunAs) > 0 {
		argv := append(append([]string{}, r.opts.RunAs...), "pkill", "-KILL", "-f", "--", "--root "+r.path("root"))
		_ = exec.Command(argv[0], argv[1:]...).Run()
	}
	if s := r.opts.Scope; s != nil && r.opts.UserRuntime != "" && len(r.opts.RunAs) > 0 {
		for _, verb := range []string{"stop", "reset-failed"} {
			argv := append(append([]string{}, r.opts.RunAs...), "env", "XDG_RUNTIME_DIR="+r.opts.UserRuntime,
				"DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(r.opts.UserRuntime, "bus"), "systemctl", "--user", verb, s.Unit+".scope")
			_ = exec.Command(argv[0], argv[1:]...).Run()
		}
	}
	if r.root != nil {
		_ = r.root.Close()
	}
	return Cleanup(r.opts, r.dir)
}

// Cleanup removes a build directory, including one a stopped worker left behind. Storage holds files owned by the
// build user's subordinate IDs, so they are removed inside its user namespace first.
func Cleanup(o Options, dir string) error {
	if o.Binary == "" {
		o.Binary = "buildah"
	}
	var owned []string
	for _, d := range []string{filepath.Join(dir, "root"), filepath.Join(dir, "run")} {
		if _, err := os.Stat(d); err == nil {
			owned = append(owned, d)
		}
	}
	if len(owned) > 0 {
		// Buildah needs a home for its configuration lookup, even to remove files.
		home, err := os.MkdirTemp("", "samuori-bh-home-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(home)
		if err := os.Chmod(home, 0o777); err != nil {
			return err
		}
		argv := append(append([]string{}, o.RunAs...), "env", "-i", "HOME="+home, "PATH=/usr/sbin:/usr/bin:/sbin:/bin")
		if o.UserRuntime != "" {
			argv = append(argv, "XDG_RUNTIME_DIR="+o.UserRuntime)
		} else if len(o.RunAs) == 0 && os.Getenv("XDG_RUNTIME_DIR") != "" {
			argv = append(argv, "XDG_RUNTIME_DIR="+os.Getenv("XDG_RUNTIME_DIR"))
		}
		argv = append(argv, o.Binary, "unshare", "find")
		argv = append(append(argv, owned...), "-mindepth", "1", "-delete")
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = "/"
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("buildah: removing %s: %v: %s", dir, err, tail(string(out)))
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("buildah: %s still exists after cleanup", dir)
	}
	return nil
}

type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }

func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}
