//go:build linux

package vmguest

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"samuori.invalid/samuori/internal/adapters/build/buildkit"
	"samuori.invalid/samuori/internal/adapters/isolation/rootlesshost"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/egress/firewall"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/vsock"
)

// Guest layout, prepared by the image (tools/vm-image).
const (
	StateDir   = "/var/lib/samuori"
	BuildUser  = "samuori-build"
	BuildGroup = "samuori"
	relayIface = "samuori0"
)

type agent struct {
	log  *slog.Logger
	self string

	mu   sync.Mutex
	used bool
}

// Main runs the agent until the VM is destroyed. It serves one build or one self-test, never both or more.
func Main(log *slog.Logger) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	l, err := vsock.Listen(AgentPort)
	if err != nil {
		return fmt.Errorf("vsock listen: %w", err)
	}
	a := &agent{log: log, self: self}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ready", a.ready)
	mux.HandleFunc("POST /v1/run", a.run)
	mux.HandleFunc("POST /v1/selftest", a.selftest)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	return srv.Serve(l)
}

// ready answers once the build user's systemd manager is up, so per-build scopes can be created.
func (a *agent) ready(w http.ResponseWriter, _ *http.Request) {
	u, err := user.Lookup(BuildUser)
	if err == nil {
		_, err = os.Stat(filepath.Join("/run/user", u.Uid, "systemd", "private"))
	}
	if err != nil {
		http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *agent) claim() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.used {
		return false
	}
	a.used = true
	return true
}

func (a *agent) run(w http.ResponseWriter, r *http.Request) {
	if !a.claim() {
		http.Error(w, "this VM already served a build", http.StatusConflict)
		return
	}
	res, files := a.build(r.Context(), r.Body)
	reply(w, res, files)
}

func (a *agent) selftest(w http.ResponseWriter, r *http.Request) {
	if !a.claim() {
		http.Error(w, "this VM already served a build", http.StatusConflict)
		return
	}
	var req RunRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, stop, err := a.provider(r.Context(), req)
	if err != nil {
		reply(w, RunResult{Class: ClassInfrastructure, Detail: err.Error()}, nil)
		return
	}
	defer stop()
	reply(w, RunResult{Checks: p.SelfTest(r.Context())}, nil)
}

func reply(w http.ResponseWriter, res RunResult, files map[string]string) {
	raw, _ := json.Marshal(res)
	w.Header().Set("Content-Type", "application/x-tar")
	tw := tar.NewWriter(w)
	if err := WriteBytes(tw, EntryResult, raw); err != nil {
		return
	}
	for _, name := range []string{EntryImage, EntryCache} {
		path, ok := files[name]
		if !ok {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return
		}
		info, err := f.Stat()
		if err == nil {
			err = WriteFile(tw, name, info.Size(), f)
		}
		f.Close()
		if err != nil {
			return
		}
	}
	_ = tw.Close()
}

func failed(class string, err error) (RunResult, map[string]string) {
	return RunResult{Class: class, Detail: clip(err.Error())}, nil
}

