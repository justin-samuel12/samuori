// Package isolation defines how workers create, run and destroy build environments (ADR 0027). Concrete
// providers (local KVM, Proxmox VE, AWS EC2, ...) implement Provider; none is assumed to exist.
package isolation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

type Profile string

const (
	VMPerJob            Profile = "vm-per-job"
	RootlessDedicatedVM Profile = "rootless-dedicated-vm"
	RootlessHost        Profile = "rootless-host"
)

func (p Profile) Valid() bool { return p == VMPerJob || p == RootlessDedicatedVM || p == RootlessHost }

// UntrustedAllowed reports whether a profile may ever run uploaded source; rootless-host is for trusted workloads only.
func (p Profile) UntrustedAllowed() bool { return p == VMPerJob || p == RootlessDedicatedVM }

type StartupClass string

const (
	StartupSeconds StartupClass = "seconds"
	StartupMinutes StartupClass = "minutes"
)

type Capabilities struct {
	Provider       string
	Profiles       []Profile
	OS             string
	Architectures  []string
	NestedVirt     bool
	MaxVCPUs       uint32
	MaxMemoryBytes uint64
	MaxDiskBytes   uint64
	Startup        StartupClass
	FreeSlots      uint32
	Engines        []string
	// StrictEgress reports that build traffic is confined to the egress proxy and mirrors (ADR 0025).
	StrictEgress bool
	// ConfigDigest changes whenever the provider's security-relevant configuration or tools change.
	ConfigDigest string
	// Providers are the optional build providers this environment can run, by name and version (ADR 0047).
	Providers map[string]string
}

type Limits struct {
	VCPUs       uint32
	MemoryBytes uint64
	DiskBytes   uint64
	WallClock   time.Duration
}

// Spec describes one environment for one attempt. BaseImage is the VM image, required for vm-per-job and
// digest-pinned whenever set; rootless profiles boot none. SourceDir holds the verified source the worker fetched
// for this attempt. OutputDir, owned by the caller, receives the artifact so it outlives the environment.
type Spec struct {
	// Progress receives build output while the build runs; the manager drops it for secret-bearing builds.
	Progress      io.Writer
	AttemptID     string
	SourceDir     string
	OutputDir     string
	Profile       Profile
	BaseImage     string
	Limits        Limits
	SecretBearing bool
	TrustDomain   string
	// ReleaseSecret asks the controller for one secret under the attempt's lease. It returns ErrSecretUnbound for an
	// optional secret with no binding and an error for every other refusal.
	ReleaseSecret func(ctx context.Context, logicalID string) ([]byte, error)
	// MirrorToken asks the controller for a token for an authorizing registry mirror, scoped to the repositories the
	// attempt's team may pull (ADR 0030). Providers hand it only to the build engine's registry client.
	MirrorToken func(ctx context.Context, mirror string, scopes []string) (token string, expiresIn int64, issuedAt time.Time, err error)
	// CacheDir holds the build cache the controller chose for the plan's scope, already verified; empty for none.
	CacheDir string
}

var ErrSecretUnbound = errors.New("the optional secret is not bound")

// SelfTester runs the worker self-test checks (SelfTestChecks) inside a provider's own environments (ADR 0027).
type SelfTester interface {
	SelfTest(ctx context.Context) []Check
}

type Env struct {
	ID        string
	AttemptID string
	// Progress receives build output as it happens; nil for builds that use secrets, whose output is masked only
	// once the build ends.
	Progress io.Writer
	Profile  Profile
	Created  time.Time
	Deadline time.Time
}

// RunResult points at the image the environment produced (an OCI layout archive) on the worker's disk.
type RunResult struct {
	ArtifactDigest string
	ArtifactFile   string
	Logs           []byte
	// Checks are validation results only the build environment can produce: the build-secret canary, which needs
	// the delivered values, and the runtime test.
	Checks []Check
	// CacheFile is this build's cache archive for the plan's scope, when the engine exported one.
	CacheFile, CacheDigest string
	// Records are lineage files the build produced besides the image, such as the Railpack plan it ran.
	Records []Record
	// Egress is what the worker's egress proxy denied; it comes from the proxy, never from build output.
	Egress EgressReport
}

type EgressReport struct {
	Denied []EgressDenial
	// Unrecorded counts decisions beyond the proxy's per-attempt limit.
	Unrecorded int
}

type EgressDenial struct {
	Host   string
	Port   int
	Count  int
	Reason string
}

// Record is one lineage file, uploaded with its artifact kind.
type Record struct {
	Kind, File, Digest string
}

// Check results carry rule, path, layer and size only, never contents or secret values.
type Check struct {
	ID       string
	Category string
	Outcome  string
	Reason   string
	Findings []CheckFinding
}

type CheckFinding struct {
	Rule, Class, Path, Layer string
	Size                     int64
	SecretID                 string
}

const (
	CheckCanary      = "build-secret-canary"
	CheckRuntimePref = "runtime-"
)

type CleanupOutcome string

const (
	CleanupDestroyed   CleanupOutcome = "destroyed"
	CleanupQuarantined CleanupOutcome = "failed-quarantined"
)

type Cleanup struct {
	EnvID, AttemptID string
	Outcome          CleanupOutcome
	Detail           string
}

// Provider is implemented once per virtualization technology.
type Provider interface {
	Name() string
	Capabilities(ctx context.Context) (Capabilities, error)
	Prepare(ctx context.Context, s Spec) (Env, error)
	Run(ctx context.Context, e Env, plan []byte) (RunResult, error)
	Collect(ctx context.Context, e Env) error
	// Destroy tears the environment down and verifies nothing persists.
	Destroy(ctx context.Context, e Env) error
	// Environments lists environments the provider still holds, for reconciliation.
	Environments(ctx context.Context) ([]Env, error)
}

// ConvertSpec is one conversion run in an environment (ADR 0048). ToolDir holds the verified tool copy, InputDir a
// verified copy of the untrusted input; OutputDir, owned by the caller, receives the step's /out.
type ConvertSpec struct {
	ToolDir   string
	InputDir  string
	OutputDir string
	// Run is the tool runner's specification (toolrun.Spec) as JSON.
	Run []byte
}

type ConvertResult struct {
	Logs []byte
}

// Converter is implemented by providers that can run a conversion tool with no network, no secrets and no registry
// credentials. ConvertReady explains why a provider cannot, so the worker does not advertise conversion providers.
type Converter interface {
	ConvertReady() error
	Convert(ctx context.Context, e Env, s ConvertSpec) (ConvertResult, error)
}

// Failure classes distinguish infrastructure problems (a new attempt may help) from build and limit failures.
var (
	ErrOccupied       = errors.New("isolation: every daemon slot is busy")
	ErrUnsupported    = errors.New("isolation: the provider cannot satisfy this environment")
	ErrResourceLimit  = errors.New("isolation: the build exceeded a resource limit")
	ErrInfrastructure = errors.New("isolation: infrastructure failure")
	ErrTimeout        = errors.New("isolation: the build exceeded its wall-clock limit")
)

// Failure records the class and a detail safe to show to operators.
type Failure struct {
	Class  error
	Detail string
}

func (f *Failure) Error() string { return fmt.Sprintf("%v: %s", f.Class, f.Detail) }
func (f *Failure) Unwrap() error { return f.Class }

// Requeue reports whether a failure justifies a new attempt; application failures and limits never do.
func Requeue(err error) bool { return errors.Is(err, ErrInfrastructure) }
