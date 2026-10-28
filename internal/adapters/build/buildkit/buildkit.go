// Package buildkit drives rootless BuildKit as its Go client. Spec text reaches the Dockerfile frontend only as
// attribute values, never through a host shell; one daemon serves exactly one build (OD-16, OD-18).
package buildkit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	digest "github.com/opencontainers/go-digest"
	"github.com/tonistiigi/fsutil"

	"samuori.invalid/samuori/internal/engine"
)

type Tools struct {
	Rootlesskit, Buildkitd string
}

func DefaultTools() Tools {
	return Tools{Rootlesskit: "rootlesskit", Buildkitd: "buildkitd"}
}

// Options run the daemon as a dedicated build user, whose traffic the worker firewall confines (ADR 0025).
type Options struct {
	// RunAs prefixes the daemon command, for example sudo -n -u samuori-build --.
	RunAs []string
	// Group is shared by the worker and the build user, and must be the build user's primary group so that files the
	// daemon creates, including its socket, belong to it.
	Group string
	// Config is the buildkitd.toml content, such as registry mirrors.
	Config []byte
	// Scope runs the daemon in a transient systemd user scope of the build user with memory and process limits.
	Scope *Scope
}

// Scope bounds one daemon's memory and process count through the build user's delegated cgroup (spike P1+L).
type Scope struct {
	Unit        string
	UserRuntime string
	MemoryBytes uint64
	Tasks       int
}

func (s *Scope) env() string {
	return "DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(s.UserRuntime, "bus")
}

// Path is the scope's cgroup, below the build user's systemd user manager.
func (s *Scope) Path() string {
	uid := filepath.Base(s.UserRuntime)
	return "/user.slice/user-" + uid + ".slice/user@" + uid + ".service/app.slice/" + s.Unit + ".scope"
}

func (s *Scope) systemctl(args ...string) []string {
	return append([]string{"env", "XDG_RUNTIME_DIR=" + s.UserRuntime, s.env(), "systemctl", "--user"}, args...)
}

// Daemon is one rootless buildkitd with private state, started for a single build.
type Daemon struct {
	tools Tools
	opts  Options
	gid   int
	dir   string
	run   string
	sock  string
	cmd   *exec.Cmd
	log   *capped
}