func (a *agent) build(ctx context.Context, body io.Reader) (RunResult, map[string]string) {
	work := filepath.Join(StateDir, "work")
	src, cache, out := filepath.Join(work, "source"), filepath.Join(work, "cache"), filepath.Join(work, "out")
	for _, d := range []string{src, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return failed(ClassInfrastructure, err)
		}
	}
	var req RunRequest
	var plan []byte
	tr := tar.NewReader(body)
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		return failed(ClassInfrastructure, err)
	}
	defer srcRoot.Close()
	var cacheRoot *os.Root
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return failed(ClassInfrastructure, fmt.Errorf("reading the run stream: %w", err))
		}
		switch {
		case h.Name == EntryRequest:
			err = json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&req)
		case h.Name == EntryPlan:
			plan, err = io.ReadAll(io.LimitReader(tr, 16<<20))
		case strings.HasPrefix(h.Name, SourcePrefix):
			h.Name = strings.TrimPrefix(h.Name, SourcePrefix)
			err = ExtractEntry(srcRoot, h, tr)
		case strings.HasPrefix(h.Name, CachePrefix):
			if cacheRoot == nil {
				if err = os.MkdirAll(cache, 0o755); err == nil {
					cacheRoot, err = os.OpenRoot(cache)
				}
			}
			if err == nil {
				h.Name = strings.TrimPrefix(h.Name, CachePrefix)
				err = ExtractEntry(cacheRoot, h, tr)
			}
		default:
			err = fmt.Errorf("unexpected entry %q", h.Name)
		}
		if err != nil {
			return failed(ClassInfrastructure, err)
		}
	}
	if cacheRoot != nil {
		cacheRoot.Close()
	}
	if req.AttemptID == "" || plan == nil {
		return failed(ClassInfrastructure, errors.New("the run stream has no request or plan"))
	}
	p, stop, err := a.provider(ctx, req)
	if err != nil {
		return failed(ClassInfrastructure, err)
	}
	defer stop()
	spec := isolation.Spec{AttemptID: req.AttemptID, SourceDir: src, OutputDir: out, Profile: isolation.RootlessDedicatedVM, Limits: req.Limits,
		SecretBearing: req.SecretBearing, TrustDomain: req.TrustDomain, ReleaseSecret: releaseSecret, MirrorToken: mirrorToken}
	if req.Cache && cacheRoot != nil {
		spec.CacheDir = cache
	}
	env, err := p.Prepare(ctx, spec)
	if err != nil {
		return failed(ClassOf(err), err)
	}
	defer p.Destroy(context.WithoutCancel(ctx), env)
	res, err := p.Run(ctx, env, plan)
	out2 := RunResult{Logs: string(res.Logs), ArtifactDigest: res.ArtifactDigest, CacheDigest: res.CacheDigest, Checks: res.Checks}
	if err != nil {
		out2.Class, out2.Detail = ClassOf(err), clip(err.Error())
		if out2.Class == ClassBuild && diskFull(err) {
			out2.Class, out2.Detail = ClassResourceLimit, "the build exceeded its disk limit"
		}
		return out2, nil
	}
	files := map[string]string{EntryImage: res.ArtifactFile}
	if res.CacheFile != "" {
		files[EntryCache] = res.CacheFile
	}
	return out2, files
}

// diskFull reports a build that failed because the scratch disk, which is the attempt's disk quota, filled up.
func diskFull(err error) bool {
	if strings.Contains(err.Error(), "no space left on device") {
		return true
	}
	var st unix.Statfs_t
	if unix.Statfs(StateDir, &st) != nil || st.Blocks == 0 {
		return false
	}
	return st.Bavail*100/st.Blocks < 2
}

// provider builds the rootless provider for this VM: strict egress through the host relays, the firewall
// confining the build user to them, and systemd limits for the daemon.
func (a *agent) provider(ctx context.Context, req RunRequest) (*rootlesshost.Provider, func(), error) {
	if len(req.Mirrors) > MaxMirrors {
		return nil, nil, errors.New("too many mirrors")
	}
	if req.ProxyURL == "" {
		return nil, nil, errors.New("vm-per-job builds need the host's egress proxy")
	}
	if err := relayInterface(); err != nil {
		return nil, nil, err
	}
	var stops []func()
	stop := func() {
		for _, s := range stops {
			s()
		}
	}
	allow := []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr(RelayIP), ProxyRelayPort)}
	relays := []struct {
		addr string
		port uint32
	}{{allow[0].String(), ProxyPort}}
	mirrors := map[string]string{}
	for _, m := range req.Mirrors {
		if m.Index < 0 || m.Index >= MaxMirrors || m.Upstream == "" {
			return nil, nil, fmt.Errorf("invalid mirror %+v", m)
		}
		mirrors[m.Upstream] = m.Relay()
		allow = append(allow, netip.MustParseAddrPort(m.Relay()))
		relays = append(relays, struct {
			addr string
			port uint32
		}{m.Relay(), m.HostPort()})
	}
	for _, r := range relays {
		s, err := relay(ctx, a.log, r.addr, r.port)
		if err != nil {
			stop()
			return nil, nil, err
		}
		stops = append(stops, s)
	}
	u, err := user.Lookup(BuildUser)
	if err != nil {
		stop()
		return nil, nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	rules, err := firewall.Ruleset(firewall.Config{BuildUID: uid, Allow: allow})
	if err != nil {
		stop()
		return nil, nil, err
	}
	nft := exec.CommandContext(ctx, "nft", "-f", "-")
	nft.Stdin = strings.NewReader(rules)
	if out, err := nft.CombinedOutput(); err != nil {
		stop()
		return nil, nil, fmt.Errorf("nft: %v: %s", err, clip(string(out)))
	}
	proxyURL := req.ProxyURL
	tasks := req.Tasks
	if tasks <= 0 {
		tasks = 2048
	}
	p, err := rootlesshost.NewFromConfig(rootlesshost.Config{Dir: filepath.Join(StateDir, "envs"), DaemonDir: filepath.Join(StateDir, "daemons"), Tools: buildkit.DefaultTools(),
		Slots: 1, BuildUser: BuildUser, BuildGroup: BuildGroup, Mirrors: mirrors, InsecureMirrors: req.InsecureMirrors, ProbeBinary: a.self,
		Profile: isolation.RootlessDedicatedVM, CgroupLimits: true, Dedicated: true, Tasks: tasks,
		Egress: func([]buildplan.EgressRule) (rootlesshost.EgressSession, error) {
			return hostSession{url: proxyURL}, nil
		}})
	if err != nil {
		stop()
		return nil, nil, err
	}
	return p, stop, nil
}

