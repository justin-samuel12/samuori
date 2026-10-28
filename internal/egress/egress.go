// Package egress holds the central build egress model: versioned ecosystem presets, the organization egress policy
// and the per-build allowlist placed in the build plan (ADR 0025, docs/architecture/egress.md).
package egress

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"samuori.invalid/samuori/internal/buildplan"
)

//go:embed presets/*.json
var presetFS embed.FS

type Preset struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	DisplayName  string   `json:"displayName"`
	Destinations []string `json:"destinations"`
	Note         string   `json:"note,omitempty"`
	// Methods limits the build methods advice may suggest this preset for; empty means any.
	Methods []string `json:"methods,omitempty"`
}

type Catalog struct{ byID map[string]Preset }

var (
	builtinOnce sync.Once
	builtin     *Catalog
	builtinErr  error
)

// Builtin returns the presets shipped with this release.
func Builtin() (*Catalog, error) {
	builtinOnce.Do(func() { builtin, builtinErr = load(presetFS) })
	return builtin, builtinErr
}

func load(fsys fs.FS) (*Catalog, error) {
	c := &Catalog{byID: map[string]Preset{}}
	err := fs.WalkDir(fsys, "presets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		var pr Preset
		if err := json.Unmarshal(raw, &pr); err != nil {
			return fmt.Errorf("egress: %s: %w", p, err)
		}
		if pr.ID == "" || pr.Version == "" || len(pr.Destinations) == 0 {
			return fmt.Errorf("egress: %s: id, version and destinations are required", p)
		}
		for _, d := range pr.Destinations {
			if _, err := ParseDestination(d); err != nil {
				return fmt.Errorf("egress: %s: %w", p, err)
			}
		}
		if _, dup := c.byID[pr.ID]; dup {
			return fmt.Errorf("egress: preset %s is defined twice", pr.ID)
		}
		c.byID[pr.ID] = pr
		return nil
	})
	return c, err
}

func (c *Catalog) Get(id string) (Preset, bool) {
	p, ok := c.byID[id]
	return p, ok
}

func (c *Catalog) All() []Preset {
	out := make([]Preset, 0, len(c.byID))
	for _, p := range c.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (c *Catalog) IDs() []string {
	var out []string
	for _, p := range c.All() {
		out = append(out, p.ID)
	}
	return out
}

var hostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// ParseDestination parses "host:port". Hosts are lower-case DNS names or IP literals; loopback, link-local,
// multicast and unspecified addresses are never valid destinations.
func ParseDestination(s string) (buildplan.EgressRule, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return buildplan.EgressRule{}, fmt.Errorf("destination %q must be host:port", s)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return buildplan.EgressRule{}, fmt.Errorf("destination %q has an invalid port", s)
	}
	r := buildplan.EgressRule{Host: host, Port: n}
	return r, ValidateRule(r)
}

func ValidateRule(r buildplan.EgressRule) error {
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("destination %s has an invalid port", r.Host)
	}
	if ip := net.ParseIP(r.Host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("destination %s is a loopback, link-local, multicast or unspecified address", r.Host)
		}
		return nil
	}
	if r.Host == "localhost" || strings.HasSuffix(r.Host, ".localhost") || !hostname.MatchString(r.Host) {
		return fmt.Errorf("destination host %q must be a lower-case DNS name or an IP address", r.Host)
	}
	return nil
}

// Destination is an administrator-defined public or internal destination, such as GitHub release downloads or a
// private artifact repository.
type Destination struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Reason string `json:"reason"`
	// Network is NetworkPublic or NetworkInternal (the default). A public destination may resolve only to public
	// addresses at the proxy; an internal one may resolve to private ones.
	Network string `json:"network,omitempty"`
	// Also lists more host:port addresses opened under the same name, network and switches.
	Also []string `json:"also,omitempty"`
	// Disabled keeps the entry without opening it; NoDockerfile opens it for every method but Dockerfile builds.
	Disabled     bool `json:"disabled,omitempty"`
	NoDockerfile bool `json:"noDockerfile,omitempty"`
}

// Rules are the destination's addresses: host and port, then Also.
func (d Destination) Rules() []buildplan.EgressRule {
	out := []buildplan.EgressRule{{Host: d.Host, Port: d.Port, Internal: d.Internal()}}
	for _, a := range d.Also {
		if r, err := ParseDestination(a); err == nil {
			r.Internal = d.Internal()
			out = append(out, r)
		}
	}
	return out
}

// Has reports whether host:port is one of the destination's addresses.
func (d Destination) Has(host string, port int) bool {
	return slices.ContainsFunc(d.Rules(), func(r buildplan.EgressRule) bool { return r.Host == host && r.Port == port })
}

const maxAddresses = 20

// OpenFor reports whether a build of method reaches d; an empty method means any build.
func (d Destination) OpenFor(method string) bool {
	return !d.Disabled && !(d.NoDockerfile && method == "dockerfile")
}

