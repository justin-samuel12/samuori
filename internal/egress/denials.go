package egress

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/egress/proxy"
)

// MaxDenials bounds the denied destinations kept for one attempt.
const MaxDenials = 50

const maxCount = 1 << 30

// Denial is one destination the worker's egress proxy denied, as the worker reported it. It never comes from the
// build log, which untrusted build steps write (docs/architecture/egress.md#denied-destinations).
type Denial struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}

// Advice is a denial with what an administrator can do about it under the current policy.
type Advice struct {
	Denial
	Hint      string   `json:"hint"`
	Presets   []string `json:"presets"`
	Allowable bool     `json:"allowable"`
	// Requestable: a project member may ask for this destination to be opened for the project only (ADR 0052).
	Requestable bool `json:"requestable"`
	// Network is how it can be opened: "public", or "internal" for a private address or non-public name.
	Network string `json:"network,omitempty"`
}

// RequestHint ends advice whose fix a project member may also request for the project alone.
const RequestHint = " A project developer can instead request access for this project only."

var blockedReason = regexp.MustCompile(`^` + regexp.QuoteMeta(proxy.ReasonBlockedAddress) + ` \(([0-9a-fA-F:.]{2,45})\)$`)

// CleanDenials keeps the well-formed entries of a worker's report: valid DNS names or IP addresses, ports 1-65535,
// non-negative counts, one entry per destination, at most MaxDenials. Unknown reasons are replaced.
func CleanDenials(in []Denial) []Denial {
	out := []Denial{}
	seen := map[string]bool{}
	for _, d := range in {
		if len(out) == MaxDenials {
			break
		}
		if d.Port < 1 || d.Port > 65535 || d.Count < 0 || !validHost(d.Host) {
			continue
		}
		k := net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
		if seen[k] {
			continue
		}
		seen[k] = true
		d.Count = min(d.Count, maxCount)
		d.Reason = cleanReason(d.Reason)
		out = append(out, d)
	}
	return out
}

func validHost(h string) bool {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Zone() == ""
	}
	return len(h) <= 253 && hostname.MatchString(h)
}

func cleanReason(r string) string {
	switch r {
	case proxy.ReasonInvalid, proxy.ReasonNotAllowed, proxy.ReasonUnresolved, proxy.ReasonConnectFailed:
		return r
	}
	if m := blockedReason.FindStringSubmatch(r); m != nil {
		if _, err := netip.ParseAddr(m[1]); err == nil {
			return r
		}
	}
	return "denied by the egress proxy"
}

