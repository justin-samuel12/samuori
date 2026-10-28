//go:build linux

package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/vmguest"
	"samuori.invalid/samuori/internal/vsock"
)

// guestOverhead is memory the guest OS needs on top of the build's own limit, which the guest enforces with a
// systemd scope.
const guestOverhead = 768 << 20

// vm is one disposable guest. Its directory holds the scratch disk, the vsock sockets and the console log; all of
// it is deleted with the VM.
type vm struct {
	id      string
	dir     string
	socket  string
	cmd     *exec.Cmd
	limits  isolation.Limits
	exited  chan struct{}
	waitErr error

	mu        sync.Mutex
	listeners []net.Listener
	handlers  *callbacks
}

func pidFile(dir string) string { return filepath.Join(dir, "vmm.pid") }

// boot starts a VM and waits until its agent answers. The VM has no network device; scratch is its only
// writable disk and is sized to the disk limit.
func (p *Provider) boot(ctx context.Context, id string, limits isolation.Limits) (*vm, error) {
	if err := p.img.Unchanged(); err != nil {
		return nil, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	dir := filepath.Join(p.cfg.Dir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	v := &vm{id: id, dir: dir, socket: filepath.Join(dir, "vsock.sock"), limits: limits, exited: make(chan struct{}), handlers: &callbacks{}}
	fail := func(err error) (*vm, error) {
		v.kill()
		_ = os.RemoveAll(dir)
		var f *isolation.Failure
		if errors.As(err, &f) {
			return nil, err
		}
		return nil, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	scratch := filepath.Join(dir, "scratch.img")
	f, err := os.OpenFile(scratch, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		err = f.Truncate(int64(limits.DiskBytes))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		return fail(err)
	}
	if err := v.listen(p); err != nil {
		return fail(err)
	}
	mem := (limits.MemoryBytes + guestOverhead) >> 20
	args := []string{
		"--kernel", p.img.kernel,
		"--cmdline", p.img.Cmdline,
		"--disk", "path=" + p.img.root + ",readonly=on,image_type=raw", "path=" + scratch + ",image_type=raw,sparse=off",
		"--cpus", "boot=" + strconv.Itoa(int(limits.VCPUs)),
		"--memory", "size=" + strconv.FormatUint(mem, 10) + "M",
		"--vsock", "cid=3,socket=" + v.socket,
		"--serial", "file=" + filepath.Join(dir, "console.log"),
		"--console", "off",
	}
	v.cmd = exec.Command(p.cfg.Binary, args...)
	v.cmd.Dir = dir
	v.cmd.Env = []string{}
	v.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	v.cmd.Stdout, v.cmd.Stderr = io.Discard, io.Discard
	if err := v.cmd.Start(); err != nil {
		return fail(err)
	}
	go func() {
		v.waitErr = v.cmd.Wait()
		close(v.exited)
	}()
	if err := os.WriteFile(pidFile(dir), []byte(strconv.Itoa(v.cmd.Process.Pid)), 0o600); err != nil {
		return fail(err)
	}
	deadline := time.Now().Add(p.cfg.BootTimeout)
	for {
		if err := v.ready(ctx); err == nil {
			return v, nil
		} else if time.Now().After(deadline) {
			return fail(fmt.Errorf("the guest did not become ready within %s: %v%s", p.cfg.BootTimeout, err, v.consoleTail()))
		}
		select {
		case <-v.exited:
			return fail(fmt.Errorf("the VMM exited during boot: %v%s", v.waitErr, v.consoleTail()))
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// consoleTail is the end of the guest console, for operators; it is guest output and is clipped and flattened.
func (v *vm) consoleTail() string {
	b, err := os.ReadFile(filepath.Join(v.dir, "console.log"))
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b) > 600 {
		b = b[len(b)-600:]
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, string(b))
	return "; console: " + s
}

func (v *vm) client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return vsock.DialGuest(ctx, v.socket, vmguest.AgentPort)
		},
		DisableKeepAlives: true,
	}}
}

func (v *vm) ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://guest/v1/ready", nil)
	resp, err := v.client(0).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("agent: %s", strings.TrimSpace(string(b)))
	}
	return nil
}

