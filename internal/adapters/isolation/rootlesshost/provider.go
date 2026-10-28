// Package rootlesshost runs builds with rootless BuildKit directly on the worker host: the rootless-host profile,
// for trusted workloads only. Eligibility never sends uploaded source to it (ADR 0027).
package rootlesshost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"samuori.invalid/samuori/internal/adapters/build/buildah"
	"samuori.invalid/samuori/internal/adapters/build/buildkit"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/egress/proxy"
	"samuori.invalid/samuori/internal/engine"
	"samuori.invalid/samuori/internal/engine/render"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/recipedef"
	"samuori.invalid/samuori/internal/redact"
)

const (
	Name          = "rootless-host"
	DedicatedName = "rootless-dedicated-vm"
)

func init() {
	isolation.Register(Name, func(settings map[string]string) (isolation.Provider, error) {
		return fromSettings(settings, isolation.RootlessHost)
	})
	isolation.Register(DedicatedName, func(settings map[string]string) (isolation.Provider, error) {
		return fromSettings(settings, isolation.RootlessDedicatedVM)
	})
}

func fromSettings(settings map[string]string, profile isolation.Profile) (isolation.Provider, error) {
	slots, _ := strconv.Atoi(settings["slots"])
	c := Config{Dir: settings["dir"], DaemonDir: settings["daemon-dir"], Tools: buildkit.DefaultTools(), Slots: max(slots, 1),
		BuildUser: settings["build-user"], BuildGroup: settings["build-group"], UnrestrictedEgress: settings["egress"] == "unrestricted",
		InsecureMirrors: settings["insecure-mirrors"] == "true", ProbeBinary: settings["probe-binary"], Profile: profile,
		CgroupLimits: settings["cgroup-limits"] == "systemd", Dedicated: settings["dedicated-host"] == "true", Engine: settings["engine"], RailpackDir: settings["railpack-dir"],
		WebLauncher: settings["web-launcher"]}
	c.Tasks, _ = strconv.Atoi(settings["max-processes"])
	c.RecycleBuilds, _ = strconv.Atoi(settings["recycle-after-builds"])
	c.RecycleHours, _ = strconv.Atoi(settings["recycle-after-hours"])
	if m := settings["mirrors"]; m != "" {
		c.Mirrors = map[string]string{}
		for _, pair := range strings.Split(m, ",") {
			up, mirror, ok := strings.Cut(pair, "=")
			if !ok || up == "" || mirror == "" {
				return nil, fmt.Errorf("rootless-host: mirror %q must be upstream=mirror-host:port", pair)
			}
			c.Mirrors[up] = mirror
		}
	}
	var l net.Listener
	if addr := settings["egress-listen"]; addr != "" {
		var err error
		if l, err = net.Listen("tcp", addr); err != nil {
			return nil, fmt.Errorf("rootless-host: egress proxy: %w", err)
		}
		c.Proxy, c.ProxyAddr = proxy.New(proxy.Options{}), l.Addr().String()
	}
	p, err := NewFromConfig(c)
	if err != nil {
		if l != nil {
			l.Close()
		}
		return nil, err
	}
	if l != nil {
		go func() { _ = c.Proxy.HTTPServer().Serve(l) }()
	}
	return p, nil
}

// Config describes a rootless-host worker. Strict egress needs a dedicated build user confined by the worker
// firewall (internal/egress/firewall), the egress proxy and registry mirrors; UnrestrictedEgress must be chosen
// explicitly otherwise.
type Config struct {
	Dir        string
	DaemonDir  string
	Tools      buildkit.Tools
	Slots      int
	BuildUser  string
	BuildGroup string
	Proxy      *proxy.Server
	ProxyAddr  string
	// Egress opens a build's egress session when the proxy runs elsewhere, as for a vm-per-job guest whose
	// only route out is the host's proxy; it replaces Proxy and ProxyAddr.
	Egress             func(rules []buildplan.EgressRule) (EgressSession, error)
	Mirrors            map[string]string
	InsecureMirrors    bool
	UnrestrictedEgress bool
	// ProbeBinary is a static samuori-worker used as the runtime-test probe inside images.
	ProbeBinary string
	// Profile is rootless-host (trusted workloads) or rootless-dedicated-vm (a VM dedicated to Samuori and one trust
	// domain, ADR 0027).
	Profile isolation.Profile
	// CgroupLimits runs each daemon in a systemd user scope of the build user with memory and process limits.
	CgroupLimits bool
	Tasks        int
	// Dedicated is the operator's confirmation that the host is a VM dedicated to Samuori and one trust domain.
	Dedicated     bool
	RecycleBuilds int
	RecycleHours  int
	// Engine is buildkit (the default) or buildah, which builds without secrets only (docs/spikes/buildah-isolation.md).
	Engine     string
	BuildahBin string
	// RailpackDir holds an installed Railpack (DefaultRailpackDir when empty); the worker offers the railpack method
	// only when it is present and the engine is BuildKit (ADR 0047).
	RailpackDir string
	// WebLauncher is a static samuori-web for the worker's architecture, copied into images that serve a frontend
	// (ADR 0055); the worker offers those builds only when it is present and the engine is BuildKit.
	WebLauncher string
}

