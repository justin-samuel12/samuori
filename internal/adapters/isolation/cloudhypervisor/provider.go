//go:build linux

package cloudhypervisor

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/adapters/isolation/rootlesshost"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/egress/proxy"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/redact"
	"samuori.invalid/samuori/internal/vmguest"
)

const Name = "kvm-cloud-hypervisor"

func init() {
	isolation.Register(Name, fromSettings)
}

// Config describes a vm-per-job worker host.
type Config struct {
	Dir    string
	Binary string
	// Image is the guest bundle's manifest (tools/vm-image).
	Image string
	Slots int
	// Warm VMs are booted ahead for builds with the default limits; each still serves one attempt only.
	Warm int
	// Max limits bound every VM; Default limits apply when the controller sends none larger.
	MaxVCPUs, DefaultVCPUs             uint32
	MaxMemoryBytes, DefaultMemoryBytes uint64
	MaxDiskBytes, DefaultDiskBytes     uint64
	Proxy                              *proxy.Server
	Mirrors                            map[string]string
	InsecureMirrors                    bool
	Tasks                              int
	BootTimeout                        time.Duration
	Log                                *slog.Logger
}

type held struct {
	vm   *vm
	spec isolation.Spec
	env  isolation.Env
}

type Provider struct {
	cfg     Config
	img     *Image
	digest  string
	mirrors []vmguest.Mirror
	log     *slog.Logger

	mu     sync.Mutex
	envs   map[string]*held
	warm   []*vm
	booted int
	closed bool
	wake   chan struct{}
}

func fromSettings(s map[string]string) (isolation.Provider, error) {
	num := func(key string, def int) int {
		if v, err := strconv.Atoi(s[key]); err == nil {
			return v
		}
		return def
	}
	gib := func(key string, def int) uint64 { return uint64(num(key, def)) << 30 }
	c := Config{Dir: s["dir"], Binary: s["vmm"], Image: s["vm-image"], Slots: num("slots", 1), Warm: num("warm-vms", 0),
		MaxVCPUs: uint32(num("vm-max-vcpus", 4)), DefaultVCPUs: uint32(num("vm-vcpus", 2)),
		MaxMemoryBytes: gib("vm-max-memory-gib", 8), DefaultMemoryBytes: gib("vm-memory-gib", 4),
		MaxDiskBytes: gib("vm-max-disk-gib", 40), DefaultDiskBytes: gib("vm-disk-gib", 20),
		InsecureMirrors: s["insecure-mirrors"] == "true", Tasks: num("max-processes", 2048)}
	if c.Binary == "" {
		c.Binary = "cloud-hypervisor"
	}
	if m := s["mirrors"]; m != "" {
		c.Mirrors = map[string]string{}
		for _, pair := range strings.Split(m, ",") {
			up, mirror, ok := strings.Cut(pair, "=")
			if !ok || up == "" || mirror == "" {
				return nil, fmt.Errorf("cloud-hypervisor: mirror %q must be upstream=mirror-host:port", pair)
			}
			c.Mirrors[up] = mirror
		}
	}
	if s["egress"] == "unrestricted" {
		return nil, errors.New("cloud-hypervisor: vm-per-job always uses strict egress; the guest has no network device")
	}
	c.Proxy = proxy.New(proxy.Options{})
	return New(c)
}

func New(c Config) (*Provider, error) {
	switch {
	case c.Dir == "":
		return nil, errors.New("cloud-hypervisor: the dir setting is required")
	case c.Image == "":
		return nil, errors.New("cloud-hypervisor: vm-image must name the guest image manifest")
	case c.Proxy == nil:
		return nil, errors.New("cloud-hypervisor: an egress proxy is required")
	case len(c.Mirrors) > vmguest.MaxMirrors:
		return nil, fmt.Errorf("cloud-hypervisor: at most %d mirrors", vmguest.MaxMirrors)
	case c.DefaultVCPUs > c.MaxVCPUs || c.DefaultMemoryBytes > c.MaxMemoryBytes || c.DefaultDiskBytes > c.MaxDiskBytes:
		return nil, errors.New("cloud-hypervisor: default VM limits exceed the maximums")
	}
	if c.Slots < 1 {
		c.Slots = 1
	}
	if c.Warm > c.Slots {
		c.Warm = c.Slots
	}
	if c.BootTimeout <= 0 {
		c.BootTimeout = 90 * time.Second
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	if _, err := exec.LookPath(c.Binary); err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: %w", err)
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: /dev/kvm: %w", err)
	} else {
		f.Close()
	}
	img, err := LoadImage(c.Image)
	if err != nil {
		return nil, fmt.Errorf("cloud-hypervisor: %w", err)
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return nil, err
	}
	p := &Provider{cfg: c, img: img, log: c.Log, envs: map[string]*held{}, wake: make(chan struct{}, 1)}
	ups := make([]string, 0, len(c.Mirrors))
	for up := range c.Mirrors {
		ups = append(ups, up)
	}
	sort.Strings(ups)
	for i, up := range ups {
		p.mirrors = append(p.mirrors, vmguest.Mirror{Upstream: up, Index: i})
	}
	p.digest = p.configDigest()
	if c.Warm > 0 {
		go p.keepWarm()
	}
	return p, nil
}

