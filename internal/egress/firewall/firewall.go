// Package firewall renders the worker host's nftables egress rules (ADR 0025). Every packet a socket owned by the
// build user sends is rejected unless its original destination, before any NAT, is the egress proxy or an approved
// registry mirror (spike findings F5 and F7). This covers DNS, the internet, cloud metadata and services on the
// worker host itself.
package firewall

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

const Table = "samuori_egress"

type Config struct {
	BuildUID int
	Allow    []netip.AddrPort
}

// Ruleset returns an nftables script that atomically replaces the Samuori egress table.
func Ruleset(c Config) (string, error) {
	if c.BuildUID <= 0 {
		return "", errors.New("firewall: the build user must be a dedicated, non-root user")
	}
	if len(c.Allow) == 0 {
		return "", errors.New("firewall: at least the egress proxy address is required")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s\ndelete table inet %s\n", Table, Table)
	fmt.Fprintf(&b, "table inet %s {\n\tchain output {\n\t\ttype filter hook output priority filter; policy accept;\n", Table)
	for _, a := range c.Allow {
		if !a.IsValid() || a.Port() == 0 || a.Addr().IsUnspecified() {
			return "", fmt.Errorf("firewall: %s is not a specific address and port", a)
		}
		family := "ip"
		if a.Addr().Unmap().Is6() {
			family = "ip6"
		}
		fmt.Fprintf(&b, "\t\tmeta skuid %d meta l4proto tcp ct original %s daddr %s ct original proto-dst %d accept\n", c.BuildUID, family, a.Addr().Unmap(), a.Port())
	}
	fmt.Fprintf(&b, "\t\tmeta skuid %d meta l4proto tcp reject with tcp reset\n", c.BuildUID)
	fmt.Fprintf(&b, "\t\tmeta skuid %d reject with icmpx admin-prohibited\n", c.BuildUID)
	b.WriteString("\t}\n}\n")
	return b.String(), nil
}
