package clusterjoin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// Route sync keeps every NATS member's routes at exactly the cluster's members and, once every connected peer is
// listed, turns on verify_cert_and_check_known_urls so a peer whose name is not in the routes is refused even with a
// certificate the cluster CA signed (OD-27; docs/operators/high-availability.md#nats-routes).

const routePort = "6222"

// RouteURL is the route of the NATS member on host.
func RouteURL(host string) string { return "nats-route://" + net.JoinHostPort(host, routePort) }

// RouteURLs returns the routes for hosts, sorted.
func RouteURLs(hosts []string) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = RouteURL(h)
	}
	sort.Strings(out)
	return out
}

var clusterTLS = regexp.MustCompile(`(?m)^([ \t]+tls \{[^}\n]*?)(verify: true|verify_cert_and_check_known_urls: true)( \})$`)

const checkKnownURLs = "verify_cert_and_check_known_urls: true"

// RouteHosts returns the host names in a member's routes, lowercased.
func RouteHosts(conf []byte) []string {
	m := routesLine.FindSubmatch(conf)
	if m == nil {
		return nil
	}
	var out []string
	for _, r := range strings.Split(string(m[2]), ",") {
		u, err := url.Parse(strings.TrimSpace(r))
		if err != nil || u.Hostname() == "" {
			continue
		}
		out = appendHost(out, u.Hostname())
	}
	return out
}

// RoutesVerified reports whether the member's cluster TLS checks peers against its routes.
func RoutesVerified(conf []byte) bool {
	m := clusterTLS.FindSubmatch(conf)
	return m != nil && string(m[2]) == checkKnownURLs
}

// SetRoutes rewrites a member's routes and its route verification.
func SetRoutes(conf []byte, routes []string, verify bool) ([]byte, error) {
	if routesLine.Find(conf) == nil || clusterTLS.Find(conf) == nil {
		return nil, errors.New("clusterjoin: nats.conf has no cluster routes or cluster TLS to rewrite")
	}
	for _, r := range routes {
		if strings.ContainsAny(r, ",]\n\"") {
			return nil, fmt.Errorf("clusterjoin: %q is not a route", r)
		}
	}
	m := routesLine.FindSubmatchIndex(conf)
	out := append(append(append([]byte{}, conf[:m[4]]...), strings.Join(routes, ", ")...), conf[m[5]:]...)
	flag := "verify: true"
	if verify {
		flag = checkKnownURLs
	}
	return clusterTLS.ReplaceAll(out, []byte("${1}"+flag+"${3}")), nil
}

// RoutePlan is what a controller asks its host's member to route to, from the database.
type RoutePlan struct {
	// Members are the cluster's controllers and hosts whose join was accepted.
	Members []string `json:"members"`
	// Removed are controllers removed from the cluster; they are never routed to, even if a member still lists them.
	Removed []string `json:"removed"`
	// Verify means every running controller is known by name, so routes may be verified once the member's own
	// connected peers are all listed.
	Verify bool `json:"verify"`
	// Restart allows the helper to restart the member when a route to a removed host survives the reload; the
	// controller holds the cluster-wide restart lease while it asks.
	Restart bool `json:"restart,omitempty"`
}

// RouteOutcome is the helper's answer to a RoutePlan.
type RouteOutcome struct {
	// Outcome is succeeded, failed, stray (a route to a removed host survived the reload) or restarted.
	Outcome string   `json:"outcome"`
	Detail  string   `json:"detail,omitempty"`
	Hosts   []string `json:"hosts,omitempty"`
	Verify  bool     `json:"verify"`
	Waiting string   `json:"waiting,omitempty"`
	// Missing are listed members the local member has no route to.
	Missing []string `json:"missing,omitempty"`
	Stray   []string `json:"stray,omitempty"`
}

// RouteChange is the routes a member should have and why verification is on or off.
type RouteChange struct {
	Hosts  []string
	Verify bool
	// Waiting explains why verification stays off.
	Waiting string
}

