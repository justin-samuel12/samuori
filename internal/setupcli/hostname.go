package setupcli

import (
	"bufio"
	"io"
	"net"
	"strings"
)

// HostFacts is what PickHostname chooses from.
type HostFacts struct {
	OSName        string
	SearchDomains []string
	LocalAddrs    []net.IP
	MetadataName  string
	Resolve       func(name string) []net.IP
}

// PickHostname prefers OwnName; then the cloud metadata name; then the bare OS name, lowercased.
func PickHostname(f HostFacts) string {
	if name, ok := OwnName(f); ok {
		return name
	}
	if f.MetadataName != "" {
		return strings.ToLower(f.MetadataName)
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(f.OSName), "."))
}

// OwnName is the OS hostname, lowercased and qualified with a resolver search domain when it is short, if that name
// resolves to one of this host's own addresses.
func OwnName(f HostFacts) (string, bool) {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(f.OSName), "."))
	var candidates []string
	if strings.Contains(base, ".") {
		candidates = append(candidates, base)
	} else if base != "" {
		for _, d := range f.SearchDomains {
			if d = strings.ToLower(strings.Trim(d, ".")); d != "" {
				candidates = append(candidates, base+"."+d)
			}
		}
	}
	for _, c := range candidates {
		if f.Resolve != nil && pointsHere(f.Resolve(c), f.LocalAddrs) {
			return c, true
		}
	}
	return "", false
}

// pointsHere counts a loopback answer as this host: Debian and Ubuntu map the host's own name to 127.0.1.1 in
// /etc/hosts.
func pointsHere(resolved, local []net.IP) bool {
	for _, r := range resolved {
		if r.IsLoopback() {
			return true
		}
		for _, l := range local {
			if r.Equal(l) {
				return true
			}
		}
	}
	return false
}

// SearchDomains reads the search and domain lines of a resolv.conf.
func SearchDomains(r io.Reader) []string {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) > 1 && (f[0] == "search" || f[0] == "domain") {
			out = append(out, f[1:]...)
		}
	}
	return out
}