// ImageRef is the environment image reference workers pin for this bundle.
func (p *Provider) ImageRef() string { return "samuori-guest@" + p.img.Digest }

func (p *Provider) configDigest() string {
	h := sha256.New()
	bin, _ := exec.LookPath(p.cfg.Binary)
	vmm, _, _ := digestFile(bin)
	mirrors := make([]string, 0, len(p.cfg.Mirrors))
	for k, v := range p.cfg.Mirrors {
		mirrors = append(mirrors, k+"="+v)
	}
	sort.Strings(mirrors)
	fmt.Fprintf(h, "provider=%s\nimage=%s\nvmm=%s\nslots=%d\nwarm=%d\nmax=%d/%d/%d\ndefault=%d/%d/%d\nmirrors=%s\ninsecure=%t\ntasks=%d\n",
		Name, p.img.Digest, vmm, p.cfg.Slots, p.cfg.Warm, p.cfg.MaxVCPUs, p.cfg.MaxMemoryBytes, p.cfg.MaxDiskBytes,
		p.cfg.DefaultVCPUs, p.cfg.DefaultMemoryBytes, p.cfg.DefaultDiskBytes, strings.Join(mirrors, ","), p.cfg.InsecureMirrors, p.cfg.Tasks)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func (p *Provider) Name() string { return Name }

func (p *Provider) Capabilities(context.Context) (isolation.Capabilities, error) {
	p.mu.Lock()
	free := p.cfg.Slots - len(p.envs)
	p.mu.Unlock()
	return isolation.Capabilities{Provider: Name, Profiles: []isolation.Profile{isolation.VMPerJob}, OS: "linux", Architectures: []string{p.img.Arch},
		MaxVCPUs: p.cfg.MaxVCPUs, MaxMemoryBytes: p.cfg.MaxMemoryBytes, MaxDiskBytes: p.cfg.MaxDiskBytes, Startup: isolation.StartupSeconds,
		FreeSlots: uint32(max(free, 0)), StrictEgress: true, Engines: []string{"buildkit"}, ConfigDigest: p.digest}, nil
}

// limits fills defaults and refuses requests above the host's maximums.
func (p *Provider) limits(l isolation.Limits) (isolation.Limits, error) {
	if l.VCPUs == 0 {
		l.VCPUs = p.cfg.DefaultVCPUs
	}
	if l.MemoryBytes == 0 {
		l.MemoryBytes = p.cfg.DefaultMemoryBytes
	}
	if l.DiskBytes == 0 {
		l.DiskBytes = p.cfg.DefaultDiskBytes
	}
	if l.VCPUs > p.cfg.MaxVCPUs || l.MemoryBytes > p.cfg.MaxMemoryBytes || l.DiskBytes > p.cfg.MaxDiskBytes {
		return l, &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "the requested limits exceed this host's VM maximums"}
	}
	return l, nil
}

func (p *Provider) defaults(l isolation.Limits) bool {
	return l.VCPUs == p.cfg.DefaultVCPUs && l.MemoryBytes == p.cfg.DefaultMemoryBytes && l.DiskBytes == p.cfg.DefaultDiskBytes
}

