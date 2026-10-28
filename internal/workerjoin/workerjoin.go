// Package workerjoin is the worker join code: the controller worker endpoints, the CA pin and a single-use
// enrollment token in one value for `samuori-join worker` (ADR 0045, SEC-WRK-11).
package workerjoin

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/pki"
)

const (
	Prefix      = "samuoriwj_"
	tokenPrefix = "samuoriw_"
	maxLen      = 4096
)

var ErrMalformed = errors.New("the worker join code is malformed; copy it again from the portal")

// Code holds no secret beyond the token, which is single use, expires within the hour and enrols the worker
// unassigned: no builds, secrets or self-test until an administrator assigns it to a pool.
type Code struct {
	Controllers []string `json:"controllers"`
	Pin         string   `json:"pin"`
	Token       string   `json:"token"`
}

func (c Code) valid() bool {
	if len(c.Controllers) == 0 || len(c.Controllers) > 16 || !validPin(c.Pin) || !validToken(c.Token) {
		return false
	}
	for _, e := range c.Controllers {
		if !validEndpoint(e) {
			return false
		}
	}
	return true
}

func validPin(p string) bool {
	h, ok := strings.CutPrefix(p, "sha256:")
	if !ok || len(h) != 64 {
		return false
	}
	for _, r := range h {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func validToken(t string) bool {
	v, ok := strings.CutPrefix(t, tokenPrefix)
	if !ok || len(v) < 32 || len(v) > 128 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil
}

func validEndpoint(e string) bool {
	host, port, err := net.SplitHostPort(e)
	if err != nil || host == "" || len(host) > 253 || strings.ContainsAny(host, " ,'\"`$\\;&|") {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 65536
}

func Encode(c Code) (string, error) {
	if !c.valid() {
		return "", errors.New("workerjoin: incomplete code")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func Decode(s string) (Code, error) {
	s = strings.TrimSpace(s)
	body, ok := strings.CutPrefix(s, Prefix)
	if !ok || len(s) > maxLen {
		return Code{}, ErrMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Code{}, ErrMalformed
	}
	var c Code
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || !c.valid() {
		return Code{}, ErrMalformed
	}
	return c, nil
}

// ServerName is the name the worker expects in every controller certificate: the first endpoint's host, as
// install.sh --controller has always used.
func (c Code) ServerName() string {
	if len(c.Controllers) == 0 {
		return ""
	}
	h, _, _ := net.SplitHostPort(c.Controllers[0])
	return h
}

// Endpoints lists the worker endpoints a remote worker can reach: this controller's certificate names that are not
// loopback, then the other controllers of a cluster, all on this controller's worker port. It is empty when the
// worker endpoint listens on loopback only, since no remote worker could connect.
func Endpoints(listen string, hostnames, controllers []string) []string {
	h, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" || loopback(h) {
		return nil
	}
	var out []string
	for _, name := range append(slices.Clone(hostnames), controllers...) {
		name = strings.TrimSpace(name)
		if name == "" || loopback(name) {
			continue
		}
		e := net.JoinHostPort(name, port)
		if validEndpoint(e) && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	if len(out) > 16 {
		out = out[:16]
	}
	return out
}

func loopback(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.IsLoopback()
}

// Check connects to each endpoint before anything is installed and refuses the code when a controller does not
// present a certificate for ServerName that chains to the pinned CA. Unreachable endpoints are skipped; at least one
// must answer.
func Check(ctx context.Context, c Code, timeout time.Duration) error {
	return check(ctx, c, c.ServerName(), timeout)
}

func check(ctx context.Context, c Code, serverName string, timeout time.Duration) error {
	var unreachable []string
	for _, e := range c.Controllers {
		d := tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: &tls.Config{
			MinVersion: tls.VersionTLS13, ServerName: serverName, NextProtos: []string{"h2"},
			InsecureSkipVerify:    true, // verified against the pinned CA below
			VerifyPeerCertificate: pki.VerifyPinned(c.Pin, serverName),
		}}
		dctx, cancel := context.WithTimeout(ctx, timeout)
		conn, err := d.DialContext(dctx, "tcp", e)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		var netErr net.Error
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" || errors.As(err, &netErr) && netErr.Timeout() {
			unreachable = append(unreachable, e)
			continue
		}
		return fmt.Errorf("cannot verify the controller at %s: %w; check that the code comes from your portal and that nothing intercepts the connection", e, err)
	}
	return fmt.Errorf("no controller answers at %s; check the network and firewall from this host to port %s", strings.Join(unreachable, ", "), port(c.Controllers[0]))
}

func port(e string) string {
	_, p, _ := net.SplitHostPort(e)
	return p
}
