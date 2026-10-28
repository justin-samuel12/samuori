package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	s3store "samuori.invalid/samuori/internal/adapters/storage/s3"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/setupcli"
	"samuori.invalid/samuori/internal/topology"
)

const rdsGlobalBundle = "https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem"

// setupCommand is `samuori-server setup`, run as root through the release folder's samuori-setup (ADR 0045).
func setupCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	release := fs.String("release", "", "the release folder holding install.sh (default: this binary's folder)")
	answers := fs.String("answers", "", "answers file (.yml, root-owned, mode 0600) for a non-interactive setup")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("setup takes no arguments; secrets are asked for or read from --answers, never from the command line")
	}
	if os.Geteuid() != 0 {
		return errors.New("run as root: sudo ./samuori-setup")
	}
	dir := *release
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		dir = filepath.Dir(exe)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "install.sh")); err != nil {
		return fmt.Errorf("%s is not a release folder: %w", dir, err)
	}
	s := &setupcli.Setup{Host: &setupHost{}, Out: stdout, ReleaseDir: dir}
	s.Installer = &releaseInstaller{dir: dir}
	if *answers != "" {
		return s.FromFile(ctx, *answers, 0)
	}
	fmt.Fprintln(stdout, "Samuori setup: each answer is checked before the next question; nothing is installed before the summary.")
	return s.Interactive(ctx, newTerminal(stdin, stdout))
}

type terminal struct {
	in  *bufio.Reader
	out io.Writer
}

func newTerminal(in io.Reader, out io.Writer) *terminal {
	return &terminal{in: bufio.NewReader(in), out: out}
}

func (t *terminal) Ask(q, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(t.out, "%s [%s]: ", q, def)
	} else {
		fmt.Fprintf(t.out, "%s: ", q)
	}
	line, err := t.in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", err
	}
	if v := strings.TrimSpace(line); v != "" {
		return v, nil
	}
	return def, nil
}

// Secret turns terminal echo off while the answer is typed and refuses input that is not a terminal.
func (t *terminal) Secret(q string) (string, error) {
	if err := stty("-echo"); err != nil {
		return "", errors.New("passwords are read from a terminal only; use --answers for automation")
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			_ = stty("echo")
			os.Exit(130)
		case <-done:
		}
	}()
	defer func() {
		close(done)
		signal.Stop(sig)
		_ = stty("echo")
		fmt.Fprintln(t.out)
	}()
	fmt.Fprintf(t.out, "%s: ", q)
	line, err := t.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func stty(arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

type setupHost struct {
	once   sync.Once
	token  string
	client *http.Client
}

func (h *setupHost) imds(ctx context.Context, path string) string {
	h.once.Do(func() {
		h.client = &http.Client{Timeout: 2 * time.Second}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
		req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
		if resp, err := h.client.Do(req); err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				h.token = string(b)
			}
		}
	})
	if h.token == "" {
		return ""
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://169.254.169.254/latest/meta-data/"+path, nil)
	req.Header.Set("X-aws-ec2-metadata-token", h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (h *setupHost) OnAWS(ctx context.Context) bool {
	if vendor, err := os.ReadFile("/sys/devices/virtual/dmi/id/sys_vendor"); err == nil && strings.Contains(string(vendor), "Amazon") {
		return true
	}
	return h.imds(ctx, "instance-id") != ""
}

func (h *setupHost) AWSRegion(ctx context.Context) string { return h.imds(ctx, "placement/region") }

// DefaultHostname is the OS hostname, qualified with a search domain when it resolves to this host, before the
// cloud metadata name.
func (h *setupHost) DefaultHostname(ctx context.Context) string {
	name, _ := os.Hostname()
	f := setupcli.HostFacts{OSName: name, Resolve: func(n string) []net.IP {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		ips, _ := net.DefaultResolver.LookupIP(c, "ip", n)
		return ips
	}}
	if rc, err := os.Open("/etc/resolv.conf"); err == nil {
		f.SearchDomains = setupcli.SearchDomains(rc)
		rc.Close()
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				f.LocalAddrs = append(f.LocalAddrs, n.IP)
			}
		}
	}
	if own, ok := setupcli.OwnName(f); ok {
		return own
	}
	f.MetadataName = h.imds(ctx, "local-hostname")
	return setupcli.PickHostname(f)
}

