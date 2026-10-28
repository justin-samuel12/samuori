// Package buildplan defines the immutable, digest-addressed plan the controller resolves at admission and workers
// execute. It holds logical secret IDs, never values or provider references (docs/architecture/buildspec.md).
package buildplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/distribution/reference"

	"samuori.invalid/samuori/internal/recipedef"

	"samuori.invalid/samuori/internal/buildspec"
)

type Plan struct {
	Version          int               `json:"version"`
	SnapshotDigest   string            `json:"snapshotDigest"`
	SpecDigest       string            `json:"specDigest"`
	Method           string            `json:"method"`
	Recipe           *PlanRecipe       `json:"recipe,omitempty"`
	Dockerfile       *PlanDockerfile   `json:"dockerfile,omitempty"`
	Railpack         *PlanRailpack     `json:"railpack,omitempty"`
	Buildpacks       *PlanBuildpacks   `json:"buildpacks,omitempty"`
	BuildArgs        map[string]string `json:"buildArgs,omitempty"`
	Platforms        []string          `json:"platforms"`
	Secrets          []PlanSecret      `json:"secrets"`
	IsolationProfile string            `json:"isolationProfile"`
	Limits           PlanLimits        `json:"limits"`
	Policies         map[string]string `json:"policies"`
	Cache            PlanCache         `json:"cache"`
	Egress           PlanEgress        `json:"egress"`
	Validation       PlanValidation    `json:"validation"`
	Image            *PlanImage        `json:"image,omitempty"`
}

// PlanImage is the metadata the build writes onto the image: labels into its config, annotations onto every image
// manifest and, for a multi-platform build, the index. Spec values are merged with the automatic OCI values at
// admission, so the digest covers exactly what is applied (docs/architecture/buildspec.md#image-labels-and-annotations).
type PlanImage struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PlanValidation is the runtime test the worker runs on the built image (docs/architecture/validation.md).
type PlanValidation struct {
	Profile        string `json:"profile"`
	Port           int    `json:"port,omitempty"`
	Path           string `json:"path,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	// Expected are sensitive outputs the spec declares; findings under them are explained, not excused.
	Expected []ExpectedOutput `json:"expected,omitempty"`
	SBOM     bool             `json:"sbom,omitempty"`
}

type ExpectedOutput struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

const (
	ProfileHTTPStartup  = "http-startup"
	ProfileProcessStart = "process-start"
	ProfileCLISmoke     = "cli-smoke"
	ProfileStaticServe  = "static-serve"
	ProfileNone         = "none-requested"
)

// PlanEgress is the build's complete egress allowlist; anything else is refused (ADR 0025).
type PlanEgress struct {
	Presets []string     `json:"presets"`
	Allow   []EgressRule `json:"allow"`
	// Allowances are the project egress allowances included in Allow and Presets (ADR 0052).
	Allowances []string `json:"allowances,omitempty"`
}

// EgressRule allows one host and port through the egress proxy. Internal rules are admin-defined destinations and
// may resolve to private addresses; all others must resolve to public addresses.
type EgressRule struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Internal bool   `json:"internal,omitempty"`
}

// PlanCache is the controller-assigned cache scope; workers never choose or list cache locations.
type PlanCache struct {
	Mode  string `json:"mode"`
	Scope string `json:"scope,omitempty"`
}

type PlanRecipe struct {
	ID         string            `json:"id"`
	Version    string            `json:"version"`
	Runtime    string            `json:"runtime"`
	BuildImage string            `json:"buildImage"`
	RunImage   string            `json:"runImage"`
	Install    buildspec.Command `json:"install,omitzero"`
	Build      buildspec.Command `json:"build,omitzero"`
	Start      buildspec.Command `json:"start,omitzero"`
	Test       buildspec.Command `json:"test,omitzero"`
	AppType    string            `json:"appType,omitempty"`
	Port       int               `json:"port,omitempty"`
	// RuntimeID and RuntimeSource are set for a runtime an administrator added (ADR 0051); the digest covers them.
	RuntimeID     string `json:"runtimeId,omitempty"`
	RuntimeSource string `json:"runtimeSource,omitempty"`
	// SystemPackages are the BuildSpec's OS packages, with the package manager admission found for each image.
	SystemPackages *PlanSystemPackages `json:"systemPackages,omitempty"`
	// Custom is the approved custom recipe version the worker renders, and Parameters its resolved values; the plan
	// digest covers both, so the worker builds what the administrator previewed (ADR 0041).
	Custom     *recipedef.Definition `json:"custom,omitempty"`
	Parameters map[string]string     `json:"parameters,omitempty"`
	// Path is the application's folder in the source; Frontend adds a browser frontend served by the web launcher
	// (ADR 0055).
	Path     string        `json:"path,omitempty"`
	Frontend *PlanFrontend `json:"frontend,omitempty"`
}

// LauncherContract is the web launcher interface version a frontend plan needs; workers that ship samuori-web announce it
// as the provider "samuori-web".
const (
	LauncherContract = "1"
	LauncherProvider = "samuori-web"
)

// FrontendRecipe is the built-in recipe whose runtimes and egress presets a frontend uses.
const (
	FrontendRecipe        = "node-service"
	FrontendRecipeVersion = "1.0.0"
)

// PlanFrontend is a frontend with every default resolved and its Node.js image pinned by digest.
type PlanFrontend struct {
	Path        string            `json:"path"`
	Image       string            `json:"image"`
	Node        string            `json:"node"`
	Install     buildspec.Command `json:"install"`
	Build       buildspec.Command `json:"build"`
	Output      string            `json:"output"`
	APIPrefixes []string          `json:"apiPrefixes"`
	BackendPort int               `json:"backendPort"`
	Launcher    string            `json:"launcher"`
}

type PlanSystemPackages struct {
	Build          []string `json:"build,omitempty"`
	BuildManager   string   `json:"buildManager,omitempty"`
	Runtime        []string `json:"runtime,omitempty"`
	RuntimeManager string   `json:"runtimeManager,omitempty"`
}

type PlanDockerfile struct {
	Context string `json:"context"`
	Path    string `json:"path"`
	Digest  string `json:"digest"`
	Target  string `json:"target,omitempty"`
	// BaseImagePins are FROM tags resolved at admission; the worker builds each pinned image in place of the tag
	// (ADR 0049). The plan digest covers them, so a rebuild of this plan uses the same images.
	BaseImagePins []BaseImagePin `json:"baseImagePins,omitempty"`
}

// BaseImagePin maps a FROM reference as written to the image it was resolved to, repository@sha256:<digest>.
type BaseImagePin struct {
	Ref   string `json:"ref"`
	Image string `json:"image"`
}

// PlanRailpack pins everything a railpack build runs: the binary by checksum per worker architecture, the frontend
// and the builder and runtime images by digest (ADR 0047). CacheKey prefixes every Railpack cache mount with the
// controller's cache scope; SecretEnv maps the environment variable each build secret reaches steps as to its
// logical ID. User, when set, is the numeric uid:gid the worker makes the image run as instead of root.
type PlanRailpack struct {
	Version        string            `json:"version"`
	Binaries       map[string]string `json:"binaries"`
	Frontend       string            `json:"frontend"`
	Images         []string          `json:"images"`
	Context        string            `json:"context"`
	BuildCommand   string            `json:"buildCommand,omitempty"`
	StartCommand   string            `json:"startCommand,omitempty"`
	CacheKey       string            `json:"cacheKey"`
	SecretEnv      map[string]string `json:"secretEnv,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	User           string            `json:"user,omitempty"`
}