// Prepare gives the attempt its own VM: a warm one booted for nobody in particular, or a fresh boot.
func (p *Provider) Prepare(ctx context.Context, s isolation.Spec) (isolation.Env, error) {
	if s.Profile != isolation.VMPerJob {
		return isolation.Env{}, &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "this provider offers only vm-per-job"}
	}
	if s.BaseImage != "" {
		if _, digest, _ := strings.Cut(s.BaseImage, "@"); digest != p.img.Digest {
			return isolation.Env{}, &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "this worker's guest image is " + p.ImageRef() + ", not " + s.BaseImage}
		}
	}
	limits, err := p.limits(s.Limits)
	if err != nil {
		return isolation.Env{}, err
	}
	p.mu.Lock()
	if _, busy := p.envs[s.AttemptID]; busy {
		p.mu.Unlock()
		return isolation.Env{}, &isolation.Failure{Class: isolation.ErrOccupied, Detail: "an environment for this attempt already exists"}
	}
	if len(p.envs) >= p.cfg.Slots {
		p.mu.Unlock()
		return isolation.Env{}, &isolation.Failure{Class: isolation.ErrOccupied, Detail: "every VM slot is busy"}
	}
	h := &held{spec: s}
	p.envs[s.AttemptID] = h
	var v *vm
	if p.defaults(limits) && len(p.warm) > 0 {
		v, p.warm = p.warm[0], p.warm[1:]
	}
	p.mu.Unlock()
	if v != nil && !v.alive() {
		p.discard(v)
		v = nil
	}
	if v == nil {
		v, err = p.boot(ctx, "vm-"+strings.ToLower(ids.New()), limits)
		if err != nil {
			p.mu.Lock()
			delete(p.envs, s.AttemptID)
			p.mu.Unlock()
			return isolation.Env{}, err
		}
	}
	p.poke()
	wall := s.Limits.WallClock
	if wall <= 0 {
		wall = time.Duration(buildplan.DefaultLimits.WallClockSecs) * time.Second
	}
	h.vm = v
	h.env = isolation.Env{ID: v.id, AttemptID: s.AttemptID, Profile: isolation.VMPerJob, Created: time.Now(), Deadline: time.Now().Add(wall)}
	p.mu.Lock()
	p.booted++
	p.mu.Unlock()
	return h.env, nil
}

func (p *Provider) get(e isolation.Env) (*held, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.envs[e.AttemptID]
	if !ok || h.vm == nil || h.vm.id != e.ID {
		return nil, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "environment not found"}
	}
	return h, nil
}

func (p *Provider) relayTarget(relay string) (string, bool) {
	for _, m := range p.mirrors {
		if m.Relay() == relay {
			return p.cfg.Mirrors[m.Upstream], true
		}
	}
	return "", false
}

// Run sends the plan and source to the guest and reads back the image. Everything the guest returns is
// untrusted: entries are bounded, the digest is recomputed here and the canary runs here, where the released
// values are known.
func (p *Provider) Run(ctx context.Context, e isolation.Env, raw []byte) (isolation.RunResult, error) {
	var sess *proxy.Session
	res, err := p.run(ctx, e, raw, &sess)
	if sess != nil {
		res.Egress = rootlesshost.ProxyReport(sess)
	}
	return res, err
}

