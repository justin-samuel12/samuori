// Package proxy is the build egress proxy (ADR 0025). Build steps reach it through HTTP(S)_PROXY with a per-attempt
// credential; it allows only the attempt's plan allowlist, decides on host and port without intercepting TLS,
// resolves names itself and dials the address it checked, so DNS answers cannot redirect a tunnel to a blocked
// address.
package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
)

const (
	user          = "samuori"
	maxDecisions  = 1000
	maxTunnels    = 64
	dialTimeout   = 10 * time.Second
	idleTimeout   = 5 * time.Minute
	maxHeaderSize = 16 << 10
)

type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type Options struct {
	Resolver Resolver
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error)
}

type Decision struct {
	Time    time.Time
	Method  string
	Host    string
	Port    int
	Allowed bool
	Reason  string
}

type Server struct {
	resolver Resolver
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)

	mu       sync.Mutex
	sessions map[string]*Session
}

func New(o Options) *Server {
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	if o.Dial == nil {
		o.Dial = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	return &Server{resolver: o.Resolver, dial: o.Dial, sessions: map[string]*Session{}}
}

// HTTPServer returns an http.Server for the proxy with bounded header reads.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: maxHeaderSize}
}

type Session struct {
	srv   *Server
	key   string
	token string
	rules []buildplan.EgressRule

	mu        sync.Mutex
	closed    bool
	decisions []Decision
	dropped   int
	conns     map[net.Conn]struct{}
}

// Open registers an attempt's allowlist and returns its session; Close revokes the credential and ends its tunnels.
func (s *Server) Open(rules []buildplan.EgressRule) (*Session, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(b)
	sess := &Session{srv: s, key: hash(token), token: token, rules: append([]buildplan.EgressRule(nil), rules...), conns: map[net.Conn]struct{}{}}
	s.mu.Lock()
	s.sessions[sess.key] = sess
	s.mu.Unlock()
	return sess, nil
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// URL is the proxy URL, with this session's credential, for build steps to use.
func (sess *Session) URL(addr string) string {
	return "http://" + user + ":" + sess.token + "@" + addr
}

func (sess *Session) Close() {
	sess.srv.mu.Lock()
	delete(sess.srv.sessions, sess.key)
	sess.srv.mu.Unlock()
	sess.mu.Lock()
	sess.closed = true
	conns := sess.conns
	sess.conns = map[net.Conn]struct{}{}
	sess.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
}

func (sess *Session) Decisions() []Decision {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return append([]Decision(nil), sess.decisions...)
}

// Summary lists each destination once with allowed and denied counts, for the run log.
func (sess *Session) Summary() string {
	type count struct{ allowed, denied int }
	sess.mu.Lock()
	byDest := map[string]*count{}
	reasons := map[string]string{}
	for _, d := range sess.decisions {
		k := net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
		if byDest[k] == nil {
			byDest[k] = &count{}
		}
		if d.Allowed {
			byDest[k].allowed++
		} else {
			byDest[k].denied++
			reasons[k] = d.Reason
		}
	}
	dropped := sess.dropped
	sess.mu.Unlock()
	keys := make([]string, 0, len(byDest))
	for k := range byDest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		c := byDest[k]
		if c.allowed > 0 {
			fmt.Fprintf(&b, "egress: allowed %s (%d)\n", k, c.allowed)
		}
		if c.denied > 0 {
			fmt.Fprintf(&b, "egress: denied %s (%d): %s\n", k, c.denied, reasons[k])
		}
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "egress: %d further decisions not recorded\n", dropped)
	}
	return b.String()
}

// Denial is one denied destination with its decision count and the latest reason.
type Denial struct {
	Host   string
	Port   int
	Count  int
	Reason string
}

// Denied lists denied destinations sorted by host and port, and how many decisions were not recorded, for the
// attempt report.
func (sess *Session) Denied() ([]Denial, int) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	byDest := map[string]*Denial{}
	var out []*Denial
	for _, d := range sess.decisions {
		if d.Allowed {
			continue
		}
		k := net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
		if byDest[k] == nil {
			byDest[k] = &Denial{Host: d.Host, Port: d.Port}
			out = append(out, byDest[k])
		}
		byDest[k].Count++
		byDest[k].Reason = d.Reason
	}
	res := make([]Denial, 0, len(out))
	for _, d := range out {
		res = append(res, *d)
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Host != res[j].Host {
			return res[i].Host < res[j].Host
		}
		return res[i].Port < res[j].Port
	})
	return res, sess.dropped
}

func (sess *Session) record(d Decision) {
	d.Time = time.Now().UTC()
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.decisions) >= maxDecisions {
		sess.dropped++
		return
	}
	sess.decisions = append(sess.decisions, d)
}

func (sess *Session) track(c net.Conn) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed || len(sess.conns) >= maxTunnels {
		return false
	}
	sess.conns[c] = struct{}{}
	return true
}

func (sess *Session) untrack(c net.Conn) {
	sess.mu.Lock()
	delete(sess.conns, c)
	sess.mu.Unlock()
}