// RailpackUser is the non-root user Railpack images run as unless the provider keeps Railpack's root user (ADR 0047).
const RailpackUser = "10001:10001"

// CNBPlatformAPI is the Cloud Native Buildpacks platform API workers drive the lifecycle with. Workers that can run
// buildpacks builds report it as their buildpacks version, since they install nothing for it (ADR 0047).
const CNBPlatformAPI = "0.14"

// PlanBuildpacks pins a buildpacks build: the builder and run images by digest and the CNB platform API the worker
// drives the lifecycle with (ADR 0047). Version is the approved lifecycle version, kept for lineage. Bindings name
// logical secrets only.
type PlanBuildpacks struct {
	Version        string            `json:"version"`
	PlatformAPI    string            `json:"platformApi"`
	Builder        string            `json:"builder"`
	RunImage       string            `json:"runImage"`
	Context        string            `json:"context"`
	Env            map[string]string `json:"env,omitempty"`
	Bindings       []PlanBinding     `json:"bindings,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
}

// PlanBinding is one file of a CNB service binding, filled from the logical secret SecretID.
type PlanBinding struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Key      string `json:"key"`
	SecretID string `json:"secretId"`
}

// PlanSecret is a logical secret with the binding version admitted for it; an empty BindingID is an optional
// secret with no binding, which is never mounted. Values and provider references never appear in a plan.
type PlanSecret struct {
	ID             string `json:"id"`
	Required       bool   `json:"required"`
	Target         string `json:"target"`
	BindingID      string `json:"bindingId,omitempty"`
	BindingVersion int    `json:"bindingVersion,omitempty"`
}

type PlanLimits struct {
	VCPUs         uint32 `json:"vcpus"`
	MemoryBytes   uint64 `json:"memoryBytes"`
	DiskBytes     uint64 `json:"diskBytes"`
	WallClockSecs uint32 `json:"wallClockSeconds"`
}

// DefaultLimits apply until resource policy exists.
var DefaultLimits = PlanLimits{VCPUs: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30, WallClockSecs: 3600}

func (p Plan) Encode() ([]byte, string) {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return raw, "sha256:" + hex.EncodeToString(sum[:])
}

// Digest hashes plan bytes exactly as a worker must when it verifies them (SEC-PLN-02).
func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Unknown reports whether the plan carries fields this release does not know, which means a newer controller wrote
// it.
func Unknown(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var p Plan
	return d.Decode(&p) != nil && json.Unmarshal(raw, &p) == nil
}

// Decode parses plan bytes after their digest has been verified.
func Decode(raw []byte) (Plan, error) {
	var p Plan
	err := json.Unmarshal(raw, &p)
	return p, err
}

// ContextName is the named-context key BuildKit's Dockerfile frontend looks up for a FROM reference: the familiar
// form of the normalized name with its tag, and no tag for ":latest" (dockerui.Client.NamedContext in BuildKit
// v0.34.0). "node:18-alpine" and "docker.io/library/node:18-alpine" both give "node:18-alpine".
func ContextName(ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(reference.FamiliarString(reference.TagNameOnly(named)), ":latest"), nil
}