func (h *setupHost) Resolve(ctx context.Context, name string) error {
	if net.ParseIP(name) != nil {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(c, name)
	if err == nil && len(addrs) == 0 {
		err = errors.New("no addresses")
	}
	return err
}

func (h *setupHost) DownloadRDSBundle(ctx context.Context, path string) error {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, rdsGlobalBundle, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", rdsGlobalBundle, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if n := countCerts(raw); n == 0 {
		return errors.New("the download holds no certificate")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func countCerts(raw []byte) int {
	n := 0
	for {
		var b *pem.Block
		if b, raw = pem.Decode(raw); b == nil {
			return n
		}
		if _, err := x509.ParseCertificate(b.Bytes); b.Type == "CERTIFICATE" && err == nil {
			n++
		}
	}
}

// CheckDatabase signs in, requires PostgreSQL 16 to 18, and requires a database that holds no cluster yet.
func (h *setupHost) CheckDatabase(ctx context.Context, dsn string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	d, err := db.Open(c, dsn)
	if err != nil {
		return "", err
	}
	defer d.Close()
	if err := d.Ping(c); err != nil {
		return "", fmt.Errorf("signing in failed: %w", err)
	}
	var num, version string
	if err := d.SQL.QueryRowContext(c, `SELECT current_setting('server_version_num'), current_setting('server_version')`).Scan(&num, &version); err != nil {
		return "", fmt.Errorf("reading the server version: %w", err)
	}
	n, _ := strconv.Atoi(num)
	if n < 160000 || n >= 190000 {
		return "", fmt.Errorf("PostgreSQL %s is not supported; use 16, 17 or 18", version)
	}
	if err := checkDatabase(dsn, true); err != nil {
		return "", err
	}
	return "PostgreSQL " + version, nil
}

func (h *setupHost) CheckStorage(ctx context.Context, s setupcli.S3) error {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := s3store.New(c, s3store.Config{Bucket: s.Bucket, Region: s.Region, Prefix: s.Prefix})
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	key := "samuori-setup-probe/" + hex.EncodeToString(nonce)
	body := []byte("samuori-setup probe " + hex.EncodeToString(nonce))
	if err := r.Put(c, key, bytes.NewReader(body), int64(len(body))); err != nil {
		return fmt.Errorf("writing a probe object: %w", err)
	}
	var got bytes.Buffer
	readErr := r.Get(c, key, &got)
	if err := r.Delete(c, key); err != nil {
		return fmt.Errorf("deleting the probe object %s: %w", key, err)
	}
	if readErr != nil {
		return fmt.Errorf("reading the probe object back: %w", readErr)
	}
	if !bytes.Equal(got.Bytes(), body) {
		return errors.New("the probe object read back differs from what was written")
	}
	return nil
}

// releaseInstaller drives the release's install.sh with the checked answers; secrets travel in a private file
// and the environment, never on a command line.
type releaseInstaller struct{ dir string }

func (r *releaseInstaller) Install(ctx context.Context, a setupcli.Answers, out io.Writer) error {
	tmp, err := os.MkdirTemp("", "samuori-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	pwFile := filepath.Join(tmp, "admin-password")
	if err := os.WriteFile(pwFile, []byte(a.Admin.Password), 0o600); err != nil {
		return err
	}
	if err := writePlan("/etc/samuori", a.Plan()); err != nil {
		return err
	}
	args := []string{"--release", r.dir}
	if a.ReleaseSigners != "" {
		signers, err := filepath.Abs(a.ReleaseSigners)
		if err != nil {
			return err
		}
		args = append(args, "--release-signers", signers)
	}
	setupArgs := []string{"--external-url", strings.TrimSuffix(a.PortalURL, "/"),
		"--organization", a.Organization, "--admin-username", a.Admin.Username, "--admin-email", a.Admin.Email,
		"--admin-password-file", pwFile}
	if a.S3.Bucket != "" {
		setupArgs = append(setupArgs, "--s3-bucket", a.S3.Bucket, "--s3-region", a.S3.Region)
		if a.S3.Prefix != "" {
			setupArgs = append(setupArgs, "--s3-prefix", a.S3.Prefix)
		}
	}
	env := withoutEnv(os.Environ(), "SAMUORI_DATABASE")
	if a.Deployment == setupcli.Cluster {
		args = append(args, "--role", "controller", "--host", a.Host, "init")
		args = append(args, setupArgs...)
		env = append(env, "SAMUORI_DATABASE="+a.Database.DSN())
	} else {
		args = append(args, "--confirm-dedicated-vm", "--worker-hostnames", "localhost,127.0.0.1,"+a.Host)
		args = append(args, setupArgs...)
	}
	if err := runInstaller(ctx, r.dir, args, env, out); err != nil {
		return fmt.Errorf("%w; the answers were valid, so fix the reported problem and run samuori-setup again", err)
	}
	return r.finish(ctx, a, out)
}

// runInstaller runs the release's install.sh with args and env, copying its output line by line to out.
func runInstaller(ctx context.Context, dir string, args, env []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, filepath.Join(dir, "install.sh"), args...)
	cmd.Env = env
	cmd.Dir = dir
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	copied := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			fmt.Fprintln(out, sc.Text())
		}
		_, _ = io.Copy(io.Discard, pr)
		close(copied)
	}()
	runErr := cmd.Run()
	pw.Close()
	<-copied
	if runErr != nil {
		return fmt.Errorf("install.sh failed: %w", runErr)
	}
	return nil
}

// finish waits for the server to create the administrator, then removes the password file it was created from.
func (r *releaseInstaller) finish(ctx context.Context, a setupcli.Answers, out io.Writer) error {
	status, client := "http://127.0.0.1:8080/api/v1/setup/status", http.DefaultClient
	if a.Deployment == setupcli.Cluster {
		pool := x509.NewCertPool()
		ca, err := os.ReadFile("/etc/samuori/cluster-ca.crt")
		if err != nil || !pool.AppendCertsFromPEM(ca) {
			return errors.New("installed, but the cluster CA /etc/samuori/cluster-ca.crt is unreadable")
		}
		status = "https://127.0.0.1:8443/api/v1/setup/status"
		client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: a.Host}}}
	}
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
		if setupCompleted(ctx, client, status) {
			if err := os.Remove("/etc/samuori/admin-password"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			fmt.Fprintf(out, "\nSetup is complete. Sign in at %s as %s.\n", a.PortalURL, a.Admin.Username)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	fmt.Fprintf(out, "\nSamuori is installed, but the server has not created the administrator yet. Once you can sign in at %s,\n"+
		"delete the password file it was created from: sudo rm /etc/samuori/admin-password\n", a.PortalURL)
	return nil
}

func setupCompleted(ctx context.Context, client *http.Client, url string) bool {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var s struct {
		Completed bool `json:"completed"`
	}
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&s) == nil && s.Completed
}

func withoutEnv(env []string, name string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// writePlan leaves the target topology next to server.yml; the server imports it once at its first start (ADR 0046).
func writePlan(dir string, p topology.Plan) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, topology.PlanFile), raw, 0o644)
}
