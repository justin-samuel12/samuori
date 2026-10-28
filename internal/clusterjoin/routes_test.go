package clusterjoin

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestPlanRoutes(t *testing.T) {
	t.Run("SEC-HA-10", func(t *testing.T) {
		all := RoutePlan{Members: []string{"a.example", "b.example", "c.example"}, Verify: true}
		ch, err := PlanRoutes("a.example", []string{"a.example", "b.example"}, false, all, []string{"b.example"})
		if err != nil || !slices.Equal(ch.Hosts, []string{"a.example", "b.example", "c.example"}) || !ch.Verify {
			t.Fatalf("filling seed-only routes: %+v, %v", ch, err)
		}

		ch, err = PlanRoutes("a.example", []string{"a.example", "b.example"}, false, all, []string{"b.example", "old.example"})
		if err != nil || ch.Verify || !strings.Contains(ch.Waiting, "old.example") || !slices.Contains(ch.Hosts, "b.example") {
			t.Fatalf("verification turned on while a connected peer is not listed: %+v, %v", ch, err)
		}

		ch, err = PlanRoutes("a.example", []string{"a.example", "legacy.example"}, true, all, nil)
		if err != nil || !slices.Contains(ch.Hosts, "legacy.example") || !ch.Verify {
			t.Fatalf("a route the plan does not name was dropped, or verification changed without peers: %+v, %v", ch, err)
		}

		ch, err = PlanRoutes("a.example", []string{"a.example", "b.example", "gone.example"}, true, all, []string{"b.example"})
		if err != nil || slices.Contains(ch.Hosts, "gone.example") || !ch.Verify {
			t.Fatalf("an unconnected route the plan does not name was kept: %+v, %v", ch, err)
		}
		ch, err = PlanRoutes("a.example", []string{"a.example", "b.example", "gone.example"}, true, RoutePlan{Members: all.Members}, []string{"b.example"})
		if err != nil || !slices.Contains(ch.Hosts, "gone.example") {
			t.Fatalf("a route was dropped while a running controller has no recorded name: %+v, %v", ch, err)
		}
		ch, err = PlanRoutes("a.example", []string{"a.example", "b.example", "live.example"}, true, all, []string{"b.example", "live.example"})
		if err != nil || !slices.Contains(ch.Hosts, "live.example") {
			t.Fatalf("a connected peer the plan does not name lost its route: %+v, %v", ch, err)
		}

		ch, err = PlanRoutes("a.example", []string{"a.example", "b.example"}, true, RoutePlan{Members: all.Members}, []string{"b.example"})
		if err != nil || ch.Verify {
			t.Fatalf("verification stayed on although a running controller has no recorded name: %+v, %v", ch, err)
		}

		rm := RoutePlan{Members: []string{"a.example", "b.example"}, Removed: []string{"C.example"}, Verify: true}
		ch, err = PlanRoutes("a.example", []string{"a.example", "b.example", "c.example"}, true, rm, []string{"b.example", "c.example"})
		if err != nil || slices.Contains(ch.Hosts, "c.example") || !ch.Verify {
			t.Fatalf("removing a member: %+v, %v", ch, err)
		}

		if _, err := PlanRoutes("a.example", []string{"a.example", "b.example"}, true, RoutePlan{Removed: []string{"b.example"}, Verify: true}, nil); err == nil {
			t.Fatal("a plan that leaves this member only its own route was applied")
		}
		if _, err := PlanRoutes("a.example", []string{"a.example", "b.example"}, true, RoutePlan{Removed: []string{"a.example"}}, nil); err == nil {
			t.Fatal("a removed member rewrote its own routes")
		}
		if _, err := PlanRoutes("a.example", nil, false, RoutePlan{Members: []string{"b.example\"]"}}, nil); err == nil {
			t.Fatal("a plan with a malformed name was applied")
		}
		ch, err = PlanRoutes("a.example", []string{"a.example"}, false, RoutePlan{Members: []string{"a.example"}, Verify: true}, []string{})
		if err != nil || !slices.Equal(ch.Hosts, []string{"a.example"}) {
			t.Fatalf("the first member alone: %+v, %v", ch, err)
		}
		ch, err = PlanRoutes("a.example", []string{"a.example"}, false, RoutePlan{Members: []string{"a.example", "10.0.0.2"}, Verify: true}, []string{})
		if err != nil || ch.Verify {
			t.Fatalf("verification turned on for a member named by IP address: %+v, %v", ch, err)
		}
	})
}

