// Package cloudhypervisor is the vm-per-job isolation provider: each build attempt runs in its own Cloud
// Hypervisor VM booted from a digest-pinned guest image and destroyed afterwards (ADR 0038). Linux with KVM only.
package cloudhypervisor
