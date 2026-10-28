package egress

import (
	"fmt"
	"strings"
	"testing"
)

// TestPolicyDestinationNetwork: a public policy destination or project allowance keeps the proxy's private-address
// refusal and must be a public name; an internal one (the policy's default, as before) may resolve privately.
func TestPolicyDestinationNetwork(t *testing.T) {
	cat, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p := cat.DefaultPolicy()
	p.Destinations = []Destination{
		{Name: "github-objects", Host: "objects.githubusercontent.com", Port: 443, Reason: "sharp", Network: NetworkPublic},
		{Name: "nexus", Host: "nexus.corp", Port: 443, Reason: "mirror"},
		{Name: "cache", Host: "10.0.0.5", Port: 8080, Reason: "cache", Network: NetworkInternal},
	}
	if err := cat.ValidatePolicy(p); err != nil {
		t.Fatal(err)
	}
	p = p.ForProject("dockerfile", []Allowance{
		{ID: "a1", Host: "github.com", Port: 443, Methods: []string{"dockerfile"}},
		{ID: "a2", Host: "artifacts.corp", Port: 443, Methods: []string{"dockerfile"}, Internal: true},
	})
	plan, _ := cat.Resolve(nil, p)
	want := map[string]bool{"objects.githubusercontent.com": false, "nexus.corp": true, "10.0.0.5": true, "github.com": false, "artifacts.corp": true}
	for _, r := range plan.Allow {
		if internal, ok := want[r.Host]; ok {
			if r.Internal != internal {
				t.Errorf("%s internal = %v, want %v", r.Host, r.Internal, internal)
			}
			delete(want, r.Host)
		}
	}
	if len(want) > 0 {
		t.Errorf("not in the plan: %v", want)
	}
	for _, bad := range []Destination{
		{Name: "a", Host: "nexus.corp", Port: 443, Reason: "r", Network: NetworkPublic},
		{Name: "b", Host: "10.0.0.5", Port: 443, Reason: "r", Network: NetworkPublic},
		{Name: "c", Host: "example.com", Port: 443, Reason: "r", Network: "anywhere"},
	} {
		q := cat.DefaultPolicy()
		q.Destinations = []Destination{bad}
		if cat.ValidatePolicy(q) == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// TestDestinationMethods: a disabled destination opens for no build, and one closed to Dockerfile builds still opens
// for the other methods, as preset switches do.
func TestDestinationMethods(t *testing.T) {
	cat, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p := cat.DefaultPolicy()
	p.Destinations = []Destination{
		{Name: "all", Host: "all.example.com", Port: 443, Network: NetworkPublic},
		{Name: "no-df", Host: "nodf.example.com", Port: 443, Network: NetworkPublic, NoDockerfile: true},
		{Name: "off", Host: "off.example.com", Port: 443, Network: NetworkPublic, Disabled: true},
	}
	hosts := func(method string) map[string]bool {
		plan, _ := cat.Resolve(nil, p.ForMethod(method))
		out := map[string]bool{}
		for _, r := range plan.Allow {
			out[r.Host] = true
		}
		return out
	}
	if h := hosts("dockerfile"); !h["all.example.com"] || h["nodf.example.com"] || h["off.example.com"] {
		t.Errorf("dockerfile = %v", h)
	}
	if h := hosts("recipe"); !h["all.example.com"] || !h["nodf.example.com"] || h["off.example.com"] {
		t.Errorf("recipe = %v", h)
	}
	if a := cat.Advise(Denial{Host: "nodf.example.com", Port: 443, Reason: "not on the build's egress allowlist"}, "dockerfile", p); strings.Contains(a.Hint, "now an egress destination") {
		t.Errorf("advice claims a destination closed to Dockerfile builds is open: %s", a.Hint)
	}
}

// TestEveryBuildPresetsAndAddresses: a preset set to All builds reaches builds whose recipe does not ask for it, and
// every address of a multi-address destination is opened under one name.
func TestEveryBuildPresetsAndAddresses(t *testing.T) {
	cat, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p := cat.DefaultPolicy()
	p.EveryBuildPresets = []string{"github-releases"}
	p.Destinations = []Destination{{Name: "corp-mirror", Host: "a.example.com", Port: 443, Network: NetworkPublic, Also: []string{"b.example.com:443", "c.example.com:8443"}}}
	if err := cat.ValidatePolicy(p); err != nil {
		t.Fatal(err)
	}
	plan, refused := cat.Resolve([]string{"npm"}, p.ForMethod("recipe"))
	hosts := map[string]bool{}
	for _, r := range plan.Allow {
		hosts[fmt.Sprintf("%s:%d", r.Host, r.Port)] = !r.Internal
	}
	for _, want := range []string{"registry.npmjs.org:443", "github.com:443", "objects.githubusercontent.com:443", "a.example.com:443", "b.example.com:443", "c.example.com:8443"} {
		if !hosts[want] {
			t.Errorf("%s missing or not public in %v (refused %v)", want, hosts, refused)
		}
	}
	bad := p
	bad.EveryBuildPresets = []string{"github-releases"}
	bad.AllowedPresets = []string{"npm"}
	if cat.ValidatePolicy(bad) == nil {
		t.Error("an every-build preset that is not allowed was accepted")
	}
	bad = cat.DefaultPolicy()
	bad.Destinations = []Destination{{Name: "x", Host: "a.example.com", Port: 443, Network: NetworkPublic, Also: []string{"nexus.corp:443"}}}
	if cat.ValidatePolicy(bad) == nil {
		t.Error("an internal name was accepted as an extra public address")
	}
	if !p.Destinations[0].Has("c.example.com", 8443) {
		t.Error("Has misses an extra address")
	}
}
