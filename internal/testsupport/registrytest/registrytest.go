// Package registrytest starts CNCF Distribution processes for tests (deploy/dev/lima/provision-egress.sh installs the
// registry binary): plain, htpasswd-protected, or a pull-through mirror that trusts a token certificate.
package registrytest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Registry is a running Distribution instance.
type Registry struct {
	Addr string
	cmd  *exec.Cmd
}

// Stop kills the process, for tests that need the registry gone.
func (r *Registry) Stop() {
	_ = r.cmd.Process.Kill()
	_ = r.cmd.Wait()
}

// Start runs Distribution with a config in which ADDR stands for the listen address; it skips the test when the
// binary is missing, unless SAMUORI_REQUIRE_DISTRIBUTION is set.
func Start(t *testing.T, config string) *Registry {
	t.Helper()
	return StartOn(t, "127.0.0.1", config)
}

// StartOn listens on host instead of loopback, for clients in another network namespace such as rootless BuildKit.
func StartOn(t *testing.T, host, config string) *Registry {
	t.Helper()
	bin, err := exec.LookPath("registry")
	if err != nil {
		if os.Getenv("SAMUORI_REQUIRE_DISTRIBUTION") != "" {
			t.Fatal("the registry binary is not installed")
		}
		t.Skip("CNCF Distribution is not installed on this host")
	}
	dir := t.TempDir()
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cfg := filepath.Join(dir, "config.yml")
	config = strings.ReplaceAll(strings.ReplaceAll(config, "ADDR", addr), "DATA", filepath.Join(dir, "data"))
	if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve", cfg)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &Registry{Addr: addr, cmd: cmd}
	t.Cleanup(r.Stop)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return r
		}
		if time.Now().After(deadline) {
			t.Fatal("the registry did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Upstream starts a registry that requires user and password.
func Upstream(t *testing.T, user, password string) *Registry {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	htpasswd := filepath.Join(t.TempDir(), "htpasswd")
	if err := os.WriteFile(htpasswd, []byte(user+":"+string(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Start(t, fmt.Sprintf(`version: 0.1
log: { level: error }
storage: { filesystem: { rootdirectory: DATA } }
http: { addr: "ADDR" }
auth: { htpasswd: { realm: upstream, path: %s } }
`, htpasswd))
}

// Mirror starts a pull-through cache of upstream that accepts only tokens for service and issuer signed by a key in
// trustPEM.
func Mirror(t *testing.T, upstream, user, password, service, issuer, trustPEM string) *Registry {
	t.Helper()
	return MirrorOn(t, "127.0.0.1", upstream, user, password, service, issuer, trustPEM)
}

// MirrorOn starts Mirror listening on host.
func MirrorOn(t *testing.T, host, upstream, user, password, service, issuer, trustPEM string) *Registry {
	t.Helper()
	trust := filepath.Join(t.TempDir(), "trust.pem")
	if err := os.WriteFile(trust, []byte(trustPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	return StartOn(t, host, fmt.Sprintf(`version: 0.1
log: { level: error }
storage: { filesystem: { rootdirectory: DATA } }
http: { addr: "ADDR" }
auth:
  token: { realm: "https://samuori.invalid/unused", service: %s, issuer: %s, rootcertbundle: %s }
proxy: { remoteurl: "http://%s", username: %s, password: %s }
`, service, issuer, trust, upstream, user, password))
}