const (
	NetworkPublic   = "public"
	NetworkInternal = "internal"
)

func (d Destination) Internal() bool {
	return d.Network != NetworkPublic
}

// Policy is the organization's egress policy document (policy kind "egress").
type Policy struct {
	AllowedPresets    []string `json:"allowedPresets"`
	DockerfilePresets []string `json:"dockerfilePresets"`
	// EveryBuildPresets reach every build, whatever its recipe or method asks for; a subset of the allowed ones.
	EveryBuildPresets []string      `json:"everyBuildPresets,omitempty"`
	Destinations      []Destination `json:"destinations"`
	// Project holds one project's active allowances for one build method (ForProject); never stored in the policy.
	Project []Allowance `json:"-"`
	// Method is the build method this policy is applied to (ForMethod, ForProject); empty means any build.
	Method string `json:"-"`
}

// ForMethod returns p for builds of method.
func (p Policy) ForMethod(method string) Policy {
	p.Method = method
	return p
}

var destName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (c *Catalog) ValidatePolicy(p Policy) error {
	for _, id := range append(slices.Clone(p.AllowedPresets), p.DockerfilePresets...) {
		if _, ok := c.Get(id); !ok {
			return fmt.Errorf("unknown preset %q", id)
		}
	}
	for _, id := range p.DockerfilePresets {
		if !slices.Contains(p.AllowedPresets, id) {
			return fmt.Errorf("Dockerfile preset %q is not in allowedPresets", id)
		}
	}
	for _, id := range p.EveryBuildPresets {
		if !slices.Contains(p.AllowedPresets, id) {
			return fmt.Errorf("every-build preset %q is not in allowedPresets", id)
		}
	}
	seen := map[string]bool{}
	for _, d := range p.Destinations {
		if !destName.MatchString(d.Name) || seen[d.Name] {
			return fmt.Errorf("destination names must be unique lower-case labels; %q is not", d.Name)
		}
		seen[d.Name] = true
		if len(d.Reason) > 1024 {
			return fmt.Errorf("destination %s has a reason longer than 1024 characters", d.Name)
		}
		if len(d.Also) >= maxAddresses {
			return fmt.Errorf("destination %s has more than %d addresses", d.Name, maxAddresses)
		}
		addrs := []string{net.JoinHostPort(d.Host, strconv.Itoa(d.Port))}
		for _, a := range d.Also {
			if _, err := ParseDestination(a); err != nil {
				return fmt.Errorf("destination %s: %w", d.Name, err)
			}
			addrs = append(addrs, a)
		}
		if err := ValidateRule(buildplan.EgressRule{Host: d.Host, Port: d.Port}); err != nil {
			return err
		}
		switch d.Network {
		case "", NetworkInternal:
		case NetworkPublic:
			for _, a := range addrs {
				if _, err := RequestableDestination(a); err != nil {
					return fmt.Errorf("destination %s: %w", d.Name, err)
				}
			}
		default:
			return fmt.Errorf("destination %s: network must be public or internal", d.Name)
		}
	}
	return nil
}

// DefaultPolicy allows every built-in preset, gives Dockerfile builds the OS package archives and defines no internal
// destinations.
func (c *Catalog) DefaultPolicy() Policy {
	return Policy{AllowedPresets: c.IDs(), DockerfilePresets: []string{"alpine", "debian", "ubuntu"}, Destinations: []Destination{}}
}

// Resolve builds the plan allowlist from the presets a build needs plus the project's allowances. Presets that
// neither the policy nor an allowance permits are returned so admission can refuse the build instead of letting it
// fail at run time.
func (c *Catalog) Resolve(presets []string, p Policy) (buildplan.PlanEgress, []string) {
	out := buildplan.PlanEgress{Presets: []string{}, Allow: []buildplan.EgressRule{}}
	var refused []string
	seen := map[buildplan.EgressRule]bool{}
	add := func(r buildplan.EgressRule) {
		if !seen[r] {
			seen[r] = true
			out.Allow = append(out.Allow, r)
		}
	}
	ids := slices.Concat(presets, p.projectPresets(), p.EveryBuildPresets)
	sort.Strings(ids)
	for _, id := range slices.Compact(ids) {
		pr, ok := c.Get(id)
		if !ok || !(slices.Contains(p.AllowedPresets, id) || p.projectPreset(id)) {
			refused = append(refused, id)
			continue
		}
		out.Presets = append(out.Presets, pr.ID+"@"+pr.Version)
		for _, d := range pr.Destinations {
			r, _ := ParseDestination(d)
			add(r)
		}
	}
	for _, d := range p.Destinations {
		if d.OpenFor(p.Method) {
			for _, r := range d.Rules() {
				add(r)
			}
		}
	}
	for _, a := range p.Project {
		if a.Host != "" {
			add(buildplan.EgressRule{Host: a.Host, Port: a.Port, Internal: a.Internal})
		}
	}
	out.Allowances = p.projectIDs()
	return out, refused
}