// Start launches a fresh daemon whose state lives under dir and waits until it answers.
func Start(ctx context.Context, tools Tools, dir string, o Options) (*Daemon, error) {
	d := &Daemon{tools: tools, opts: o, gid: -1, dir: dir, log: &capped{max: 64 << 10}}
	if o.Group != "" {
		g, err := user.LookupGroup(o.Group)
		if err != nil {
			return nil, fmt.Errorf("buildkit: group %s: %w", o.Group, err)
		}
		if d.gid, err = strconv.Atoi(g.Gid); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		return nil, err
	}
	// Sockets live in a short private directory because Unix socket paths are limited to 108 bytes.
	run, err := os.MkdirTemp("", "samuori-bk-")
	if err != nil {
		return nil, err
	}
	d.run, d.sock = run, "unix://"+filepath.Join(run, "buildkitd.sock")
	for _, p := range []string{dir, filepath.Join(dir, "state"), run} {
		if err := d.share(p); err != nil {
			_ = os.RemoveAll(run)
			return nil, err
		}
	}
	// The socket takes the group of its setgid directory; buildkitd's --group would map to a subordinate GID inside
	// the user namespace.
	args := []string{"--addr", d.sock, "--root", filepath.Join(dir, "state")}
	// The build user's environment is reset by sudo, so every runtime path is set explicitly. Build history is off
	// because it would keep raw step output, including anything a step prints from a secret.
	config := fmt.Sprintf("[otel]\n  socketPath = %q\n[history]\n  maxEntries = 0\n", filepath.Join(run, "otel-grpc.sock")) + string(o.Config)
	cfg := filepath.Join(run, "buildkitd.toml")
	if err := os.WriteFile(cfg, []byte(config), 0o640); err != nil {
		_ = os.RemoveAll(run)
		return nil, err
	}
	if err := d.share(cfg); err != nil {
		_ = os.RemoveAll(run)
		return nil, err
	}
	args = append(args, "--config", cfg)
	argv := append([]string{}, o.RunAs...)
	if len(o.RunAs) > 0 {
		// systemd-run finds the user manager through the user's runtime directory; the daemon gets its own.
		if o.Scope != nil {
			argv = append(argv, "env", "XDG_RUNTIME_DIR="+o.Scope.UserRuntime, o.Scope.env(), "systemd-run", "--user", "--scope", "--quiet", "--unit="+o.Scope.Unit,
				"-p", "Delegate=yes", "-p", fmt.Sprintf("MemoryMax=%d", o.Scope.MemoryBytes), "-p", "MemorySwapMax=0", "-p", fmt.Sprintf("TasksMax=%d", o.Scope.Tasks), "--",
				// runc creates step cgroups beside its own cgroup. Moving the daemon into a child of the delegated scope
				// keeps every step inside the scope and under its limits.
				"sh", "-c", `mkdir -p "$1" && echo 0 > "$1/cgroup.procs" && shift && exec "$@"`, "samuori-cgroup", filepath.Join("/sys/fs/cgroup", o.Scope.Path(), "daemon"))
		}
		// Without the user bus, runc places build steps in child cgroups of the daemon's scope instead of asking
		// systemd for sibling units outside its limits.
		argv = append(argv, "env", "-u", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR="+run)
	}
	argv = append(argv, tools.Rootlesskit, "--net=slirp4netns", "--copy-up=/etc", "--disable-host-loopback", "--state-dir="+filepath.Join(run, "rk"), tools.Buildkitd)
	argv = append(argv, args...)
	d.cmd = exec.Command(argv[0], argv[1:]...)
	d.cmd.Dir = "/"
	d.cmd.Stdout, d.cmd.Stderr = d.log, d.log
	d.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	d.cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+run)
	if err := d.cmd.Start(); err != nil {
		_ = os.RemoveAll(run)
		return nil, fmt.Errorf("buildkit: starting daemon: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			d.Stop()
			return nil, ctx.Err()
		}
		if ready(ctx, d.sock) {
			// buildkitd mounts its tracing socket into every step when the file exists (spike F3); nothing Samuori runs
			// needs it, so it is removed once the daemon is listening.
			_ = os.Remove(filepath.Join(run, "otel-grpc.sock"))
			return d, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs := d.log.String()
	d.Stop()
	return nil, fmt.Errorf("buildkit: daemon did not become ready: %s", tail(logs))
}

// share makes p usable by the build user through the shared group; without a group it stays private.
func (d *Daemon) share(p string) error {
	if d.gid < 0 {
		return nil
	}
	mode := os.FileMode(0o2770)
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		mode = 0o640
	}
	if err := os.Chown(p, -1, d.gid); err != nil {
		return err
	}
	return os.Chmod(p, mode)
}

// Stop terminates the daemon's process group and deletes its state; it is safe to call more than once. Processes
// that run as the build user are killed as that user, matched by this daemon's unique runtime directory.
func (d *Daemon) Stop() error {
	if d.cmd != nil && d.cmd.Process != nil {
		_ = syscall.Kill(-d.cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = d.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-d.cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		d.cmd = nil
	}
	if len(d.opts.RunAs) > 0 && d.run != "" {
		_ = d.asBuildUser("pkill", "-KILL", "-f", "--", d.run).Run()
	}
	if s := d.opts.Scope; s != nil {
		for _, verb := range []string{"stop", "reset-failed"} {
			argv := s.systemctl(verb, s.Unit+".scope")
			_ = d.asBuildUser(argv[0], argv[1:]...).Run()
		}
	}
	if d.run != "" {
		if err := d.removeAll(d.run); err != nil {
			return err
		}
	}
	return d.removeAll(d.dir)
}

// Cleanup kills any daemon whose state lives under dir and removes dir, for environments left by a worker that
// stopped without destroying them.
func Cleanup(tools Tools, o Options, dir string) error {
	d := &Daemon{tools: tools, opts: o, gid: -1, dir: dir}
	if o.Group != "" {
		if g, err := user.LookupGroup(o.Group); err == nil {
			d.gid, _ = strconv.Atoi(g.Gid)
		}
	}
	_ = d.asBuildUser("pkill", "-KILL", "-f", "--", "--root "+filepath.Join(dir, "state")).Run()
	return d.removeAll(dir)
}

func (d *Daemon) asBuildUser(name string, args ...string) *exec.Cmd {
	argv := append(append(append([]string{}, d.opts.RunAs...), name), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	// The build user may not be able to enter the worker's working directory.
	cmd.Dir = "/"
	return cmd
}

// removeAll deletes daemon state. Rootless snapshots contain files owned by the build user and its subordinate
// UIDs, which only its user namespace can remove, so a failed plain removal is retried inside one.
func (d *Daemon) removeAll(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	state, err := os.MkdirTemp("", "samuori-rm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(state)
	if err := d.share(state); err != nil {
		return err
	}
	out, err := d.asBuildUser(d.tools.Rootlesskit, "--net=host", "--state-dir="+filepath.Join(state, "rk"), "find", dir, "-mindepth", "1", "-delete").CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildkit: removing %s: %v: %s", dir, err, tail(string(out)))
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("buildkit: %s still exists after cleanup", dir)
	}
	return nil
}

func ready(ctx context.Context, sock string) bool {
	c, err := client.New(ctx, sock)
	if err != nil {
		return false
	}
	defer c.Close()
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err = c.ListWorkers(wctx)
	return err == nil
}

// Socket is the daemon's Unix socket path.
func (d *Daemon) Socket() string { return strings.TrimPrefix(d.sock, "unix://") }

// OutOfMemory reports whether the kernel killed a process in the daemon's scope, a build step or the daemon
// itself, for exceeding the memory limit.
func (d *Daemon) OutOfMemory() bool {
	s := d.opts.Scope
	if s == nil {
		return false
	}
	if raw, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", s.Path(), "memory.events")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "oom_kill" && f[1] != "0" {
				return true
			}
		}
	}
	argv := s.systemctl("show", "--property=Result", "--value", s.Unit+".scope")
	out, err := d.asBuildUser(argv[0], argv[1:]...).Output()
	return err == nil && strings.TrimSpace(string(out)) == "oom-kill"
}