func TestSetRoutesRoundTrip(t *testing.T) {
	t.Run("SEC-HA-10", func(t *testing.T) {
		dir := t.TempDir()
		conf := []byte(Bundle{Host: "a.example", NATSURLs: []string{NATSURL("a.example")}}.natsConfig(dir))
		if RoutesVerified(conf) {
			t.Fatal("a new member's routes are verified before the route sync ran")
		}
		next, err := SetRoutes(conf, RouteURLs([]string{"b.example", "a.example"}), true)
		if err != nil {
			t.Fatal(err)
		}
		if !RoutesVerified(next) || !slices.Equal(RouteHosts(next), []string{"a.example", "b.example"}) {
			t.Fatalf("rewritten configuration:\n%s", next)
		}
		if bytes.Contains(bytes.SplitN(next, []byte("\n"), 5)[3], []byte(checkKnownURLs)) || bytes.Count(next, []byte(checkKnownURLs)) != 1 {
			t.Fatalf("verification is not limited to the cluster TLS block:\n%s", next)
		}
		again, _ := SetRoutes(next, RouteURLs([]string{"a.example", "b.example"}), true)
		if !bytes.Equal(again, next) {
			t.Error("rewriting the same routes changed the file")
		}
		if dropped, ok := DropRoute(next, "b.example"); !ok || !RoutesVerified(dropped) {
			t.Error("forget no longer understands the rewritten routes")
		}
		ca, err := Create(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		sc, err := ca.Issue(NATSServer, "a.example")
		if err != nil {
			t.Fatal(err)
		}
		for f, data := range map[string][]byte{"ca.crt": ca.CertPEM(), "server.crt": sc.Cert, "server.key": sc.Key, systemConfFile: nil} {
			_ = os.WriteFile(filepath.Join(dir, f), data, 0o600)
		}
		if err := os.WriteFile(filepath.Join(dir, "nats.conf"), next, 0o644); err != nil {
			t.Fatal(err)
		}
		opts, err := server.ProcessConfigFile(filepath.Join(dir, "nats.conf"))
		if err != nil {
			t.Fatalf("the rewritten nats.conf does not parse: %v", err)
		}
		if !opts.Cluster.TLSCheckKnownURLs || len(opts.Routes) != 2 {
			t.Fatalf("parsed: check known URLs %v, routes %v", opts.Cluster.TLSCheckKnownURLs, opts.Routes)
		}
		if _, err := SetRoutes(next, []string{"nats-route://x:6222, nats-route://y:6222"}, true); err == nil {
			t.Error("a route holding a list separator was written")
		}
	})
}

type routeMember struct {
	name, dir   string
	port, cport int
	srv         *server.Server
	routeTo     func([]string) []string
	mesh        map[string]*routeMember
	t           *testing.T
}

func (m *routeMember) conf() []byte {
	b, err := os.ReadFile(filepath.Join(m.dir, "nats.conf"))
	if err != nil {
		m.t.Fatal(err)
	}
	return b
}

// sync applies a plan the way samuori-server cluster routes does, then reloads as SIGHUP does.
func (m *routeMember) sync(plan RoutePlan) RouteChange {
	m.t.Helper()
	pw, _ := os.ReadFile(filepath.Join(m.dir, systemPassword))
	var nc *nats.Conn
	var err error
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(routePoll) {
		nc, err = dialSystem(fmt.Sprintf("tls://%s:%d", m.name, m.port), bytes.TrimSpace(pw), filepath.Join(m.dir, "ca.crt"), filepath.Join(m.dir, "server.crt"), filepath.Join(m.dir, "server.key"))
		if err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		m.t.Fatalf("%s system account: %v", m.name, err)
	}
	defer nc.Close()
	before, err := LocalState(context.Background(), nc)
	if err != nil {
		m.t.Fatalf("%s state: %v", m.name, err)
	}
	conf := m.conf()
	ch, err := PlanRoutes(OwnName(conf), RouteHosts(conf), RoutesVerified(conf), plan, before.Peers)
	if err != nil {
		m.t.Fatalf("%s plan: %v", m.name, err)
	}
	m.write(ch.Hosts, ch.Verify)
	if err := m.srv.Reload(); err != nil {
		m.t.Fatalf("%s reload: %v", m.name, err)
	}
	after, err := LocalState(context.Background(), nc)
	if err != nil || !after.ConfigLoaded.After(before.ConfigLoaded) || !slices.Equal(after.Routes, ch.Hosts) {
		m.t.Fatalf("%s after the reload: %+v, %v; want routes %v", m.name, after, err, ch.Hosts)
	}
	return ch
}

func (m *routeMember) write(hosts []string, verify bool) {
	m.t.Helper()
	next, err := SetRoutes(m.conf(), m.routeTo(hosts), verify)
	if err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, "nats.conf"), next, 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *routeMember) peers() []string {
	rz, err := m.srv.Routez(nil)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range rz.Routes {
		if r.RemoteName != "" && !slices.Contains(out, r.RemoteName) {
			out = append(out, r.RemoteName)
		}
	}
	slices.Sort(out)
	return out
}

