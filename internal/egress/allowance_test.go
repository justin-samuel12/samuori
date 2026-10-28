package egress

import (
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/egress/proxy"
)

// TestRequestableDestination: project members may request public names and addresses as public, and private
// addresses and internal names only as internal; loopback, link-local and metadata are refused either way (ADR 0052).
func TestRequestableDestination(t *testing.T) {
	for _, d := range []string{"github.com:443", "release-assets.githubusercontent.com:443", "8.8.8.8:443"} {
		if _, err := RequestableDestination(d); err != nil {
			t.Errorf("%s refused: %v", d, err)
		}
	}
	for _, d := range []string{"127.0.0.1:80", "localhost:80", "a.localhost:80", "169.254.169.254:80", "[::1]:443", "10.0.0.5:443", "192.168.1.1:443",
		"metadata.google.internal:80", "metadata:80", "build-cache.corp:443", "printer.local:631", "github.com:0", "GitHub.com:443"} {
		if _, err := RequestableDestination(d); err == nil {
			t.Errorf("%s may be requested as public", d)
		}
	}
	for _, d := range []string{"10.0.0.5:443", "192.168.1.1:443", "build-cache.corp:443", "nexus:8081", "github.com:443"} {
		if r, err := RequestableInternal(d); err != nil || !r.Internal {
			t.Errorf("%s refused as internal: %v", d, err)
		}
	}
	for _, d := range []string{"127.0.0.1:80", "localhost:80", "169.254.169.254:80", "[::1]:443", "[fe80::1]:443", "0.0.0.0:80", "GitHub.com:443"} {
		if _, err := RequestableInternal(d); err == nil {
			t.Errorf("%s may be requested as internal", d)
		}
	}
}

// TestAdviseProjectAllowance: a destination the project's allowance opened is advised as allowed for the project; a
// requestable public denial says a developer may request it, and a private one is never requestable.
func TestAdviseProjectAllowance(t *testing.T) {
	cat, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	org := cat.DefaultPolicy()
	deny := func(host string, port int) Denial {
		return Denial{Host: host, Port: port, Reason: proxy.ReasonNotAllowed}
	}
	a := cat.Advise(deny("github.com", 443), "dockerfile", org)
	if !a.Requestable || !strings.HasSuffix(a.Hint, RequestHint) {
		t.Errorf("public denial = %+v", a)
	}
	widened := org.ForProject("dockerfile", []Allowance{{ID: "x", Host: "github.com", Port: 443, Methods: []string{"dockerfile"}}, {ID: "y", Preset: "npm", Methods: []string{"dockerfile"}}})
	for _, d := range []Denial{deny("github.com", 443), deny("registry.npmjs.org", 443)} {
		if a := cat.Advise(d, "dockerfile", widened); a.Requestable || !strings.Contains(a.Hint, "allowed for this project") {
			t.Errorf("%s after approval = %+v", d.Host, a)
		}
	}
	if a := cat.Advise(deny("github.com", 443), "recipe", org.ForProject("recipe", widened.Project)); !a.Requestable {
		t.Errorf("a Dockerfile allowance counted for a recipe build: %+v", a)
	}
	if a := cat.Advise(deny("10.1.2.3", 443), "dockerfile", org); !a.Requestable || a.Network != NetworkInternal {
		t.Errorf("private denial = %+v", a)
	}
	if a := cat.Advise(deny("github.com", 443), "dockerfile", org); a.Network != NetworkPublic {
		t.Errorf("public denial = %+v", a)
	}
	if a := cat.Advise(deny("169.254.169.254", 80), "dockerfile", org); a.Requestable || a.Allowable {
		t.Errorf("metadata denial = %+v", a)
	}
}