func (s *Server) session(r *http.Request) *Session {
	h := r.Header.Get("Proxy-Authorization")
	scheme, cred, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred))
	if err != nil {
		return nil
	}
	u, token, ok := strings.Cut(string(raw), ":")
	if !ok || u != user {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[hash(token)]
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r)
	if sess == nil {
		w.Header().Set("Proxy-Authenticate", `Basic realm="samuori"`)
		http.Error(w, "proxy credential required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		sess.connect(w, r)
		return
	}
	sess.forward(w, r)
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

func (sess *Session) rule(host string, port int) (buildplan.EgressRule, bool) {
	for _, r := range sess.rules {
		if r.Host == host && r.Port == port {
			return r, true
		}
	}
	return buildplan.EgressRule{}, false
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

// Permitted reports whether a resolved address may be dialled. Loopback, link-local (including cloud metadata),
// multicast and unspecified addresses are never permitted; private and reserved ranges only for internal rules.
func Permitted(a netip.Addr, internal bool) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return false
	}
	if internal {
		return true
	}
	if a.IsPrivate() || !a.IsGlobalUnicast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Denial reasons. The controller classifies reported denials by these texts (internal/egress Advise).
const (
	ReasonInvalid        = "invalid destination"
	ReasonNotAllowed     = "not on the build's egress allowlist"
	ReasonUnresolved     = "name did not resolve"
	ReasonBlockedAddress = "resolves to a blocked address"
	ReasonConnectFailed  = "connection failed"
)

var errDenied = errors.New("denied")

// open checks the destination against the allowlist and dials an address that passed the address checks.
func (sess *Session) open(ctx context.Context, method, hostport, defaultPort string) (net.Conn, Decision, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		host, portStr = hostport, defaultPort
	}
	host = normalizeHost(strings.Trim(host, "[]"))
	port, err := strconv.Atoi(portStr)
	d := Decision{Method: method, Host: host, Port: port}
	if err != nil || port < 1 || port > 65535 {
		d.Reason = ReasonInvalid
		return nil, d, errDenied
	}
	rule, ok := sess.rule(host, port)
	if !ok {
		d.Reason = ReasonNotAllowed
		return nil, d, errDenied
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		rctx, cancel := context.WithTimeout(ctx, dialTimeout)
		addrs, err = sess.srv.resolver.LookupNetIP(rctx, "ip", host)
		cancel()
		if err != nil || len(addrs) == 0 {
			d.Reason = ReasonUnresolved
			return nil, d, errDenied
		}
	}
	var lastErr error
	for _, a := range addrs {
		if !Permitted(a, rule.Internal) {
			lastErr = fmt.Errorf("%s (%s)", ReasonBlockedAddress, a.Unmap())
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		c, err := sess.srv.dial(dctx, "tcp", netip.AddrPortFrom(a.Unmap(), uint16(port)).String())
		cancel()
		if err == nil {
			d.Allowed = true
			return c, d, nil
		}
		lastErr = errors.New(ReasonConnectFailed)
	}
	d.Reason = lastErr.Error()
	return nil, d, errDenied
}

func (sess *Session) connect(w http.ResponseWriter, r *http.Request) {
	up, d, err := sess.open(r.Context(), r.Method, r.Host, "443")
	sess.record(d)
	if err != nil {
		http.Error(w, "Samuori egress policy: "+d.Reason, http.StatusForbidden)
		return
	}
	if !sess.track(up) {
		_ = up.Close()
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	defer sess.untrack(up)
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "unsupported", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	defer down.Close()
	defer up.Close()
	if _, err := io.WriteString(down, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			_, _ = up.Write(b)
		}
		_, _ = io.Copy(idle{up}, idle{down})
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(idle{down}, idle{up})
		done <- struct{}{}
	}()
	<-done
}

// idle extends the connection deadline on every read and write, so stalled tunnels end.
type idle struct{ net.Conn }

func (c idle) Read(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(idleTimeout))
	return c.Conn.Read(p)
}

func (c idle) Write(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(idleTimeout))
	return c.Conn.Write(p)
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authorization", "Proxy-Authenticate", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// forward relays a plain-HTTP request, as package managers such as apt use for signed archives.
func (sess *Session) forward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() || r.URL.Scheme != "http" {
		http.Error(w, "only absolute http:// URLs or CONNECT are accepted", http.StatusBadRequest)
		return
	}
	up, d, err := sess.open(r.Context(), r.Method, r.URL.Host, "80")
	sess.record(d)
	if err != nil {
		http.Error(w, "Samuori egress policy: "+d.Reason, http.StatusForbidden)
		return
	}
	if !sess.track(up) {
		_ = up.Close()
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	defer sess.untrack(up)
	tr := &http.Transport{DisableKeepAlives: true, DialContext: func(context.Context, string, string) (net.Conn, error) { return up, nil }, ResponseHeaderTimeout: time.Minute}
	defer tr.CloseIdleConnections()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := tr.RoundTrip(out)
	if err != nil {
		_ = up.Close()
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, idle{readerConn{resp.Body, up}})
}

// readerConn applies the idle deadline of conn to reads from a response body.
type readerConn struct {
	io.Reader
	net.Conn
}

func (r readerConn) Read(p []byte) (int, error) { return r.Reader.Read(p) }