// Pid identifies the daemon's process tree; it is zero once the daemon is stopped.
func (d *Daemon) Pid() int {
	if d.cmd == nil || d.cmd.Process == nil {
		return 0
	}
	return d.cmd.Process.Pid
}

func (d *Daemon) Running() bool { return d.cmd != nil && d.cmd.ProcessState == nil }

type (
	SecretSource = engine.SecretSource
	Request      = engine.Request
	Result       = engine.Result
	TestRequest  = engine.TestRequest
	MirrorToken  = engine.MirrorToken
)

var (
	ErrSecretNotFound = engine.ErrSecretNotFound
	ErrBuild          = engine.ErrBuild
)

type secretStore SecretSource

// GetSecret maps the engine's not-found error to BuildKit's, which leaves an optional mount empty.
func (s secretStore) GetSecret(ctx context.Context, id string) ([]byte, error) {
	v, err := s(ctx, id)
	if errors.Is(err, engine.ErrSecretNotFound) {
		return nil, secrets.ErrNotFound
	}
	return v, err
}

// Attrs returns the Dockerfile frontend attributes for r. Every value is passed as data to the frontend, never
// through a shell.
func Attrs(r Request) map[string]string {
	attrs := map[string]string{"filename": r.Filename}
	if r.Frontend != "" {
		for k, v := range r.FrontendOpts {
			attrs[k] = v
		}
		attrs["source"], attrs["filename"] = r.Frontend, r.Filename
		if len(r.Platforms) > 0 {
			attrs["platform"] = strings.Join(r.Platforms, ",")
		}
		if r.NoCache {
			attrs["no-cache"] = ""
		}
		return attrs
	}
	if r.Target != "" {
		attrs["target"] = r.Target
	}
	if len(r.Platforms) > 0 {
		attrs["platform"] = strings.Join(r.Platforms, ",")
	}
	for k, v := range r.BuildArgs {
		attrs["build-arg:"+k] = v
	}
	// BuildKit's predefined proxy arguments reach RUN steps without entering image history.
	if r.Proxy != "" {
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			attrs["build-arg:"+k] = r.Proxy
		}
	}
	for name := range r.NamedContexts {
		attrs["context:"+name] = "local:" + name
	}
	for k, v := range r.Labels {
		attrs["label:"+k] = v
	}
	for name, image := range r.ImageContexts {
		attrs["context:"+name] = "docker-image://" + image
	}
	if r.NoCache {
		attrs["no-cache"] = ""
	}
	return attrs
}

// ExportAttrs are the OCI exporter's attributes. "annotation.<key>" annotates every platform's image manifest;
// "annotation-index.<key>" annotates the index, which BuildKit v0.34.0 refuses for a single-platform export
// (exporter/containerimage/writer.go).
func ExportAttrs(r Request) map[string]string {
	attrs := map[string]string{}
	for k, v := range r.Annotations {
		attrs["annotation."+k] = v
		if len(r.Platforms) > 1 {
			attrs["annotation-index."+k] = v
		}
	}
	return attrs
}

