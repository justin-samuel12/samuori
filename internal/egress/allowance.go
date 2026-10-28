package egress

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/egress/proxy"
)

// Methods a project allowance can apply to.
var Methods = []string{"dockerfile", "recipe", "railpack", "buildpacks"}

// Allowance opens one preset, or one public or internal destination, for a single project's builds of the listed
// methods (ADR 0052). It is never part of the organization policy document.
type Allowance struct {
	ID      string
	Preset  string
	Host    string
	Port    int
	Methods []string
	// Internal lets the destination resolve to a private address.
	Internal bool
}

// Entry is "preset:<id>" or the destination's host:port.
func (a Allowance) Entry() string {
	if a.Preset != "" {
		return "preset:" + a.Preset
	}
	return net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
}

// ForProject returns p widened by the allowances that apply to method; an empty method takes every allowance, as
// predictions for "any build" do.
func (p Policy) ForProject(method string, all []Allowance) Policy {
	p.Project, p.Method = nil, method
	for _, a := range all {
		if method == "" || slices.Contains(a.Methods, method) {
			p.Project = append(p.Project, a)
		}
	}
	return p
}

func (p Policy) projectPreset(id string) bool {
	return slices.ContainsFunc(p.Project, func(a Allowance) bool { return a.Preset == id })
}

func (p Policy) projectPresets() []string {
	var out []string
	for _, a := range p.Project {
		if a.Preset != "" {
			out = append(out, a.Preset)
		}
	}
	return out
}

func (p Policy) projectDestination(host string, port int) bool {
	return slices.ContainsFunc(p.Project, func(a Allowance) bool { return a.Host == host && a.Port == port })
}

func (p Policy) projectIDs() []string {
	var out []string
	for _, a := range p.Project {
		out = append(out, a.ID)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// nonPublicSuffixes are names that never resolve on the public internet; they can only be internal destinations.
var nonPublicSuffixes = []string{".internal", ".local", ".localdomain", ".lan", ".home.arpa", ".intranet", ".corp"}

// RequestableInternal checks a destination a project member may ask to open as internal: any valid destination,
// so loopback, link-local (including metadata), multicast and unspecified addresses stay refused.
func RequestableInternal(s string) (buildplan.EgressRule, error) {
	r, err := ParseDestination(s)
	if err != nil {
		return r, err
	}
	r.Internal = true
	return r, nil
}

// RequestableDestination checks a destination a project member may ask for: a public DNS name or public IP literal.
// Loopback, link-local and metadata addresses are refused on principle, and private or reserved addresses and
// non-public names may only be requested as internal (docs/architecture/egress.md#project-allowances).
func RequestableDestination(s string) (buildplan.EgressRule, error) {
	r, err := ParseDestination(s)
	if err != nil {
		return r, err
	}
	if a, err := netip.ParseAddr(r.Host); err == nil {
		if !proxy.Permitted(a, false) {
			return r, fmt.Errorf("%s is not a public address; request it as an internal destination", s)
		}
		return r, nil
	}
	if !strings.Contains(r.Host, ".") || slices.ContainsFunc(nonPublicSuffixes, func(suf string) bool { return strings.HasSuffix(r.Host, suf) }) {
		return r, fmt.Errorf("%s is not a public name; request it as an internal destination", s)
	}
	return r, nil
}
