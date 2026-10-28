package isolation

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Host describes virtualization facts found on this machine. Facts are not verification: a profile becomes usable
// for untrusted builds only after its pool passes the self-test.
type Host struct {
	OS, Arch          string
	KVM               bool
	NestedVirt        bool
	UserNamespaces    bool
	NewUIDMap         bool
	CandidateProfiles []Profile
	Notes             []string
}

// DetectHost reads facts only; it starts nothing.
func DetectHost() Host {
	h := Host{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if h.OS != "linux" {
		h.Notes = append(h.Notes, "Build isolation requires Linux; this host can enroll but not build.")
		return h
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		h.KVM = true
	} else if _, statErr := os.Stat("/dev/kvm"); statErr == nil {
		h.Notes = append(h.Notes, "/dev/kvm exists but this user cannot open it; add the worker user to the kvm group.")
	} else {
		h.Notes = append(h.Notes, "No /dev/kvm: vm-per-job needs hardware virtualization (enable it, or nested virtualization on a VM host).")
	}
	for _, p := range []string{"/sys/module/kvm_intel/parameters/nested", "/sys/module/kvm_amd/parameters/nested"} {
		if b, err := os.ReadFile(p); err == nil && (strings.HasPrefix(string(b), "Y") || strings.HasPrefix(string(b), "1")) {
			h.NestedVirt = true
		}
	}
	if b, err := os.ReadFile("/proc/sys/user/max_user_namespaces"); err == nil && strings.TrimSpace(string(b)) != "0" {
		h.UserNamespaces = true
	}
	if _, err := exec.LookPath("newuidmap"); err == nil {
		h.NewUIDMap = true
	}
	if h.KVM {
		h.CandidateProfiles = append(h.CandidateProfiles, VMPerJob)
	}
	if h.UserNamespaces && h.NewUIDMap {
		h.CandidateProfiles = append(h.CandidateProfiles, RootlessDedicatedVM, RootlessHost)
	} else {
		h.Notes = append(h.Notes, "Rootless builds need user namespaces and newuidmap (package uidmap).")
	}
	return h
}