// Build runs one solve as the BuildKit client. Secrets are served through the session only when the daemon asks
// for them; nothing is written to disk.
func (d *Daemon) Build(ctx context.Context, r Request) (Result, error) {
	c, err := client.New(ctx, d.sock)
	if err != nil {
		return Result{}, err
	}
	defer c.Close()
	contextFS, err := fsutil.NewFS(r.ContextDir)
	if err != nil {
		return Result{}, err
	}
	dockerfileFS, err := fsutil.NewFS(r.DockerfileDir)
	if err != nil {
		return Result{}, err
	}
	frontend := "dockerfile.v0"
	if r.Frontend != "" {
		frontend = "gateway.v0"
	}
	opt := client.SolveOpt{
		Frontend:      frontend,
		FrontendAttrs: Attrs(r),
		LocalMounts:   map[string]fsutil.FS{"context": contextFS, "dockerfile": dockerfileFS},
		Exports: []client.ExportEntry{{Type: client.ExporterOCI, Attrs: ExportAttrs(r), Output: func(map[string]string) (io.WriteCloser, error) {
			return os.OpenFile(r.OutputTar, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		}}},
	}
	if r.OutputDir != "" {
		opt.Exports = []client.ExportEntry{{Type: client.ExporterLocal, OutputDir: r.OutputDir}}
	}
	for name, dir := range r.NamedContexts {
		if name == "context" || name == "dockerfile" {
			return Result{}, fmt.Errorf("buildkit: %q is not a named context", name)
		}
		fs, err := fsutil.NewFS(dir)
		if err != nil {
			return Result{}, err
		}
		opt.LocalMounts[name] = fs
	}
	if r.Secrets != nil {
		opt.Session = []session.Attachable{secretsprovider.NewSecretProvider(secretStore(r.Secrets))}
	} else {
		opt.Session = []session.Attachable{secretsprovider.NewSecretProvider(secretStore(func(context.Context, string) ([]byte, error) {
			return nil, errors.New("this build has no secrets")
		}))}
	}
	if r.CacheImportDir != "" {
		opt.CacheImports = []client.CacheOptionsEntry{{Type: "local", Attrs: map[string]string{"src": r.CacheImportDir}}}
	}
	if r.CacheExportDir != "" {
		opt.CacheExports = []client.CacheOptionsEntry{{Type: "local", Attrs: map[string]string{"dest": r.CacheExportDir, "mode": "max"}}}
	}
	if r.MirrorToken != nil && len(r.MirrorHosts) > 0 {
		opt.Session = append(opt.Session, NewMirrorAuth(r.MirrorHosts, r.MirrorToken))
	}
	out := &capped{max: 4 << 20}
	ch := make(chan *client.SolveStatus)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var w io.Writer = out
		if r.Progress != nil {
			w = io.MultiWriter(out, r.Progress)
		}
		writeProgress(w, ch)
	}()
	resp, err := c.Solve(ctx, nil, opt, ch)
	<-done
	res := Result{Log: out.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		return res, fmt.Errorf("%w: %v\n%s", ErrBuild, err, tail(res.Log))
	}
	res.ImageDigest = resp.ExporterResponse["containerimage.digest"]
	return res, nil
}

// Test solves a validation Dockerfile and exports its final filesystem to OutputDir. It never receives secrets.
func (d *Daemon) Test(ctx context.Context, r TestRequest) (Result, error) {
	c, err := client.New(ctx, d.sock)
	if err != nil {
		return Result{}, err
	}
	defer c.Close()
	store, err := local.NewStore(r.Layout)
	if err != nil {
		return Result{}, err
	}
	dockerfileFS, err := fsutil.NewFS(r.DockerfileDir)
	if err != nil {
		return Result{}, err
	}
	probeFS, err := fsutil.NewFS(r.ProbeDir)
	if err != nil {
		return Result{}, err
	}
	opt := client.SolveOpt{
		Frontend: "dockerfile.v0",
		FrontendAttrs: map[string]string{"filename": "Dockerfile", "no-cache": "", "platform": r.Platform,
			"context:samuoriimage": "oci-layout://samuoriimage@" + r.Manifest, "context:samuoriprobe": "local:samuoriprobe"},
		LocalMounts: map[string]fsutil.FS{"context": dockerfileFS, "dockerfile": dockerfileFS, "samuoriprobe": probeFS},
		OCIStores:   map[string]content.Store{"samuoriimage": store},
		Exports:     []client.ExportEntry{{Type: client.ExporterLocal, OutputDir: r.OutputDir}},
		Session: []session.Attachable{secretsprovider.NewSecretProvider(secretStore(func(context.Context, string) ([]byte, error) {
			return nil, errors.New("runtime tests receive no secrets")
		}))},
	}
	out := &capped{max: 1 << 20}
	ch := make(chan *client.SolveStatus)
	done := make(chan struct{})
	go func() {
		defer close(done)
		writeProgress(out, ch)
	}()
	_, err = c.Solve(ctx, nil, opt, ch)
	<-done
	res := Result{Log: out.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		return res, fmt.Errorf("%w: %v", ErrBuild, err)
	}
	return res, nil
}