// PlanRoutes brings the member's current routes to the plan. Removed hosts are always dropped. Another route the plan
// does not name is dropped only when every running controller has recorded its name (plan.Verify) and the member is
// not connected to that host, so a controller that never recorded its name, or a peer the member cannot read, keeps
// its route. connected holds the peers the member is routed to now, or nil when the member could not be asked;
// verification is then left as it is.
func PlanRoutes(own string, current []string, verified bool, plan RoutePlan, connected []string) (RouteChange, error) {
	own = strings.ToLower(own)
	if own == "" {
		return RouteChange{}, errors.New("clusterjoin: nats.conf names no server")
	}
	if len(plan.Members)+len(plan.Removed) > 64 {
		return RouteChange{}, errors.New("clusterjoin: the plan names more than 64 hosts")
	}
	removed := map[string]bool{}
	for _, h := range plan.Removed {
		if !ValidHost(h) {
			return RouteChange{}, fmt.Errorf("clusterjoin: the plan names %q, which is not a host name", h)
		}
		removed[strings.ToLower(h)] = true
	}
	if removed[own] {
		return RouteChange{}, errors.New("clusterjoin: this member was removed from the cluster; it keeps its routes until it is wiped")
	}
	hosts := []string{own}
	for _, h := range plan.Members {
		if !ValidHost(h) {
			return RouteChange{}, fmt.Errorf("clusterjoin: the plan names %q, which is not a host name", h)
		}
		if !removed[strings.ToLower(h)] {
			hosts = appendHost(hosts, h)
		}
	}
	for _, h := range current {
		l := strings.ToLower(h)
		stale := plan.Verify && connected != nil && !slices.ContainsFunc(connected, func(p string) bool { return strings.EqualFold(p, l) })
		if ValidHost(h) && !removed[l] && !stale {
			hosts = appendHost(hosts, h)
		}
	}
	sort.Strings(hosts)
	peers := slices.DeleteFunc(slices.Clone(hosts), func(h string) bool { return h == own })
	hadPeers := slices.ContainsFunc(current, func(h string) bool { return !strings.EqualFold(h, own) })
	if len(peers) == 0 && hadPeers {
		return RouteChange{}, errors.New("clusterjoin: the new routes would cut this member off from every peer")
	}
	ch := RouteChange{Hosts: hosts, Verify: verified}
	switch {
	case !plan.Verify:
		ch.Verify, ch.Waiting = false, "a running controller has not recorded its NATS name yet"
	case slices.ContainsFunc(hosts, func(h string) bool { return net.ParseIP(h) != nil }):
		ch.Verify, ch.Waiting = false, "a member is named by IP address; its certificate carries no DNS name to check"
	case connected == nil:
		if !verified {
			ch.Waiting = "the member's connected peers could not be read"
		}
	default:
		var unlisted []string
		for _, p := range connected {
			if !slices.Contains(hosts, strings.ToLower(p)) && !removed[strings.ToLower(p)] {
				unlisted = appendHost(unlisted, p)
			}
		}
		ch.Verify = len(unlisted) == 0
		if !ch.Verify {
			ch.Waiting = "connected peers are not in the plan yet: " + strings.Join(unlisted, ", ")
		}
	}
	return ch, nil
}

func appendHost(list []string, h string) []string {
	h = strings.ToLower(h)
	if slices.Contains(list, h) {
		return list
	}
	return append(list, h)
}

// MemberState is what the local member reports through the system account.
type MemberState struct {
	ConfigLoaded time.Time
	Routes       []string
	Peers        []string
}

// LocalState reads the local member's configuration load time, configured routes and connected route peers.
func LocalState(ctx context.Context, nc *nats.Conn) (MemberState, error) {
	id := nc.ConnectedServerId()
	if id == "" {
		return MemberState{}, errors.New("clusterjoin: not connected to the NATS member")
	}
	var varz struct {
		ConfigLoad time.Time `json:"config_load_time"`
		Cluster    struct {
			URLs []string `json:"urls"`
		} `json:"cluster"`
	}
	if err := sysRequest(ctx, nc, "$SYS.REQ.SERVER."+id+".VARZ", &varz); err != nil {
		return MemberState{}, err
	}
	var routez struct {
		Routes []struct {
			RemoteName string `json:"remote_name"`
		} `json:"routes"`
	}
	if err := sysRequest(ctx, nc, "$SYS.REQ.SERVER."+id+".ROUTEZ", &routez); err != nil {
		return MemberState{}, err
	}
	st := MemberState{ConfigLoaded: varz.ConfigLoad, Peers: []string{}}
	for _, u := range varz.Cluster.URLs {
		if h, _, err := net.SplitHostPort(u); err == nil {
			st.Routes = appendHost(st.Routes, h)
		}
	}
	for _, r := range routez.Routes {
		if r.RemoteName != "" {
			st.Peers = appendHost(st.Peers, r.RemoteName)
		}
	}
	sort.Strings(st.Routes)
	sort.Strings(st.Peers)
	return st, nil
}

func sysRequest(ctx context.Context, nc *nats.Conn, subject string, data any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	msg, err := nc.RequestWithContext(ctx, subject, nil)
	if err != nil {
		return fmt.Errorf("clusterjoin: %s: %w", subject, err)
	}
	var resp struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("clusterjoin: %s: %s", subject, resp.Error.Description)
	}
	return json.Unmarshal(resp.Data, data)
}