// routes are the peers that list m too. A member that dials a peer registers the route as soon as the peer's INFO
// arrives, before the peer checks its certificate, so a refused retry shows briefly on the dialling side only.
func (m *routeMember) routes() []string {
	var out []string
	for _, p := range m.peers() {
		if o := m.mesh[p]; o != nil && o.srv != nil && slices.Contains(o.peers(), m.name) {
			out = append(out, p)
		}
	}
	return out
}

// accepted reports whether m holds a route the member named from opened, the only kind route verification checks.
func (m *routeMember) accepted(from string) bool {
	rz, err := m.srv.Routez(nil)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(rz.Routes, func(r *server.RouteInfo) bool { return r.RemoteName == from && !r.DidSolicit })
}

const routePoll = 100 * time.Millisecond

func waitRoutes(t *testing.T, m *routeMember, want []string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := m.routes()
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is routed to %v; want %v", m.name, got, want)
		}
		time.Sleep(routePoll)
	}
}

func holdRoutes(t *testing.T, m *routeMember, want []string, period time.Duration) {
	t.Helper()
	deadline := time.Now().Add(period)
	for time.Now().Before(deadline) {
		if got := m.routes(); !slices.Equal(got, want) {
			t.Fatalf("%s is routed to %v; want %v", m.name, got, want)
		}
		time.Sleep(routePoll)
	}
}

// restart stops a member and starts it again from its configuration file, as the helper does for a stray route.
func (m *routeMember) restart() {
	m.t.Helper()
	m.srv.Shutdown()
	m.srv.WaitForShutdown()
	opts, err := server.ProcessConfigFile(filepath.Join(m.dir, "nats.conf"))
	if err != nil {
		m.t.Fatal(err)
	}
	opts.NoSigs, opts.NoLog = true, true
	if m.srv, err = server.NewServer(opts); err != nil {
		m.t.Fatal(err)
	}
	go m.srv.Start()
	m.t.Cleanup(m.srv.Shutdown)
	if !m.srv.ReadyForConnections(10 * time.Second) {
		m.t.Fatalf("%s did not restart", m.name)
	}
}

// recheckStrays repeats what the controller's follow-up route checks do: a survivor still routed to the removed
// member after strayWait is restarted. It returns once the removed member has had no route for quiet, and fails if
// that does not happen within a minute.
func recheckStrays(t *testing.T, removed *routeMember, quiet time.Duration, survivors ...*routeMember) {
	t.Helper()
	const strayWait = 2 * time.Second
	deadline := time.Now().Add(time.Minute)
	since := map[*routeMember]time.Time{}
	var isolated time.Time
	for {
		now := time.Now()
		for _, m := range survivors {
			if !slices.Contains(m.peers(), removed.name) {
				delete(since, m)
				continue
			}
			if since[m].IsZero() {
				since[m] = now
			} else if now.Sub(since[m]) >= strayWait {
				t.Logf("%s kept a stray route to %s; restarting it", m.name, removed.name)
				m.restart()
				delete(since, m)
			}
		}
		switch got := removed.routes(); {
		case len(got) > 0:
			isolated = time.Time{}
		case isolated.IsZero():
			isolated = now
		case now.Sub(isolated) >= quiet:
			return
		}
		if now.After(deadline) {
			t.Fatalf("%s is still routed to %v", removed.name, removed.routes())
		}
		time.Sleep(routePoll)
	}
}

