package setupcli

import (
	"net"
	"strings"
	"testing"
)

func TestPickHostname(t *testing.T) {
	here := []net.IP{net.ParseIP("10.0.0.30")}
	dns := map[string]string{
		"samuori-controller01.aws.example.com": "10.0.0.30",
		"other.aws.example.com":                "10.0.0.99",
		"etc-hosts.aws.example.com":            "127.0.1.1",
	}
	resolve := func(n string) []net.IP {
		if ip, ok := dns[n]; ok {
			return []net.IP{net.ParseIP(ip)}
		}
		return nil
	}
	meta := "ip-10-104-25-30.aws.example.com"
	for _, c := range []struct {
		name, os string
		search   []string
		want     string
	}{
		{"short uppercase name qualified by the search domain", "Samuori-CONTROLLER01", []string{"aws.example.com"}, "samuori-controller01.aws.example.com"},
		{"an FQDN that points here", "Samuori-CONTROLLER01.aws.example.com", nil, "samuori-controller01.aws.example.com"},
		{"the host's own name mapped to 127.0.1.1 in /etc/hosts", "ETC-HOSTS", []string{"aws.example.com"}, "etc-hosts.aws.example.com"},
		{"a name that points at another host falls back to metadata", "OTHER", []string{"aws.example.com"}, meta},
		{"an unresolvable name falls back to metadata", "box", []string{"aws.example.com"}, meta},
	} {
		got := PickHostname(HostFacts{OSName: c.os, SearchDomains: c.search, LocalAddrs: here, MetadataName: meta, Resolve: resolve})
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := PickHostname(HostFacts{OSName: "Box", Resolve: resolve}); got != "box" {
		t.Errorf("without metadata or DNS: got %q, want the lowercased OS name", got)
	}
	if d := SearchDomains(strings.NewReader("nameserver 127.0.0.53\nsearch aws.example.com example.com\noptions edns0\n")); strings.Join(d, ",") != "aws.example.com,example.com" {
		t.Errorf("search domains = %v", d)
	}
}