// Advise explains one denial for a build of the given method ("dockerfile", "recipe", "railpack" or "buildpacks")
// from the preset catalog and the organization's current egress policy.
func (c *Catalog) Advise(d Denial, method string, p Policy) Advice {
	dest := net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
	a := Advice{Denial: d, Presets: c.containing(dest, method), Allowable: true}
	if why := refused(d); why != "" {
		a.Allowable, a.Presets = false, []string{}
		a.Hint = dest + " cannot be allowed: " + why
		return a
	}
	switch d.Reason {
	case proxy.ReasonUnresolved:
		a.Hint = dest + " is on the build's allowlist, but the name did not resolve from the worker. Check the name and the worker's DNS."
		return a
	case proxy.ReasonConnectFailed:
		a.Hint = dest + " is on the build's allowlist, but the connection failed. The destination may be down or unreachable from the worker."
		return a
	case proxy.ReasonInvalid:
		a.Hint = dest + " is not a valid destination."
		return a
	}
	for _, dst := range p.Destinations {
		if dst.Has(d.Host, d.Port) && dst.OpenFor(method) {
			a.Hint = dest + " is now an egress destination in the policy; run the build again."
			return a
		}
	}
	if p.projectDestination(d.Host, d.Port) || slices.ContainsFunc(a.Presets, p.projectPreset) {
		a.Hint = dest + " is now allowed for this project by an approved access request; run the build again."
		return a
	}
	a.Requestable, a.Network = true, NetworkPublic
	if _, notPublic := RequestableDestination(dest); notPublic != nil {
		a.Network = NetworkInternal
	}
	if m := blockedReason.FindStringSubmatch(d.Reason); m != nil {
		a.Network, a.Presets = NetworkInternal, []string{}
		a.Hint = fmt.Sprintf("%s resolved to the private address %s, which public destinations may not reach. If it is a host inside your network, an organization administrator can add it as an internal destination in the egress policy.", dest, m[1])
		return a.withRequestHint()
	}
	addDest := "an organization administrator can add it as a public destination in the egress policy."
	if a.Network == NetworkInternal {
		addDest = "an organization administrator can add it as an internal destination in the egress policy."
	}
	if len(a.Presets) == 0 {
		if pr, ok := c.ForOtherMethods(dest, method); ok {
			a.Hint = fmt.Sprintf("%s is in the %s preset, which is for %s; %s", dest, pr.ID, pr.MethodsLabel(), addDest)
		} else {
			a.Hint = dest + " is in no egress preset; " + addDest
		}
		return a.withRequestHint()
	}
	id := a.Presets[0]
	for _, pid := range a.Presets {
		if slices.Contains(p.AllowedPresets, pid) {
			id = pid
			break
		}
	}
	for _, pid := range a.Presets {
		if pr, ok := c.Get(pid); ok && slices.Contains(pr.Methods, method) {
			id = pid
			break
		}
	}
	for _, pid := range a.Presets {
		if slices.Contains(p.EveryBuildPresets, pid) {
			a.Hint = fmt.Sprintf("%s is in the %s preset, which now reaches all builds; run the build again.", dest, pid)
			a.Requestable = false
			return a
		}
	}
	allowed := slices.Contains(p.AllowedPresets, id)
	switch method {
	case "dockerfile":
		for _, pid := range a.Presets {
			if slices.Contains(p.DockerfilePresets, pid) {
				a.Hint = fmt.Sprintf("%s is in the %s preset, which now reaches Dockerfile builds; run the build again.", dest, pid)
				a.Requestable = false
				return a
			}
		}
		a.Hint = fmt.Sprintf("%s is in the %s preset; an organization administrator can set %s to reach Dockerfile builds (Builds that need it, and Dockerfile builds, or All builds) in the egress policy.", dest, id, id)
	case "railpack", "buildpacks":
		name := methodNames[method]
		first := ""
		if !allowed {
			first = fmt.Sprintf(" (with %s set to at least Builds that need it)", id)
		}
		a.Hint = fmt.Sprintf("%s is in the %s preset; an organization administrator can add %s to the %s provider's egress presets under Build & conversion providers%s, or set %s to All builds in the egress policy.", dest, id, id, name, first, id)
	default:
		a.Hint = fmt.Sprintf("%s is in the %s preset, which this build's recipe does not use; an organization administrator can set %s to All builds in the egress policy, or add the destination.", dest, id, id)
	}
	return a.withRequestHint()
}

func (a Advice) withRequestHint() Advice {
	if a.Requestable {
		a.Hint += RequestHint
	}
	return a
}

// refused says why the proxy never allows a destination: loopback, link-local (including metadata), multicast and
// unspecified addresses are refused on principle. Private addresses can be opened only as internal destinations, with
// an administrator's approval.
func refused(d Denial) string {
	addr := d.Host
	if m := blockedReason.FindStringSubmatch(d.Reason); m != nil {
		addr = m[1]
	}
	if a, err := netip.ParseAddr(addr); err == nil {
		if !proxy.Permitted(a, true) {
			return "the proxy never permits loopback, link-local, multicast or unspecified addresses."
		}
		return ""
	}
	if ValidateRule(buildplan.EgressRule{Host: d.Host, Port: d.Port}) != nil {
		return "the proxy never permits loopback names or addresses."
	}
	return ""
}

var methodNames = map[string]string{"dockerfile": "Dockerfile", "recipe": "recipe", "railpack": "Railpack", "buildpacks": "Paketo Buildpacks"}

// ForOtherMethods returns a preset listing dest that advice for method never offers, because the preset is limited to
// other build methods.
func (c *Catalog) ForOtherMethods(dest, method string) (Preset, bool) {
	for _, p := range c.All() {
		if len(p.Methods) > 0 && !slices.Contains(p.Methods, method) && slices.Contains(p.Destinations, dest) {
			return p, true
		}
	}
	return Preset{}, false
}

// MethodsLabel names the build methods the preset is limited to, as in "Railpack builds".
func (p Preset) MethodsLabel() string {
	names := make([]string, len(p.Methods))
	for i, m := range p.Methods {
		names[i] = methodNames[m]
	}
	switch len(names) {
	case 0:
		return "all builds"
	case 1:
		return names[0] + " builds"
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " builds"
}

func (c *Catalog) containing(dest, method string) []string {
	out := []string{}
	for _, p := range c.All() {
		if slices.Contains(p.Destinations, dest) && (len(p.Methods) == 0 || slices.Contains(p.Methods, method)) {
			out = append(out, p.ID)
		}
	}
	return out
}
