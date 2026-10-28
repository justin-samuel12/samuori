package clusterjoin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"golang.org/x/crypto/bcrypt"
)

// The NATS system account lets a root helper on a controller remove a departed member from the JetStream meta group
// (docs/operators/high-availability.md#remove-a-controller). Its password is generated on each host, readable by
// root only, and the NATS member holds only its bcrypt hash. Controllers keep connecting with their client
// certificate alone, as the cluster's default user in the global account.
const (
	SystemUser        = "samuori-system"
	systemConfFile    = "system-account.conf"
	systemPassword    = "system-account.password"
	systemInclude     = `include "system-account.conf"`
	defaultClientUser = "samuori-controller"
)

func systemAccountConfig(hash string) string {
	return fmt.Sprintf(`accounts {
  SYS { users: [ { user: %q, password: %q } ] }
}
system_account: SYS
authorization { users: [ { user: %q } ] }
no_auth_user: %q
`, SystemUser, hash, defaultClientUser, defaultClientUser)
}

var systemHash = regexp.MustCompile(`password: "([^"]+)"`)

// EnsureSystemAccount gives the NATS member in natsDir a system account, keeping an existing one. It returns whether
// anything changed, in which case the member must restart: NATS cannot switch system accounts on reload. It runs as
// root from the installer and on every update.
func EnsureSystemAccount(natsDir, natsOwner string) (bool, error) {
	confPath := filepath.Join(natsDir, "nats.conf")
	conf, err := os.ReadFile(confPath)
	if err != nil {
		return false, err
	}
	nats, err := lookup(natsOwner)
	if err != nil {
		return false, err
	}
	root := owners{os.Geteuid(), os.Getegid()}
	changed := false
	pwPath := filepath.Join(natsDir, systemPassword)
	pw, err := os.ReadFile(pwPath)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(bytes.TrimSpace(pw)) < 32) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return false, err
		}
		pw = []byte(base64.RawURLEncoding.EncodeToString(raw))
		if err := writeOwned(pwPath, pw, 0o600, root); err != nil {
			return false, err
		}
		changed = true
	} else if err != nil {
		return false, err
	}
	pw = bytes.TrimSpace(pw)
	incPath := filepath.Join(natsDir, systemConfFile)
	inc, _ := os.ReadFile(incPath)
	if m := systemHash.FindSubmatch(inc); m == nil || bcrypt.CompareHashAndPassword(m[1], pw) != nil {
		hash, err := bcrypt.GenerateFromPassword(pw, 11)
		if err != nil {
			return false, err
		}
		if err := writeOwned(incPath, []byte(systemAccountConfig(string(hash))), 0o640, owners{root.uid, nats.gid}); err != nil {
			return false, err
		}
		changed = true
	}
	if !bytes.Contains(conf, []byte(systemInclude)) {
		conf = append(bytes.TrimRight(conf, "\n"), []byte("\n"+systemInclude+"\n")...)
		if err := writeOwned(confPath, conf, 0o644, nats); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

var (
	serverName = regexp.MustCompile(`(?m)^server_name: "([^"]+)"`)
	routesLine = regexp.MustCompile(`(?m)^(\s*routes: \[)([^\]]*)(\])`)
)

// OwnName is the server name in a member's nats.conf: the host it was installed for.
func OwnName(conf []byte) string {
	if m := serverName.FindSubmatch(conf); m != nil {
		return string(m[1])
	}
	return ""
}

// DropRoute removes host from the routes in a member's nats.conf.
func DropRoute(conf []byte, host string) ([]byte, bool) {
	m := routesLine.FindSubmatchIndex(conf)
	if m == nil {
		return conf, false
	}
	drop := "nats-route://" + net.JoinHostPort(host, "6222")
	var keep []string
	changed := false
	for _, r := range strings.Split(string(conf[m[4]:m[5]]), ",") {
		r = strings.TrimSpace(r)
		switch {
		case r == "":
		case r == drop:
			changed = true
		default:
			keep = append(keep, r)
		}
	}
	if !changed {
		return conf, false
	}
	out := append([]byte{}, conf[:m[4]]...)
	out = append(out, strings.Join(keep, ", ")...)
	return append(out, conf[m[5]:]...), true
}

// DropMessagingURL removes host's NATS member from the messaging URL list in a controller's server.yml.
func DropMessagingURL(serverYML []byte, host string) ([]byte, bool) {
	lines := strings.Split(string(serverYML), "\n")
	inMessaging, changed := false, false
	for i, l := range lines {
		if !strings.HasPrefix(l, " ") {
			inMessaging = strings.HasPrefix(l, "messaging:")
			continue
		}
		v, ok := strings.CutPrefix(l, "  url: ")
		if !inMessaging || !ok {
			continue
		}
		s, err := strconv.Unquote(v)
		if err != nil {
			continue
		}
		var keep []string
		for _, u := range strings.Split(s, ",") {
			if u != NATSURL(host) && u != "" {
				keep = append(keep, u)
			}
		}
		if len(keep) > 0 && len(keep) < len(strings.Split(s, ",")) {
			lines[i] = "  url: " + strconv.Quote(strings.Join(keep, ","))
			changed = true
		}
	}
	return []byte(strings.Join(lines, "\n")), changed
}

// DialSystem connects to the local NATS member in natsDir as the system account, with the member's own certificate.
// Only root can read the files it needs.
func DialSystem(natsDir string) (*nats.Conn, error) {
	conf, err := os.ReadFile(filepath.Join(natsDir, "nats.conf"))
	if err != nil {
		return nil, err
	}
	host := OwnName(conf)
	if host == "" {
		return nil, errors.New("clusterjoin: nats.conf names no server")
	}
	pw, err := os.ReadFile(filepath.Join(natsDir, systemPassword))
	if err != nil {
		return nil, fmt.Errorf("clusterjoin: the NATS system account is not set up: %w", err)
	}
	return dialSystem(NATSURL(host), bytes.TrimSpace(pw), filepath.Join(natsDir, "ca.crt"), filepath.Join(natsDir, "server.crt"), filepath.Join(natsDir, "server.key"))
}

func dialSystem(url string, pw []byte, caFile, certFile, keyFile string) (*nats.Conn, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("clusterjoin: the NATS CA file holds no certificates")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return nats.Connect(url, nats.Name("samuori-forget"), nats.UserInfo(SystemUser, string(pw)), nats.MaxReconnects(0),
		nats.Secure(&tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}))
}

// jsNotMember is NATS's error code for a peer that is not in the meta group.
const jsNotMember = 10044

// RemoveServer removes the member named name from the JetStream meta group. A member that is already gone counts as
// removed.
func RemoveServer(ctx context.Context, nc *nats.Conn, name string) error {
	req, _ := json.Marshal(map[string]string{"peer": name})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	msg, err := nc.RequestWithContext(ctx, "$JS.API.SERVER.REMOVE", req)
	if err != nil {
		return fmt.Errorf("clusterjoin: removing %s from the JetStream meta group: %w", name, err)
	}
	var resp struct {
		Success bool `json:"success"`
		Error   *struct {
			Code        int    `json:"err_code"`
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return err
	}
	switch {
	case resp.Success:
		return nil
	case resp.Error != nil && resp.Error.Code == jsNotMember:
		return nil
	case resp.Error != nil:
		return fmt.Errorf("clusterjoin: removing %s from the JetStream meta group: %s", name, resp.Error.Description)
	}
	return fmt.Errorf("clusterjoin: removing %s from the JetStream meta group: no answer", name)
}