// EgressSession is one build's proxy credential and decision log.
type EgressSession interface {
	URL() string
	Summary() string
	Report() isolation.EgressReport
	Close()
}

type env struct {
	spec   isolation.Spec
	runner engine.Runner
	// daemon is the runner when the engine is BuildKit.
	daemon    *buildkit.Daemon
	dir       string
	daemonDir string
}

type Provider struct {
	cfg     Config
	opts    buildkit.Options
	runtime string
	started time.Time
	digest  string

	mu    sync.Mutex
	envs  map[string]*env
	built int
	// railpack is the installed Railpack version, read once with the configuration digest.
	railpackVersion string
}

// New returns a provider with unrestricted build egress, for tests and trusted development hosts.
func New(dir string, tools buildkit.Tools, slots int) *Provider {
	p, _ := NewFromConfig(Config{Dir: dir, Tools: tools, Slots: slots, UnrestrictedEgress: true})
	return p
}

func NewFromConfig(c Config) (*Provider, error) {
	if c.Dir == "" {
		return nil, errors.New("rootless-host: the dir setting is required")
	}
	strict := c.Proxy != nil || c.Egress != nil
	switch {
	case c.Proxy != nil && c.Egress != nil:
		return nil, errors.New("rootless-host: configure either a local proxy or an egress session source, not both")
	case strict && c.UnrestrictedEgress:
		return nil, errors.New("rootless-host: egress cannot be both proxied and unrestricted")
	case !strict && !c.UnrestrictedEgress:
		return nil, errors.New("rootless-host: configure strict egress (egress-listen, build-user, build-group) or set egress=unrestricted explicitly")
	case strict && (c.BuildUser == "" || c.BuildGroup == "" || c.DaemonDir == ""):
		return nil, errors.New("rootless-host: strict egress needs build-user, build-group and daemon-dir so the firewall can confine the daemon")
	}
	if c.DaemonDir == "" {
		c.DaemonDir = c.Dir
	}
	if c.Engine == "" {
		c.Engine = engine.BuildKit
	}
	if !engine.Known(c.Engine) {
		return nil, fmt.Errorf("rootless-host: unknown engine %q; use buildkit or buildah", c.Engine)
	}
	if c.RailpackDir == "" {
		c.RailpackDir = DefaultRailpackDir
	}
	if c.BuildahBin == "" {
		c.BuildahBin = "buildah"
	}
	if c.Profile == "" {
		c.Profile = isolation.RootlessHost
	}
	if c.Tasks <= 0 {
		c.Tasks = 2048
	}
	if c.RecycleBuilds <= 0 {
		c.RecycleBuilds = 100
	}
	if c.RecycleHours <= 0 {
		c.RecycleHours = 24
	}
	if c.CgroupLimits && c.BuildUser == "" {
		return nil, errors.New("rootless-host: cgroup limits need a dedicated build user")
	}
	if c.Profile == isolation.RootlessDedicatedVM {
		switch {
		case !strict:
			return nil, errors.New("rootless-dedicated-vm: strict egress is required for uploaded source")
		case !c.CgroupLimits:
			return nil, errors.New("rootless-dedicated-vm: set cgroup-limits=systemd so every build has memory and process limits")
		case !c.Dedicated:
			return nil, errors.New("rootless-dedicated-vm: confirm with dedicated-host=true that this VM runs only Samuori builds for one trust domain")
		case !inVM():
			return nil, errors.New("rootless-dedicated-vm: this host is not a virtual machine; use vm-per-job on hardware hosts")
		}
	}
	p := &Provider{cfg: c, envs: map[string]*env{}, started: time.Now()}
	if c.CgroupLimits {
		u, err := user.Lookup(c.BuildUser)
		if err != nil {
			return nil, fmt.Errorf("rootless-host: build user: %w", err)
		}
		p.runtime = filepath.Join("/run/user", u.Uid)
	}
	if c.BuildGroup != "" {
		if err := groupReachable(c.DaemonDir, c.BuildGroup); err != nil {
			return nil, err
		}
	}
	if c.BuildUser != "" {
		p.opts.RunAs = []string{"sudo", "-n", "-u", c.BuildUser, "--"}
		p.opts.Group = c.BuildGroup
	}
	p.opts.Config = registryConfig(c.Mirrors, c.InsecureMirrors)
	if v, ok := installedRailpack(c.RailpackDir); ok && c.Engine == engine.BuildKit {
		p.railpackVersion = v
	}
	p.digest = configDigest(c, p.railpackVersion)
	return p, nil
}