func (p *Provider) run(ctx context.Context, e isolation.Env, raw []byte, egressSess **proxy.Session) (isolation.RunResult, error) {
	h, err := p.get(e)
	if err != nil {
		return isolation.RunResult{}, err
	}
	plan, err := buildplan.Decode(raw)
	if err != nil {
		return isolation.RunResult{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "unreadable plan"}
	}
	sess, err := p.cfg.Proxy.Open(plan.Egress.Allow)
	if err != nil {
		return isolation.RunResult{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	defer sess.Close()
	*egressSess = sess
	mask := redact.New()
	var vmu sync.Mutex
	delivered := map[string][]byte{}
	defer func() {
		vmu.Lock()
		for k, v := range delivered {
			clear(v)
			delete(delivered, k)
		}
		vmu.Unlock()
	}()
	h.vm.handlers.assign(&h.spec, func(id string, v []byte) {
		mask.Add(id, v)
		vmu.Lock()
		delivered[id] = append([]byte(nil), v...)
		vmu.Unlock()
	})
	defer h.vm.handlers.assign(nil, nil)
	req := vmguest.RunRequest{AttemptID: h.spec.AttemptID, SecretBearing: h.spec.SecretBearing, TrustDomain: h.spec.TrustDomain, Limits: h.vm.limits,
		ProxyURL: sess.URL(fmt.Sprintf("%s:%d", vmguest.RelayIP, vmguest.ProxyRelayPort)), Mirrors: p.mirrors, InsecureMirrors: p.cfg.InsecureMirrors,
		Tasks: p.cfg.Tasks, Cache: h.spec.CacheDir != ""}
	ctx, cancel := context.WithDeadline(ctx, e.Deadline)
	defer cancel()
	out := h.spec.OutputDir
	if out == "" {
		out = h.vm.dir
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := writeRun(tw, req, raw, h.spec)
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	res, files, err := p.exchange(ctx, h.vm, "/v1/run", "application/x-tar", pr, out)
	pr.CloseWithError(errors.New("exchange finished"))
	logs := mask.Mask([]byte(clipLog(res.Logs) + sess.Summary()))
	if err != nil {
		return isolation.RunResult{Logs: logs}, p.classify(ctx, h.vm, err)
	}
	if res.Class != "" {
		return isolation.RunResult{Logs: logs}, vmguest.Err(res.Class, string(mask.Mask([]byte(clipText(res.Detail)))))
	}
	image, ok := files[vmguest.EntryImage]
	if !ok {
		return isolation.RunResult{Logs: logs}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "the guest returned no image"}
	}
	digest, _, err := digestFile(image)
	if err != nil || digest != res.ArtifactDigest {
		return isolation.RunResult{Logs: logs}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "the image digest does not match the guest's report"}
	}
	var checks []isolation.Check
	for _, c := range res.Checks {
		if c.ID != isolation.CheckCanary && strings.HasPrefix(c.ID, isolation.CheckRuntimePref) && len(checks) < 16 {
			checks = append(checks, boundCheck(c))
		}
	}
	for _, s := range plan.Secrets {
		if s.BindingID != "" {
			vmu.Lock()
			values := make(map[string][]byte, len(delivered))
			for k, v := range delivered {
				values[k] = v
			}
			vmu.Unlock()
			checks = append([]isolation.Check{rootlesshost.Canary(image, values)}, checks...)
			break
		}
	}
	result := isolation.RunResult{ArtifactDigest: digest, ArtifactFile: image, Logs: logs, Checks: checks}
	if cache, ok := files[vmguest.EntryCache]; ok {
		if d, _, err := digestFile(cache); err == nil && d == res.CacheDigest {
			result.CacheFile, result.CacheDigest = cache, d
		} else {
			_ = os.Remove(cache)
		}
	}
	return result, nil
}

func writeRun(tw *tar.Writer, req vmguest.RunRequest, plan []byte, spec isolation.Spec) error {
	reqJSON, _ := json.Marshal(req)
	if err := vmguest.WriteBytes(tw, vmguest.EntryRequest, reqJSON); err != nil {
		return err
	}
	if err := vmguest.WriteBytes(tw, vmguest.EntryPlan, plan); err != nil {
		return err
	}
	if err := vmguest.WriteTree(tw, spec.SourceDir, vmguest.SourcePrefix); err != nil {
		return err
	}
	if spec.CacheDir != "" {
		return vmguest.WriteTree(tw, spec.CacheDir, vmguest.CachePrefix)
	}
	return nil
}

// classify turns a broken exchange into a failure class: the wall clock ran out, the VM died, or the transport
// failed.
func (p *Provider) classify(ctx context.Context, v *vm, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		v.kill()
		return &isolation.Failure{Class: isolation.ErrTimeout, Detail: "the build exceeded its wall-clock limit; the VM was destroyed"}
	case !v.alive():
		return &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "the VM stopped during the build" + v.consoleTail()}
	}
	return &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "guest exchange failed: " + clipText(err.Error())}
}

// exchange posts a tar stream to the agent and reads its reply: result.json, then at most an image and a cache
// archive, written below out and bounded by the VM's disk size.
func (p *Provider) exchange(ctx context.Context, v *vm, path, contentType string, body io.Reader, out string) (vmguest.RunResult, map[string]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://guest"+path, body)
	req.Header.Set("Content-Type", contentType)
	resp, err := v.client(0).Do(req)
	if err != nil {
		return vmguest.RunResult{}, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return vmguest.RunResult{}, nil, fmt.Errorf("agent answered %d: %s", resp.StatusCode, clipText(string(b)))
	}
	var res vmguest.RunResult
	files := map[string]string{}
	tr := tar.NewReader(resp.Body)
	budget := int64(v.limits.DiskBytes)
	for i := 0; ; i++ {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, nil, err
		}
		if hd.Typeflag != tar.TypeReg || hd.Size < 0 {
			return res, nil, fmt.Errorf("unexpected entry type in the guest reply")
		}
		switch {
		case i == 0 && hd.Name == vmguest.EntryResult:
			if hd.Size > vmguest.MaxResultSize {
				return res, nil, errors.New("the guest result is too large")
			}
			if err := json.NewDecoder(io.LimitReader(tr, vmguest.MaxResultSize)).Decode(&res); err != nil {
				return res, nil, fmt.Errorf("guest result: %w", err)
			}
		case i > 0 && (hd.Name == vmguest.EntryImage || hd.Name == vmguest.EntryCache) && files[hd.Name] == "":
			if hd.Size > budget {
				return res, nil, errors.New("the guest reply exceeds the VM's disk size")
			}
			budget -= hd.Size
			dst := filepath.Join(out, hd.Name)
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return res, nil, err
			}
			_, err = io.CopyN(f, tr, hd.Size)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return res, nil, err
			}
			files[hd.Name] = dst
		default:
			return res, nil, fmt.Errorf("unexpected entry %q in the guest reply", clipText(hd.Name))
		}
	}
	return res, files, nil
}