// TestNATSRouteVerification is the OD-27 part of docs/spikes/nats-member-removal.md: members run the generated
// configuration with verify_cert_and_check_known_urls, a removed member that still runs with a valid certificate
// cannot route back, and a new member routes in only once the others list it.
func TestNATSRouteVerification(t *testing.T) {
	t.Run("SEC-HA-10", func(t *testing.T) {
		for _, h := range []string{"n1.localhost", "n4.localhost"} {
			if addrs, err := net.LookupHost(h); err != nil || !slices.Contains(addrs, "127.0.0.1") {
				t.Skipf("%s does not resolve to 127.0.0.1 here (%v, %v); the spike needs per-member DNS names", h, addrs, err)
			}
		}
		me, _ := user.Current()
		ca, err := Create(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		members := map[string]*routeMember{}
		routeTo := func(hosts []string) []string {
			var out []string
			for _, h := range hosts {
				out = append(out, fmt.Sprintf("nats-route://%s:%d", h, members[h].cport))
			}
			return out
		}
		names := []string{"n1.localhost", "n2.localhost", "n3.localhost", "n4.localhost"}
		for _, n := range names {
			m := &routeMember{name: n, dir: t.TempDir(), port: freePort(t), cport: freePort(t), routeTo: routeTo, mesh: members, t: t}
			members[n] = m
			sc, err := ca.Issue(NATSServer, n)
			if err != nil {
				t.Fatal(err)
			}
			for f, data := range map[string][]byte{"ca.crt": ca.CertPEM(), "server.crt": sc.Cert, "server.key": sc.Key} {
				if err := os.WriteFile(filepath.Join(m.dir, f), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		start := func(m *routeMember, hosts []string, verify bool) {
			conf := Bundle{Host: m.name, NATSURLs: []string{NATSURL(m.name)}}.natsConfig(m.dir)
			conf = strings.NewReplacer(
				"listen: 0.0.0.0:4222", fmt.Sprintf("listen: 127.0.0.1:%d", m.port),
				m.name+":4222", fmt.Sprintf("%s:%d", m.name, m.port),
				"listen: 0.0.0.0:6222", fmt.Sprintf("listen: 127.0.0.1:%d", m.cport),
				m.name+":6222", fmt.Sprintf("%s:%d", m.name, m.cport),
				"/var/lib/nats", filepath.Join(m.dir, "js"),
			).Replace(conf)
			if err := os.WriteFile(filepath.Join(m.dir, "nats.conf"), []byte(conf), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := EnsureSystemAccount(m.dir, me.Username); err != nil {
				t.Fatal(err)
			}
			m.write(hosts, verify)
			opts, err := server.ProcessConfigFile(filepath.Join(m.dir, "nats.conf"))
			if err != nil {
				t.Fatalf("%s: %v", m.name, err)
			}
			if opts.Cluster.TLSCheckKnownURLs != verify {
				t.Fatalf("%s checks known URLs: %v", m.name, !verify)
			}
			opts.NoSigs, opts.NoLog = true, true
			s, err := server.NewServer(opts)
			if err != nil {
				t.Fatal(err)
			}
			go s.Start()
			t.Cleanup(s.Shutdown)
			if !s.ReadyForConnections(10 * time.Second) {
				t.Fatalf("%s did not start", m.name)
			}
			m.srv = s
		}
		n1, n2, n3, n4 := members[names[0]], members[names[1]], members[names[2]], members[names[3]]
		three := names[:3]
		for _, m := range []*routeMember{n1, n2, n3} {
			start(m, three, true)
		}
		waitRoutes(t, n1, []string{n2.name, n3.name}, 20*time.Second)
		waitRoutes(t, n2, []string{n1.name, n3.name}, 20*time.Second)
		waitRoutes(t, n3, []string{n1.name, n2.name}, 20*time.Second)

		removal := RoutePlan{Members: []string{n1.name, n2.name}, Removed: []string{n3.name}, Verify: true}
		for _, m := range []*routeMember{n1, n2} {
			if ch := m.sync(removal); !ch.Verify || slices.Contains(ch.Hosts, n3.name) {
				t.Fatalf("%s after the removal: %+v", m.name, ch)
			}
		}
		// A route a survivor solicited to n3 through gossip survives its reload; the helper reports it as stray and
		// the controller restarts that member, so the test does the same.
		recheckStrays(t, n3, 5*time.Second, n1, n2)
		waitRoutes(t, n1, []string{n2.name}, 15*time.Second)
		waitRoutes(t, n2, []string{n1.name}, 15*time.Second)
		if !RoutesVerified(n3.conf()) || !slices.Contains(RouteHosts(n3.conf()), n1.name) {
			t.Fatal("the removed member no longer tries to route back, so the check above proves nothing")
		}

		n2.write([]string{n1.name, n2.name}, false)
		if err := n2.srv.Reload(); err != nil {
			t.Fatal(err)
		}
		// With verification off on n2 alone, n3 routes back to n2, and n2's gossip can make n1 open an implicit route
		// to n3: n1 solicited it, so n1's own check never sees it, and only a restart drops it.
		deadline := time.Now().Add(10 * time.Second)
		for !slices.Contains(n3.routes(), n2.name) || (!slices.Contains(n3.routes(), n1.name) && time.Now().Add(5*time.Second).Before(deadline)) {
			if time.Now().After(deadline) {
				t.Fatalf("n3 did not route back to n2 with its verification off: %v", n3.routes())
			}
			time.Sleep(routePoll)
		}
		implicit := slices.Contains(n3.routes(), n1.name)
		n2.write([]string{n1.name, n2.name}, true)
		if err := n2.srv.Reload(); err != nil {
			t.Fatal(err)
		}
		// n2's check drops the route n3 opened. The implicit route n1 opened survives, and n1's gossip can make n2 dial
		// n3 itself; routes a member dialled are never checked, so only a restart drops either.
		for deadline := time.Now().Add(10 * time.Second); n2.accepted(n3.name); time.Sleep(routePoll) {
			if time.Now().After(deadline) {
				t.Fatal("n2 kept the route n3 opened after turning verification on again")
			}
		}
		if implicit {
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(routePoll) {
				if !slices.Contains(n3.routes(), n1.name) {
					t.Fatalf("the implicit route from n1 closed without a restart: %v", n3.routes())
				}
			}
		} else {
			waitRoutes(t, n3, nil, 10*time.Second)
		}
		t.Logf("gossip opened an implicit route from n1 to the removed member: %v", implicit)
		n1.restart()
		recheckStrays(t, n3, 3*time.Second, n1, n2)
		waitRoutes(t, n1, []string{n2.name}, 15*time.Second)

		// A joining member starts from its bundle: the routes its issuer knew and no verification yet, since it cannot
		// know every peer until its own sync. Until the members list it, they refuse it.
		start(n4, []string{n1.name, n4.name}, false)
		holdRoutes(t, n4, nil, 4*time.Second)
		join := RoutePlan{Members: []string{n1.name, n2.name, n4.name}, Removed: []string{n3.name}, Verify: true}
		for _, m := range []*routeMember{n1, n2} {
			m.sync(join)
		}
		waitRoutes(t, n4, []string{n1.name, n2.name}, 15*time.Second)
		if ch := n4.sync(join); !ch.Verify || !slices.Equal(ch.Hosts, []string{n1.name, n2.name, n4.name}) {
			t.Fatalf("the new member's own sync: %+v", ch)
		}
		// The members' follow-up checks after the join keep n3 cut off.
		recheckStrays(t, n3, 5*time.Second, n1, n2, n4)
		waitRoutes(t, n1, []string{n2.name, n4.name}, 15*time.Second)
	})
}