// listen opens the host ends of the guest's only routes out: callbacks, the egress proxy and each mirror.
func (v *vm) listen(p *Provider) error {
	cb, err := vsock.ListenHost(v.socket, vmguest.CallbackPort)
	if err != nil {
		return err
	}
	v.listeners = append(v.listeners, cb)
	srv := &http.Server{Handler: v.handlers.mux(p), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { _ = srv.Serve(cb) }()
	if p.cfg.Proxy != nil {
		pl, err := vsock.ListenHost(v.socket, vmguest.ProxyPort)
		if err != nil {
			return err
		}
		v.listeners = append(v.listeners, pl)
		go func() { _ = p.cfg.Proxy.HTTPServer().Serve(pl) }()
	}
	for _, m := range p.mirrors {
		ml, err := vsock.ListenHost(v.socket, m.HostPort())
		if err != nil {
			return err
		}
		v.listeners = append(v.listeners, ml)
		go forward(p.log, ml, p.cfg.Mirrors[m.Upstream])
	}
	return nil
}

// forward relays guest connections to one fixed mirror address; the guest cannot choose the destination.
func forward(log *slog.Logger, l net.Listener, target string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			d, err := (&net.Dialer{Timeout: 10 * time.Second}).Dial("tcp", target)
			if err != nil {
				log.Warn("mirror relay", "mirror", target, "error", err)
				return
			}
			defer d.Close()
			done := make(chan struct{}, 2)
			cp := func(dst, src net.Conn) {
				_, _ = io.Copy(dst, src)
				if cw, ok := dst.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				done <- struct{}{}
			}
			go cp(d, c)
			go cp(c, d)
			<-done
			<-done
		}()
	}
}

// kill ends the VMM and its process group and closes the host listeners.
func (v *vm) kill() {
	v.mu.Lock()
	ls := v.listeners
	v.listeners = nil
	v.mu.Unlock()
	for _, l := range ls {
		_ = l.Close()
	}
	if v.cmd == nil || v.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-v.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-v.exited:
	case <-time.After(10 * time.Second):
	}
}

func (v *vm) alive() bool {
	select {
	case <-v.exited:
		return false
	default:
		return true
	}
}

// killOrphan ends a VMM left by a previous worker process, identified by its pid file and its command line
// naming the environment directory.
func killOrphan(dir string) error {
	raw, err := os.ReadFile(pidFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return nil
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || !strings.Contains(string(cmdline), filepath.Join(dir, "vsock.sock")) {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for range 100 {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("cloud-hypervisor: VMM %d did not exit", pid)
}

// callbacks serve the guest's secret and mirror-token requests for the attempt the VM is assigned to. Before
// assignment (a warm VM) every request is refused.
type callbacks struct {
	mu   sync.Mutex
	spec *isolation.Spec
	seen func(id string, v []byte)
}

func (c *callbacks) assign(s *isolation.Spec, seen func(string, []byte)) {
	c.mu.Lock()
	c.spec, c.seen = s, seen
	c.mu.Unlock()
}

func (c *callbacks) current() (*isolation.Spec, func(string, []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spec, c.seen
}

func (c *callbacks) mux(p *Provider) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /v1/secret", func(w http.ResponseWriter, r *http.Request) {
		spec, seen := c.current()
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.ID == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if spec == nil || spec.ReleaseSecret == nil {
			http.Error(w, "this environment cannot release secrets", http.StatusForbidden)
			return
		}
		v, err := spec.ReleaseSecret(r.Context(), body.ID)
		switch {
		case errors.Is(err, isolation.ErrSecretUnbound):
			http.Error(w, "unbound", http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		if seen != nil {
			seen(body.ID, v)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(v)
	})
	m.HandleFunc("POST /v1/mirror-token", func(w http.ResponseWriter, r *http.Request) {
		spec, _ := c.current()
		var body struct {
			Mirror string   `json:"mirror"`
			Scopes []string `json:"scopes"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || len(body.Scopes) > 256 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		target, ok := p.relayTarget(body.Mirror)
		if spec == nil || spec.MirrorToken == nil || !ok {
			http.Error(w, "no mirror token for this environment", http.StatusForbidden)
			return
		}
		token, exp, at, err := spec.MirrorToken(r.Context(), target, body.Scopes)
		if err != nil {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expiresIn": exp, "issuedAt": at})
	})
	return m
}