func boundCheck(c isolation.Check) isolation.Check {
	c.ID, c.Category, c.Outcome, c.Reason = clipN(c.ID, 64), clipN(c.Category, 64), clipN(c.Outcome, 32), clipN(c.Reason, 1000)
	if len(c.Findings) > 100 {
		c.Findings = c.Findings[:100]
	}
	return c
}

func clipN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func clipText(s string) string { return clipN(s, 2000) }

const maxLog = 4 << 20

func clipLog(s string) string {
	if len(s) > maxLog {
		return "[earlier output truncated]\n" + s[len(s)-maxLog:]
	}
	return s
}

func (p *Provider) Collect(context.Context, isolation.Env) error { return nil }

// Destroy kills the VM and deletes its directory, scratch disk included, then verifies nothing is left.
func (p *Provider) Destroy(_ context.Context, e isolation.Env) error {
	p.mu.Lock()
	h, ok := p.envs[e.AttemptID]
	if ok && (h.vm == nil || h.vm.id == e.ID) {
		delete(p.envs, e.AttemptID)
	} else {
		ok = false
	}
	p.mu.Unlock()
	p.poke()
	if ok && h.vm != nil {
		p.discard(h.vm)
		return verifyGone(h.vm.dir)
	}
	if e.ID == "" || !filepath.IsLocal(e.ID) {
		return nil
	}
	dir := filepath.Join(p.cfg.Dir, e.ID)
	if err := killOrphan(dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return verifyGone(dir)
}

func (p *Provider) discard(v *vm) {
	v.kill()
	_ = os.RemoveAll(v.dir)
}

func verifyGone(dir string) error {
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cloud-hypervisor: %s still exists after destruction", dir)
	}
	return nil
}

// Environments lists VM directories this process does not hold as warm VMs, including those a previous worker
// process left behind.
func (p *Provider) Environments(context.Context) ([]isolation.Env, error) {
	entries, err := os.ReadDir(p.cfg.Dir)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	warm := map[string]bool{}
	for _, v := range p.warm {
		warm[v.id] = true
	}
	byVM := map[string]string{}
	for attempt, h := range p.envs {
		if h.vm != nil {
			byVM[h.vm.id] = attempt
		}
	}
	p.mu.Unlock()
	var out []isolation.Env
	for _, en := range entries {
		if !en.IsDir() || warm[en.Name()] {
			continue
		}
		attempt := byVM[en.Name()]
		if attempt == "" {
			attempt = en.Name()
		}
		out = append(out, isolation.Env{ID: en.Name(), AttemptID: attempt, Profile: isolation.VMPerJob})
	}
	return out, nil
}

func (p *Provider) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// keepWarm boots VMs ahead while slots are free. A warm VM is identical to one booted on demand and is never
// reused: it leaves the pool for one attempt and is destroyed with it.
func (p *Provider) keepWarm() {
	for {
		p.mu.Lock()
		closed := p.closed
		need := len(p.warm) < p.cfg.Warm && len(p.warm)+len(p.envs) < p.cfg.Slots+p.cfg.Warm
		p.mu.Unlock()
		if closed {
			return
		}
		if need {
			l, _ := p.limits(isolation.Limits{})
			v, err := p.boot(context.Background(), "vm-"+strings.ToLower(ids.New()), l)
			if err != nil {
				p.log.Warn("warm VM", "error", err)
				time.Sleep(10 * time.Second)
				continue
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				p.discard(v)
				return
			}
			p.warm = append(p.warm, v)
			p.mu.Unlock()
			continue
		}
		select {
		case <-p.wake:
		case <-time.After(30 * time.Second):
		}
	}
}

// Close destroys warm VMs; environments held for attempts are destroyed by their owners.
func (p *Provider) Close() {
	p.mu.Lock()
	p.closed = true
	warm := p.warm
	p.warm = nil
	p.mu.Unlock()
	p.poke()
	for _, v := range warm {
		p.discard(v)
	}
}
