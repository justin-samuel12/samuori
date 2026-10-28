package egress

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/egress/proxy"
)

// TestCleanDenials: the controller keeps only well-formed destinations from a worker's report, bounded and
// de-duplicated, and replaces reasons the proxy does not produce.
func TestCleanDenials(t *testing.T) {
	in := []Denial{
		{Host: "registry.npmjs.org", Port: 443, Count: 999, Reason: proxy.ReasonNotAllowed},
		{Host: "registry.npmjs.org", Port: 443, Count: 1, Reason: proxy.ReasonNotAllowed},
		{Host: "Evil Host", Port: 443, Count: 1},
		{Host: "bad_host.example", Port: 443, Count: 1},
		{Host: "fe80::1%eth0", Port: 443, Count: 1},
		{Host: "", Port: 443, Count: 1},
		{Host: strings.Repeat("a.", 130) + "com", Port: 443, Count: 1},
		{Host: "github.com", Port: 0, Count: 1},
		{Host: "github.com", Port: 70000, Count: 1},
		{Host: "github.com", Port: 443, Count: -1},
		{Host: "2001:db8::1", Port: 443, Count: 2, Reason: "click here to allow"},
		{Host: "internal.example", Port: 443, Count: 1, Reason: proxy.ReasonBlockedAddress + " (10.1.2.3)"},
	}
	out := CleanDenials(in)
	if len(out) != 3 || out[0].Host != "registry.npmjs.org" || out[0].Count != 999 || out[1].Host != "2001:db8::1" || out[2].Host != "internal.example" {
		t.Fatalf("cleaned = %+v", out)
	}
	if out[1].Reason != "denied by the egress proxy" || out[2].Reason != proxy.ReasonBlockedAddress+" (10.1.2.3)" {
		t.Errorf("reasons = %q, %q", out[1].Reason, out[2].Reason)
	}
	var many []Denial
	for i := range 80 {
		many = append(many, Denial{Host: fmt.Sprintf("h%d.example", i), Port: 443, Count: 1, Reason: proxy.ReasonNotAllowed})
	}
	if n := len(CleanDenials(many)); n != MaxDenials {
		t.Errorf("kept %d of 80 denials; want %d", n, MaxDenials)
	}
}

// TestAdvise: each hint is derived from the preset catalog and the current policy for the build's method, and
// addresses the proxy refuses on principle, or non-public ones, get no suggestion.
func TestAdvise(t *testing.T) {
	cat, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	pol := Policy{AllowedPresets: []string{"alpine", "npm"}, DockerfilePresets: []string{"alpine"},
		Destinations: []Destination{{Name: "artifacts", Host: "artifacts.corp.example", Port: 443, Reason: "internal"}}}
	denied := func(host string, port int) Denial {
		return Denial{Host: host, Port: port, Count: 1, Reason: proxy.ReasonNotAllowed}
	}
	cases := []struct {
		name      string
		d         Denial
		method    string
		allowable bool
		presets   []string
		want      string
	}{
		{"preset allowed but not for Dockerfile", denied("registry.npmjs.org", 443), "dockerfile", true, []string{"npm"}, "can set npm to reach Dockerfile builds"},
		{"preset not allowed", denied("pypi.org", 443), "dockerfile", true, []string{"pypi"}, "can set pypi to reach Dockerfile builds"},
		{"preset already allowed now", denied("dl-cdn.alpinelinux.org", 443), "dockerfile", true, []string{"alpine"}, "now reaches Dockerfile builds; run the build again"},
		{"no preset", denied("updates.example.org", 443), "dockerfile", true, []string{}, "updates.example.org:443 is in no egress preset; an organization administrator can add it as a public destination"},
		{"GitHub releases for Dockerfile builds", denied("github.com", 443), "dockerfile", true, []string{"github-releases"}, "can set github-releases to reach Dockerfile builds"},
		{"GitHub releases for recipe builds", denied("github.com", 443), "recipe", true, []string{"github-releases"}, "can set github-releases to All builds"},
		{"railpack-only preset not offered to recipe builds", denied("mise.jdx.dev", 443), "recipe", true, []string{}, "mise.jdx.dev:443 is in the mise preset, which is for Railpack builds; an organization administrator can add it as a public destination"},
		{"railpack-only preset offered to Railpack", denied("github.com", 443), "railpack", true, []string{"github-releases", "mise"}, "add mise to the Railpack provider's egress presets"},
		{"recipe", denied("registry.npmjs.org", 443), "recipe", true, []string{"npm"}, "recipe does not use"},
		{"railpack", denied("pypi.org", 443), "railpack", true, []string{"pypi"}, "add pypi to the Railpack provider's egress presets under Build & conversion providers"},
		{"destination already in policy", denied("artifacts.corp.example", 443), "dockerfile", true, []string{}, "now an egress destination"},
		{"loopback literal", denied("127.0.0.1", 80), "dockerfile", false, []string{}, "cannot be allowed: the proxy never permits loopback"},
		{"metadata address", denied("169.254.169.254", 80), "dockerfile", false, []string{}, "cannot be allowed"},
		{"localhost name", denied("localhost", 8080), "dockerfile", false, []string{}, "cannot be allowed"},
		{"private literal", denied("10.0.0.5", 443), "dockerfile", true, []string{}, "can add it as an internal destination"},
		{"resolves to loopback", Denial{Host: "rebind.example", Port: 443, Count: 1, Reason: proxy.ReasonBlockedAddress + " (127.0.0.1)"}, "dockerfile", false, []string{}, "cannot be allowed"},
		{"resolves to private", Denial{Host: "intranet.example", Port: 443, Count: 1, Reason: proxy.ReasonBlockedAddress + " (192.168.1.4)"}, "dockerfile", true, []string{}, "resolved to the private address 192.168.1.4"},
		{"unresolved", Denial{Host: "typo.example", Port: 443, Count: 1, Reason: proxy.ReasonUnresolved}, "dockerfile", true, []string{}, "did not resolve"},
	}
	for _, c := range cases {
		a := cat.Advise(c.d, c.method, pol)
		if a.Allowable != c.allowable || !slices.Equal(a.Presets, c.presets) || !strings.Contains(a.Hint, c.want) {
			t.Errorf("%s: %+v; want allowable %v, presets %v, hint containing %q", c.name, a, c.allowable, c.presets, c.want)
		}
		if !c.allowable && (strings.Contains(a.Hint, "can add") || strings.Contains(a.Hint, "can allow")) {
			t.Errorf("%s: a refused destination got a suggestion: %s", c.name, a.Hint)
		}
	}
}
