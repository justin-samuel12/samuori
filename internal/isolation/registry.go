package isolation

import (
	"fmt"
	"sort"
	"strings"
)

// Factory builds a provider from its operator-supplied settings.
type Factory func(settings map[string]string) (Provider, error)

var factories = map[string]Factory{}

// Register adds a provider implementation. Implementations live in internal/adapters/isolation/<name>.
func Register(name string, f Factory) { factories[name] = f }

// Known lists providers that exist in this build.
func Known() []string {
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Planned providers are designed (ADR 0027) but not implemented; kvm-cloud-hypervisor is the implemented one (ADR 0038).
var Planned = []string{"kvm-firecracker", "kvm-qemu", "proxmox-ve", "aws-ec2"}

func Open(name string, settings map[string]string) (Provider, error) {
	if name == "" || name == "none" {
		return nil, nil
	}
	if f, ok := factories[name]; ok {
		return f(settings)
	}
	for _, p := range Planned {
		if p == name {
			return nil, fmt.Errorf("isolation provider %q is planned but not available yet: it needs validation on a KVM host (OD-12b)", name)
		}
	}
	return nil, fmt.Errorf("unknown isolation provider %q (available: %s)", name, strings.Join(append([]string{"none"}, Known()...), ", "))
}