// hostSession is the session the host opened for this build; its decisions are logged on the host.
type hostSession struct{ url string }

func (s hostSession) URL() string     { return s.url }
func (s hostSession) Summary() string { return "" }

func (s hostSession) Report() isolation.EgressReport { return isolation.EgressReport{} }
func (s hostSession) Close()                         {}

func relayInterface() error {
	if _, err := net.InterfaceByName(relayIface); err == nil {
		return nil
	}
	for _, args := range [][]string{
		{"link", "add", relayIface, "type", "dummy"},
		{"addr", "add", RelayIP + "/32", "dev", relayIface},
		{"link", "set", relayIface, "up"},
	} {
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("ip %s: %v: %s", strings.Join(args, " "), err, clip(string(out)))
		}
	}
	return nil
}

// relay forwards each TCP connection on addr to the host's vsock port.
func relay(ctx context.Context, log *slog.Logger, addr string, port uint32) (func(), error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				h, err := vsock.Dial(vsock.HostCID, port, 10*time.Second)
				if err != nil {
					log.Warn("relay", "addr", addr, "error", err)
					return
				}
				defer h.Close()
				pipe(c, h)
			}()
		}
	}()
	return func() { l.Close() }, nil
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

var callbackClient = &http.Client{Transport: &http.Transport{
	DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return vsock.Dial(vsock.HostCID, CallbackPort, 10*time.Second)
	},
	DisableKeepAlives: true,
}, Timeout: 2 * time.Minute}

func callback(ctx context.Context, path string, body any) (*http.Response, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://host"+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return callbackClient.Do(req)
}

func releaseSecret(ctx context.Context, id string) ([]byte, error) {
	resp, err := callback(ctx, "/v1/secret", map[string]string{"id": id})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	switch {
	case err != nil:
		return nil, err
	case resp.StatusCode == http.StatusNotFound:
		return nil, isolation.ErrSecretUnbound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the host refused secret %q: %s", id, clip(string(b)))
	}
	return b, nil
}

type tokenReply struct {
	Token     string    `json:"token"`
	ExpiresIn int64     `json:"expiresIn"`
	IssuedAt  time.Time `json:"issuedAt"`
}

func mirrorToken(ctx context.Context, mirror string, scopes []string) (string, int64, time.Time, error) {
	resp, err := callback(ctx, "/v1/mirror-token", map[string]any{"mirror": mirror, "scopes": scopes})
	if err != nil {
		return "", 0, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, time.Time{}, fmt.Errorf("the host refused a mirror token: %s", clip(string(b)))
	}
	var t tokenReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&t); err != nil {
		return "", 0, time.Time{}, err
	}
	return t.Token, t.ExpiresIn, t.IssuedAt, nil
}

func clip(s string) string {
	if len(s) > 2000 {
		return s[len(s)-2000:]
	}
	return s
}