// configDigest covers every setting and tool a self-test result depends on, so changing any of them requires a new
// self-test (OD-22). Tool binaries are hashed by content.
func configDigest(c Config, railpack string) string {
	h := sha256.New()
	mirrors := make([]string, 0, len(c.Mirrors))
	for k, v := range c.Mirrors {
		mirrors = append(mirrors, k+"="+v)
	}
	sort.Strings(mirrors)
	fmt.Fprintf(h, "profile=%s\nuser=%s\ngroup=%s\nproxy=%s\nstrict=%t\nunrestricted=%t\nmirrors=%s\ninsecure=%t\ncgroups=%t\ntasks=%d\ndedicated=%t\nslots=%d\ndaemondir=%s\nrecycle=%d/%d\nengine=%s\n",
		c.Profile, c.BuildUser, c.BuildGroup, c.ProxyAddr, c.Proxy != nil || c.Egress != nil, c.UnrestrictedEgress, strings.Join(mirrors, ","), c.InsecureMirrors, c.CgroupLimits, c.Tasks, c.Dedicated, c.Slots, c.DaemonDir, c.RecycleBuilds, c.RecycleHours, c.Engine)
	tools := []string{c.Tools.Rootlesskit, c.Tools.Buildkitd, c.ProbeBinary}
	if c.Engine == engine.Buildah {
		tools = []string{c.BuildahBin, "runc", "netavark", "slirp4netns", c.ProbeBinary}
	}
	for _, tool := range tools {
		fmt.Fprintf(h, "tool=%s:%s\n", tool, fileHash(tool))
	}
	if railpack != "" {
		fmt.Fprintf(h, "railpack=%s:%s\n", railpack, fileHash(filepath.Join(c.RailpackDir, "railpack")))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func fileHash(name string) string {
	if name == "" {
		return ""
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "missing"
	}
	d, err := fileDigest(path)
	if err != nil {
		return "unreadable"
	}
	return d
}

// registryConfig points each upstream registry at its mirror. With strict egress the firewall refuses any other
// registry, so a pull that falls back to the upstream fails closed.
func registryConfig(mirrors map[string]string, insecure bool) []byte {
	var b strings.Builder
	ups := make([]string, 0, len(mirrors))
	for up := range mirrors {
		ups = append(ups, up)
	}
	sort.Strings(ups)
	hosts := map[string]bool{}
	for _, up := range ups {
		fmt.Fprintf(&b, "[registry.%q]\n  mirrors = [%q]\n", up, mirrors[up])
		hosts[mirrors[up]] = true
	}
	if insecure {
		for _, h := range sortedKeys(hosts) {
			fmt.Fprintf(&b, "[registry.%q]\n  http = true\n", h)
		}
	}
	return []byte(b.String())
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (p *Provider) Name() string {
	if p.cfg.Profile == isolation.RootlessDedicatedVM {
		return DedicatedName
	}
	return Name
}

// inVM reports whether the host itself is a virtual machine.
func inVM() bool {
	return exec.Command("systemd-detect-virt", "--vm", "--quiet").Run() == nil
}

// recycleDue reports that a dedicated VM ran its share of builds or hours and must be replaced (ADR 0027).
func (p *Provider) recycleDue() bool {
	if p.cfg.Profile != isolation.RootlessDedicatedVM {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.built >= p.cfg.RecycleBuilds || time.Since(p.started) >= time.Duration(p.cfg.RecycleHours)*time.Hour
}

func (p *Provider) Capabilities(context.Context) (isolation.Capabilities, error) {
	if runtime.GOOS != "linux" {
		return isolation.Capabilities{}, errors.New("rootless-host: requires Linux")
	}
	slots := uint32(p.cfg.Slots)
	if p.recycleDue() {
		slots = 0
	}
	return isolation.Capabilities{Provider: p.Name(), Profiles: []isolation.Profile{p.cfg.Profile}, OS: "linux", Architectures: []string{runtime.GOARCH}, Startup: isolation.StartupSeconds,
		FreeSlots: slots, StrictEgress: p.cfg.Proxy != nil || p.cfg.Egress != nil, Engines: []string{p.cfg.Engine}, ConfigDigest: p.digest, Providers: p.providers()}, nil
}

// providers lists the build providers this worker can run. Buildpacks need nothing installed, only BuildKit, so the
// worker reports the CNB platform API it drives the lifecycle with.
func (p *Provider) providers() map[string]string {
	out := map[string]string{}
	if p.railpackVersion != "" {
		out["railpack"] = p.railpackVersion
	}
	if p.cfg.Engine == engine.BuildKit {
		out["buildpacks"] = buildplan.CNBPlatformAPI
		if launcherPresent(p.cfg.WebLauncher) {
			out[buildplan.LauncherProvider] = buildplan.LauncherContract
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Prepare starts a fresh daemon with private state for this attempt; daemons are never shared or reused.
func (p *Provider) Prepare(ctx context.Context, s isolation.Spec) (isolation.Env, error) {
	if s.Profile != p.cfg.Profile {
		return isolation.Env{}, &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "this provider offers only " + string(p.cfg.Profile)}
	}
	if p.recycleDue() {
		return isolation.Env{}, &isolation.Failure{Class: isolation.ErrOccupied, Detail: "this dedicated VM is due for recycling"}
	}
	p.mu.Lock()
	_, busy := p.envs[s.AttemptID]
	p.mu.Unlock()
	if busy {
		return isolation.Env{}, &isolation.Failure{Class: isolation.ErrOccupied, Detail: "an environment for this attempt already exists"}
	}
	dir := filepath.Join(p.cfg.Dir, s.AttemptID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return isolation.Env{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	daemonDir := filepath.Join(dir, "daemon")
	if p.cfg.DaemonDir != p.cfg.Dir {
		daemonDir = filepath.Join(p.cfg.DaemonDir, s.AttemptID)
	}
	mem := s.Limits.MemoryBytes
	if mem == 0 {
		mem = buildplan.DefaultLimits.MemoryBytes
	}
	held := &env{spec: s, dir: dir, daemonDir: daemonDir}
	if p.cfg.Engine == engine.Buildah {
		o := p.buildahOptions()
		if p.cfg.CgroupLimits {
			o.Scope = &buildah.Scope{Unit: "samuori-bh-" + strings.ToLower(s.AttemptID), MemoryBytes: mem, Tasks: p.cfg.Tasks}
		}
		r, err := buildah.Start(daemonDir, o)
		if err != nil {
			_ = os.RemoveAll(dir)
			return isolation.Env{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
		}
		held.runner = r
	} else {
		opts := p.opts
		if p.cfg.CgroupLimits {
			opts.Scope = &buildkit.Scope{Unit: "samuori-bk-" + strings.ToLower(s.AttemptID), UserRuntime: p.runtime, MemoryBytes: mem, Tasks: p.cfg.Tasks}
		}
		d, err := buildkit.Start(ctx, p.cfg.Tools, daemonDir, opts)
		if err != nil {
			_ = os.RemoveAll(dir)
			return isolation.Env{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
		}
		held.runner, held.daemon = d, d
	}
	e := isolation.Env{ID: s.AttemptID, AttemptID: s.AttemptID, Profile: s.Profile, Created: time.Now(), Deadline: time.Now().Add(s.Limits.WallClock)}
	p.mu.Lock()
	p.envs[e.ID] = held
	p.mu.Unlock()
	return e, nil
}

func (p *Provider) get(id string) (*env, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.envs[id]
	if !ok {
		return nil, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "environment not found"}
	}
	return e, nil
}

// renderFailure classes a plan this release cannot render as infrastructure, so the attempt is retried and blamed
// on the platform (docs/architecture/build-lifecycle.md#failure-classes).
func renderFailure(raw []byte, err error) error {
	if render.Skew(raw, err) {
		return &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "this worker's release cannot render the plan; update the worker: " + err.Error()}
	}
	return &isolation.Failure{Class: isolation.ErrUnsupported, Detail: err.Error()}
}

// pinContexts maps each pinned FROM reference to the named context BuildKit looks up for it (ADR 0049).
func pinContexts(pins []buildplan.BaseImagePin) (map[string]string, error) {
	if len(pins) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, p := range pins {
		name, err := buildplan.ContextName(p.Ref)
		if err != nil {
			return nil, fmt.Errorf("pinned base image %q: %w", p.Ref, err)
		}
		if !strings.Contains(p.Image, "@sha256:") {
			return nil, fmt.Errorf("pinned base image %q is not a digest reference", p.Ref)
		}
		out[name] = p.Image
	}
	return out, nil
}

func (p *Provider) Run(ctx context.Context, ie isolation.Env, raw []byte) (isolation.RunResult, error) {
	var sess EgressSession
	res, err := p.run(ctx, ie, raw, &sess)
	if sess != nil {
		res.Egress = sess.Report()
	}
	return res, err
}

func (p *Provider) run(ctx context.Context, ie isolation.Env, raw []byte, egressSess *EgressSession) (isolation.RunResult, error) {
	e, err := p.get(ie.ID)
	if err != nil {
		return isolation.RunResult{}, err
	}
	plan, err := buildplan.Decode(raw)
	if err != nil {
		return isolation.RunResult{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "unreadable plan"}
	}
	if len(plan.Secrets) > 0 && !engine.DeliversSecrets(p.cfg.Engine) {
		return isolation.RunResult{}, &isolation.Failure{Class: isolation.ErrUnsupported, Detail: buildah.ErrSecrets.Error()}
	}
	mask := redact.New()
	delivered := &deliveredValues{values: map[string][]byte{}}
	defer delivered.clear()
	out := e.dir
	if e.spec.OutputDir != "" {
		out = e.spec.OutputDir
	}
	req := buildkit.Request{BuildArgs: map[string]string{}, NoCache: plan.Cache.Mode != "scoped", OutputTar: filepath.Join(out, "image.tar"),
		Secrets: secretSource(plan, e.spec.ReleaseSecret, mask, delivered)}
	if len(plan.Secrets) == 0 {
		req.Progress = ie.Progress
	}
	// Shared caches stay within the controller-assigned scope (docs/architecture/build-cache.md); Buildah keeps none.
	cached := plan.Cache.Mode == "scoped" && plan.Cache.Scope != "" && p.cfg.Engine == engine.BuildKit
	if cached {
		req.CacheImportDir, req.CacheExportDir = e.spec.CacheDir, filepath.Join(e.dir, "cache-out")
	}
	if e.spec.MirrorToken != nil && len(p.cfg.Mirrors) > 0 {
		hosts := make([]string, 0, len(p.cfg.Mirrors))
		for _, m := range p.cfg.Mirrors {
			hosts = append(hosts, m)
		}
		req.MirrorHosts, req.MirrorToken = hosts, engine.MirrorToken(e.spec.MirrorToken)
	}
	if plan.Image != nil {
		req.Labels, req.Annotations = plan.Image.Labels, plan.Image.Annotations
	}
	for k, v := range plan.BuildArgs {
		if !proxyArgs[strings.ToUpper(k)] {
			req.BuildArgs[k] = v
		}
	}
	egressLog := func() string {
		return "egress: unrestricted on this worker (trusted rootless-host without strict egress)\n"
	}
	if open := p.openEgress(); open != nil {
		sess, err := open(plan.Egress.Allow)
		if err != nil {
			return isolation.RunResult{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
		}
		defer sess.Close()
		*egressSess = sess
		req.Proxy = sess.URL()
		for _, m := range p.cfg.Mirrors {
			req.NoProxy = append(req.NoProxy, m)
		}
		egressLog = sess.Summary
	}
	var records []isolation.Record
	var planLog string
	var cnb *buildpacksRun
	buildCtx := ctx
	switch plan.Method {
	case "recipe":
		df, err := render.Dockerfile(plan)
		if err != nil {
			return isolation.RunResult{}, renderFailure(raw, err)
		}
		dfDir := filepath.Join(e.dir, "dockerfile")
		if err := os.MkdirAll(dfDir, 0o700); err != nil {
			return isolation.RunResult{}, err
		}
		if err := os.WriteFile(filepath.Join(dfDir, "Dockerfile"), df, 0o600); err != nil {
			return isolation.RunResult{}, err
		}
		req.ContextDir, req.DockerfileDir, req.Filename = e.spec.SourceDir, dfDir, "Dockerfile"
		if plan.Recipe.Frontend != nil {
			if planLog, err = p.placeLauncher(e, plan, &req); err != nil {
				return isolation.RunResult{}, err
			}
		}
	case "dockerfile":
		d := plan.Dockerfile
		if d == nil || !filepath.IsLocal(filepath.FromSlash(d.Context)) || !filepath.IsLocal(filepath.FromSlash(d.Path)) {
			return isolation.RunResult{}, &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "the Dockerfile context and path must stay inside the source"}
		}
		req.ContextDir = filepath.Join(e.spec.SourceDir, filepath.FromSlash(d.Context))
		req.DockerfileDir = filepath.Join(e.spec.SourceDir, filepath.FromSlash(filepath.Dir(d.Path)))
		req.Filename, req.Target = filepath.Base(d.Path), d.Target
		if req.ImageContexts, err = pinContexts(d.BaseImagePins); err != nil {
			return isolation.RunResult{}, renderFailure(raw, err)
		}
		if len(req.ImageContexts) > 0 && p.cfg.Engine != engine.BuildKit {
			return isolation.RunResult{}, &isolation.Failure{Class: isolation.ErrUnsupported, Detail: buildah.ErrPins.Error()}
		}
	case "railpack":
		proxyURL := req.Proxy
		records, planLog, err = p.railpack(ctx, e, plan, &req, out, proxyURL, mask)
		if err != nil {
			summary := egressLog()
			logs := mask.Mask([]byte("--- railpack prepare ---\n" + planLog + "\n" + summary))
			// Railpack reports a refused download as transient (exit 75), but a denial by the build's own egress
			// allowlist recurs on every attempt, so it is the build's failure, not the platform's.
			if isolation.Requeue(err) && strings.Contains(summary, "egress: denied") {
				err = fmt.Errorf("%w: railpack prepare needs a destination the build's egress allowlist denies; see the egress lines in the log", engine.ErrBuild)
			}
			var f *isolation.Failure
			if errors.As(err, &f) {
				return isolation.RunResult{Logs: logs, Records: records}, err
			}
			return isolation.RunResult{Logs: logs, Records: records}, maskedError{msg: string(mask.Mask([]byte(err.Error()))), err: err}
		}
		planLog = "--- railpack prepare ---\n" + planLog + "\n--- build ---\n"
	case "buildpacks":
		if req.Proxy != "" {
			mask.Add("egress-proxy", []byte(req.Proxy))
		}
		cnb, planLog, err = p.buildpacks(ctx, e, plan, &req, out)
		if err != nil {
			logs := mask.Mask([]byte(planLog + egressLog()))
			var f *isolation.Failure
			if errors.As(err, &f) {
				return isolation.RunResult{Logs: logs}, err
			}
			return isolation.RunResult{Logs: logs}, maskedError{msg: string(mask.Mask([]byte(err.Error()))), err: err}
		}
		planLog += "\n--- lifecycle ---\n"
		var cancel context.CancelFunc
		buildCtx, cancel = context.WithTimeout(ctx, time.Duration(max(plan.Buildpacks.TimeoutSeconds, 1))*time.Second)
		defer cancel()
	default:
		return isolation.RunResult{}, renderFailure(raw, fmt.Errorf("%w: unknown build method %q", render.ErrNewerPlan, plan.Method))
	}

	res, err := e.runner.Build(buildCtx, req)
	logs := mask.Mask([]byte(planLog + res.Log + egressLog()))
	if err != nil && e.runner.OutOfMemory() {
		return isolation.RunResult{Logs: logs}, &isolation.Failure{Class: isolation.ErrResourceLimit, Detail: "the build exceeded its memory limit"}
	}
	if err != nil && cnb != nil && ctx.Err() == nil && errors.Is(buildCtx.Err(), context.DeadlineExceeded) {
		return isolation.RunResult{Logs: logs}, &isolation.Failure{Class: isolation.ErrResourceLimit, Detail: fmt.Sprintf("the buildpacks lifecycle exceeded its %ds limit", plan.Buildpacks.TimeoutSeconds)}
	}
	if err == nil && plan.Method == "railpack" && plan.Railpack.User != "" {
		if err = NonRootImage(req.OutputTar, plan.Railpack.User); err != nil {
			return isolation.RunResult{Logs: logs}, &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "the image could not get the non-root user layer (" + err.Error() + "); an administrator can keep Railpack's root user under Build & conversion providers"}
		}
		logs = append(logs, []byte("\nnon-root user: the image runs as "+plan.Railpack.User+" (ADR 0047)\n")...)
	}
	if err == nil && cnb != nil {
		if records, err = cnb.finish(req.OutputTar); err != nil {
			var f *isolation.Failure
			if errors.As(err, &f) {
				return isolation.RunResult{Logs: logs}, err
			}
		}
	}
	if err != nil {
		// Error text reaches the controller as the attempt detail, so delivered secret values are masked in it too.
		return isolation.RunResult{Logs: logs}, maskedError{msg: string(mask.Mask([]byte(err.Error()))), err: err}
	}
	// buildctl writes the archive world-writable (spike F12).
	if err := os.Chmod(req.OutputTar, 0o600); err != nil {
		return isolation.RunResult{Logs: logs}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	digest, err := fileDigest(req.OutputTar)
	if err != nil {
		return isolation.RunResult{Logs: logs}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	var cacheFile, cacheDigest string
	if cached {
		cacheFile = filepath.Join(out, "cache.tar")
		if f, err := os.OpenFile(cacheFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			perr := engine.PackCache(req.CacheExportDir, f)
			if cerr := f.Close(); perr == nil {
				perr = cerr
			}
			if perr == nil {
				cacheDigest, perr = fileDigest(cacheFile)
			}
			if perr != nil {
				cacheFile, cacheDigest = "", ""
			}
		} else {
			cacheFile = ""
		}
	}
	var checks []isolation.Check
	values := delivered.snapshot()
	if cnb != nil && req.Proxy != "" {
		// The lifecycle hands the proxy to every buildpack; its per-build credential must not reach a layer.
		values["egress-proxy"] = []byte(req.Proxy)
	}
	scan := cnb != nil && req.Proxy != ""
	for _, s := range plan.Secrets {
		scan = scan || s.BindingID != ""
	}
	if scan {
		checks = append(checks, canary(req.OutputTar, values))
	}
	delivered.clear()
	if plan.Validation.Profile != "" && plan.Validation.Profile != buildplan.ProfileNone {
		check, testLog := p.runtimeTest(ctx, e, plan.Validation, req.OutputTar)
		checks = append(checks, check)
		if testLog != "" {
			logs = append(logs, mask.Mask([]byte("\n--- runtime test ---\n"+testLog))...)
		}
	}
	return isolation.RunResult{ArtifactDigest: digest, ArtifactFile: req.OutputTar, Logs: logs, Checks: checks, CacheFile: cacheFile, CacheDigest: cacheDigest, Records: records}, nil
}

type localSession struct {
	*proxy.Session
	addr string
}

func (s localSession) URL() string { return s.Session.URL(s.addr) }

func (s localSession) Report() isolation.EgressReport { return ProxyReport(s.Session) }

// ProxyReport is the structured form of a proxy session's denials for the attempt report.
func ProxyReport(s *proxy.Session) isolation.EgressReport {
	denied, unrecorded := s.Denied()
	r := isolation.EgressReport{Unrecorded: unrecorded}
	for _, d := range denied {
		r.Denied = append(r.Denied, isolation.EgressDenial{Host: d.Host, Port: d.Port, Count: d.Count, Reason: d.Reason})
	}
	return r
}

func (p *Provider) openEgress() func([]buildplan.EgressRule) (EgressSession, error) {
	if p.cfg.Egress != nil {
		return p.cfg.Egress
	}
	if p.cfg.Proxy == nil {
		return nil
	}
	return func(rules []buildplan.EgressRule) (EgressSession, error) {
		sess, err := p.cfg.Proxy.Open(rules)
		if err != nil {
			return nil, err
		}
		return localSession{Session: sess, addr: p.cfg.ProxyAddr}, nil
	}
}

// deliveredValues keeps released values in memory for the canary only, and wipes them afterwards.
type deliveredValues struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (d *deliveredValues) add(id string, v []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.values[id] = append([]byte(nil), v...)
}

func (d *deliveredValues) snapshot() map[string][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string][]byte, len(d.values))
	for k, v := range d.values {
		out[k] = v
	}
	return out
}

func (d *deliveredValues) clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range d.values {
		clear(v)
		delete(d.values, k)
	}
}

// secretSource serves BuildKit's secret requests. IDs outside the plan fail the step instead of mounting nothing,
// because BuildKit treats secret mounts as optional by default (SEC-SCR-03, SEC-SCR-07); only optional secrets the
// plan leaves unbound are reported as absent.
func secretSource(plan buildplan.Plan, release func(context.Context, string) ([]byte, error), mask *redact.Masker, delivered *deliveredValues) buildkit.SecretSource {
	byID := map[string]buildplan.PlanSecret{}
	for _, s := range plan.Secrets {
		byID[s.ID] = s
	}
	return func(ctx context.Context, id string) ([]byte, error) {
		ps, ok := byID[id]
		switch {
		case !ok:
			return nil, fmt.Errorf("unauthorized secret mount %q: it is not in the build plan", id)
		case ps.BindingID == "":
			return nil, buildkit.ErrSecretNotFound
		case release == nil:
			return nil, fmt.Errorf("secret %q: this worker cannot release secrets", id)
		}
		v, err := release(ctx, id)
		if err != nil {
			if errors.Is(err, isolation.ErrSecretUnbound) {
				return nil, fmt.Errorf("secret %q: the controller reports no binding for a secret the plan binds", id)
			}
			return nil, err
		}
		mask.Add(id, v)
		if delivered != nil {
			delivered.add(id, v)
		}
		return v, nil
	}
}

var proxyArgs = map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "FTP_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true}

func (p *Provider) Collect(context.Context, isolation.Env) error { return nil }

// Destroy stops the daemon and deletes everything the environment wrote, including the image archive, which the
// worker has already uploaded.
func (p *Provider) Destroy(_ context.Context, ie isolation.Env) error {
	p.mu.Lock()
	e, ok := p.envs[ie.ID]
	delete(p.envs, ie.ID)
	p.mu.Unlock()
	dir, daemonDir := filepath.Join(p.cfg.Dir, ie.AttemptID), filepath.Join(p.cfg.DaemonDir, ie.AttemptID)
	if ok {
		p.mu.Lock()
		p.built++
		p.mu.Unlock()
		if err := e.runner.Stop(); err != nil {
			return err
		}
		dir = e.dir
	} else if err := p.cleanup(daemonDir); err != nil {
		return err
	}
	if p.cfg.DaemonDir == p.cfg.Dir {
		if err := p.cleanup(filepath.Join(dir, "daemon")); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rootless-host: %s still exists after cleanup", dir)
	}
	return nil
}

// Environments lists every attempt with files under the worker or daemon directory, including orphans.
func (p *Provider) Environments(context.Context) ([]isolation.Env, error) {
	seen := map[string]bool{}
	var out []isolation.Env
	for _, root := range []string{p.cfg.Dir, p.cfg.DaemonDir} {
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, en := range entries {
			if en.IsDir() && !seen[en.Name()] {
				seen[en.Name()] = true
				out = append(out, isolation.Env{ID: en.Name(), AttemptID: en.Name(), Profile: isolation.RootlessHost})
			}
		}
	}
	return out, nil
}

func fileDigest(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// groupReachable reports, before any build, a daemon directory the build group cannot reach: every ancestor must be
// searchable by that group or by everyone, and the directory itself writable by the group.
func groupReachable(dir, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return fmt.Errorf("rootless-host: build group: %w", err)
	}
	gid, _ := strconv.Atoi(g.Gid)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for p := abs; ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("rootless-host: daemon-dir: %w", err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		mode := info.Mode().Perm()
		inGroup := int(st.Gid) == gid
		need := os.FileMode(0o001)
		if inGroup {
			need = 0o010
		}
		if p == abs && inGroup {
			need = 0o030
		}
		if mode&need != need && mode&0o001 == 0 {
			return fmt.Errorf("rootless-host: the build group %s cannot reach %s through %s (mode %v); make each directory searchable by the group", group, abs, p, mode)
		}
		if p == "/" {
			return nil
		}
	}
}

type maskedError struct {
	msg string
	err error
}

func (e maskedError) Error() string { return e.msg }
func (e maskedError) Unwrap() error { return e.err }

func (p *Provider) buildahOptions() buildah.Options {
	o := buildah.Options{Binary: p.cfg.BuildahBin, RunAs: p.opts.RunAs, Group: p.opts.Group, UserRuntime: p.runtime, Mirrors: p.cfg.Mirrors, InsecureMirrors: p.cfg.InsecureMirrors}
	if o.UserRuntime == "" && p.cfg.BuildUser != "" {
		if u, err := user.Lookup(p.cfg.BuildUser); err == nil {
			o.UserRuntime = filepath.Join("/run/user", u.Uid)
		}
	}
	return o
}

// cleanup removes a build directory a stopped worker may have left, with the engine that wrote it.
func (p *Provider) cleanup(dir string) error {
	if p.cfg.Engine == engine.Buildah {
		return buildah.Cleanup(p.buildahOptions(), dir)
	}
	return buildkit.Cleanup(p.cfg.Tools, p.opts, dir)
}

func launcherPresent(path string) bool {
	st, err := os.Stat(path)
	return path != "" && err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// placeLauncher copies the web launcher into the build's named context and logs its digest, which ties the image's
// launcher to this worker's Samuori release (ADR 0055). The binary is for the worker's architecture only.
func (p *Provider) placeLauncher(e *env, plan buildplan.Plan, req *engine.Request) (string, error) {
	if p.cfg.Engine != engine.BuildKit || !launcherPresent(p.cfg.WebLauncher) {
		return "", &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "this worker has no web launcher (samuori-web) for frontend builds"}
	}
	for _, pl := range plan.Platforms {
		if pl != "linux/"+runtime.GOARCH {
			return "", &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "a frontend build runs on a worker of its platform; this worker builds linux/" + runtime.GOARCH}
		}
	}
	dir := filepath.Join(e.dir, recipedef.LauncherContext)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	sum, err := copyVerified(p.cfg.WebLauncher, filepath.Join(dir, "samuori-web"), 0o755)
	if err != nil {
		return "", &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "web launcher: " + err.Error()}
	}
	req.NamedContexts = map[string]string{recipedef.LauncherContext: dir}
	return "web launcher: samuori-web " + sum + "\n", nil
}
