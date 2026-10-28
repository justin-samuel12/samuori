package isolation

import (
	"slices"
	"sort"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/engine"
)

// SelfTestChecks run inside a worker's own environments; a worker is verified only while all pass (ADR 0027, OD-22).
var SelfTestChecks = []string{
	"SEC-ISO-01", "SEC-ISO-02", "SEC-ISO-03", "SEC-ISO-04", "SEC-ISO-05", "SEC-ISO-07", "SEC-ISO-08",
	"SEC-NET-01", "SEC-DOS-02", "SEC-DOS-03",
}

// vmPerJobChecks are added for vm-per-job, whose isolation also covers state between attempts and disk use.
var vmPerJobChecks = []string{"SEC-ISO-09", "SEC-DOS-04"}

// SelfTestChecksFor lists the checks a pool of this profile must pass.
func SelfTestChecksFor(p Profile) []string {
	if p == VMPerJob {
		return append(append([]string{}, SelfTestChecks...), vmPerJobChecks...)
	}
	return SelfTestChecks
}

// SelfTestFreshness is how long a passed self-test keeps a worker verified.
const SelfTestFreshness = 24 * time.Hour

type PoolState struct {
	Profile              Profile
	ProfileApproved      bool
	TrustedWorkloadsOnly bool
}

// SelfTestState is a worker's newest finished self-test.
type SelfTestState struct {
	Status       string
	FinishedAt   *time.Time
	Profile      string
	ConfigDigest string
}

type WorkerState struct {
	EnrollmentActive bool
	// Unassigned workers belong to no pool and receive no builds of any kind (ADR 0045).
	Unassigned       bool
	OperationalState string
	ProtocolMajor    uint32
	// VersionCompatible applies the version matrix (D28) to the worker's release against the controller's.
	VersionCompatible bool
	OS                string
	Architectures     []string
	IsolationProfiles []string
	FreeSlots         uint32
	StrictEgress      bool
	Engines           []string
	// Providers are the optional build providers installed on the worker, by name and version (ADR 0047).
	Providers map[string]string
	// ConfigDigest identifies the worker's security-relevant configuration; a self-test covers only the digest it
	// ran against.
	ConfigDigest string
	SelfTest     SelfTestState
}

type Requirement struct {
	Untrusted bool
	Platforms []string
	// Secrets needs an engine that delivers secrets just in time (ADR 0029).
	Secrets bool
	// Providers need exactly these versions installed on the worker, such as the approved Railpack.
	Providers map[string]string
	// BaseImagePins need an engine that builds a FROM tag as the image it was pinned to (ADR 0049).
	BaseImagePins bool
}

// Eligible decides whether a worker in a pool may receive a build. Untrusted (uploaded) source needs an approved
// profile that allows untrusted work, a fresh passing self-test of this worker for that profile and its current
// configuration, and strict egress (ADR 0025); every reason that applies is returned.
func Eligible(pool PoolState, w WorkerState, req Requirement, now time.Time) (bool, []string) {
	reasons := map[string]bool{}
	if req.Untrusted {
		if !pool.ProfileApproved {
			reasons["profile-not-approved"] = true
		}
		if !pool.Profile.UntrustedAllowed() || pool.TrustedWorkloadsOnly {
			reasons["profile-not-approved"] = true
		}
		t := w.SelfTest
		switch {
		case t.Status == "failed":
			reasons["worker-self-test-failed"] = true
		case t.Status != "passed" || t.FinishedAt == nil || now.Sub(*t.FinishedAt) > SelfTestFreshness:
			reasons["worker-self-test-stale"] = true
		case t.Profile != string(pool.Profile) || w.ConfigDigest == "" || t.ConfigDigest != w.ConfigDigest:
			reasons["worker-config-changed"] = true
		}
		if !w.StrictEgress {
			reasons["egress-not-strict"] = true
		}
	}
	if w.Unassigned {
		reasons["unassigned"] = true
	}
	if !w.EnrollmentActive {
		reasons["revoked"] = true
	}
	switch w.OperationalState {
	case "cordoned", "draining":
		reasons["cordoned"] = true
	case "quarantined":
		reasons["quarantined"] = true
	}
	if w.ProtocolMajor != 1 || !w.VersionCompatible {
		reasons["version-incompatible"] = true
	}
	if !contains(w.IsolationProfiles, string(pool.Profile)) {
		reasons["profile-not-approved"] = true
	}
	for _, p := range req.Platforms {
		os, arch := split(p)
		if os != w.OS || !contains(w.Architectures, arch) {
			reasons["platform-unsupported"] = true
		}
	}
	if w.FreeSlots == 0 {
		reasons["no-free-daemon-slot"] = true
	}
	if req.Secrets && len(w.Engines) > 0 && !slices.ContainsFunc(w.Engines, engine.DeliversSecrets) {
		reasons["engine-cannot-deliver-secrets"] = true
	}
	if req.BaseImagePins && len(w.Engines) > 0 && !slices.Contains(w.Engines, engine.BuildKit) {
		reasons["engine-cannot-pin-base-images"] = true
	}
	for name, v := range req.Providers {
		if have, ok := w.Providers[name]; !ok || strings.TrimPrefix(have, "v") != strings.TrimPrefix(v, "v") {
			reasons["provider-missing"] = true
		}
	}
	out := make([]string, 0, len(reasons))
	for r := range reasons {
		out = append(out, r)
	}
	sort.Strings(out)
	return len(out) == 0, out
}

func split(platform string) (string, string) {
	for i := 0; i < len(platform); i++ {
		if platform[i] == '/' {
			return platform[:i], platform[i+1:]
		}
	}
	return platform, ""
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