// writeProgress renders solve status as plain text: each step is numbered once, with its output and outcome.
// ToolRequest runs a generated Dockerfile whose steps use only the named local contexts and export their final
// filesystem to OutputDir (ADR 0048). It pulls no image, mounts no secret and sets no proxy.
type ToolRequest struct {
	DockerfileDir string
	Contexts      map[string]string
	OutputDir     string
	Platform      string
}

// ToolAttrs are the frontend attributes of a tool run; every named context is a local one.
func ToolAttrs(r ToolRequest) map[string]string {
	attrs := map[string]string{"filename": "Dockerfile", "no-cache": "", "platform": r.Platform}
	for name := range r.Contexts {
		attrs["context:"+name] = "local:" + name
	}
	return attrs
}

func (d *Daemon) RunTool(ctx context.Context, r ToolRequest) (Result, error) {
	c, err := client.New(ctx, d.sock)
	if err != nil {
		return Result{}, err
	}
	defer c.Close()
	dockerfileFS, err := fsutil.NewFS(r.DockerfileDir)
	if err != nil {
		return Result{}, err
	}
	mounts := map[string]fsutil.FS{"context": dockerfileFS, "dockerfile": dockerfileFS}
	for name, dir := range r.Contexts {
		fs, err := fsutil.NewFS(dir)
		if err != nil {
			return Result{}, err
		}
		mounts[name] = fs
	}
	opt := client.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: ToolAttrs(r),
		LocalMounts:   mounts,
		Exports:       []client.ExportEntry{{Type: client.ExporterLocal, OutputDir: r.OutputDir}},
		Session: []session.Attachable{secretsprovider.NewSecretProvider(secretStore(func(context.Context, string) ([]byte, error) {
			return nil, errors.New("conversion runs receive no secrets")
		}))},
	}
	out := &capped{max: 1 << 20}
	ch := make(chan *client.SolveStatus)
	done := make(chan struct{})
	go func() {
		defer close(done)
		writeProgress(out, ch)
	}()
	_, err = c.Solve(ctx, nil, opt, ch)
	<-done
	res := Result{Log: out.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		return res, fmt.Errorf("%w: %v", ErrBuild, err)
	}
	return res, nil
}

func writeProgress(w io.Writer, ch chan *client.SolveStatus) {
	index := map[digest.Digest]int{}
	done, cached := map[digest.Digest]bool{}, map[digest.Digest]bool{}
	num := func(d digest.Digest) int {
		if _, ok := index[d]; !ok {
			index[d] = len(index) + 1
		}
		return index[d]
	}
	for s := range ch {
		for _, v := range s.Vertexes {
			n := num(v.Digest)
			switch {
			// A vertex can complete once before its cache match is known, so a later cached completion is still shown.
			case v.Completed != nil && v.Cached && !cached[v.Digest]:
				cached[v.Digest], done[v.Digest] = true, true
				fmt.Fprintf(w, "#%d CACHED %s\n", n, v.Name)
			case v.Completed != nil && !done[v.Digest]:
				done[v.Digest] = true
				if v.Error != "" {
					fmt.Fprintf(w, "#%d ERROR: %s\n", n, v.Error)
				} else {
					fmt.Fprintf(w, "#%d DONE %s\n", n, v.Name)
				}
			case v.Started != nil && v.Completed == nil:
				fmt.Fprintf(w, "#%d %s\n", n, v.Name)
			}
		}
		for _, l := range s.Logs {
			n := num(l.Vertex)
			for _, line := range strings.Split(strings.TrimRight(string(l.Data), "\n"), "\n") {
				fmt.Fprintf(w, "#%d %s\n", n, line)
			}
		}
	}
}

type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }

func tail(s string) string {
	if len(s) > 2000 {
		return s[len(s)-2000:]
	}
	return s
}
