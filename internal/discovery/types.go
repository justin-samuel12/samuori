// Package discovery inspects snapshot files statically to suggest how an application builds and runs. Detectors
// parse files only; they never execute code, spawn processes or use the network (docs/architecture/discovery.md).
package discovery

const (
	Strong        = 3
	Moderate      = 2
	Weak          = 1
	Contradicting = -2
)

type Evidence struct {
	Kind        string `json:"kind"`
	Path        string `json:"path"`
	Pointer     string `json:"pointer,omitempty"`
	Observation string `json:"observation"`
	Weight      int    `json:"weight"`
}

type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

type Finding struct {
	DetectorID         string            `json:"detectorId"`
	DetectorVersion    string            `json:"detectorVersion"`
	SourceSubpath      string            `json:"sourceSubpath"`
	Language           string            `json:"language,omitempty"`
	Framework          string            `json:"framework,omitempty"`
	ApplicationKind    string            `json:"applicationKind,omitempty"`
	RuntimeConstraints map[string]string `json:"runtimeConstraints,omitempty"`
	PackageManager     string            `json:"packageManager,omitempty"`
	SuggestedCommands  map[string]string `json:"suggestedCommands,omitempty"`
	RequiredPlatforms  []string          `json:"requiredPlatforms,omitempty"`
	Evidence           []Evidence        `json:"evidence"`
	Confidence         string            `json:"confidence"`
	Warnings           []Warning         `json:"warnings,omitempty"`
	NetworkNeeds       []NetworkNeed     `json:"networkNeeds,omitempty"`
	Readiness          []Readiness       `json:"readiness,omitempty"`
	scripts            map[string]string
}

type SecretMount struct {
	ID         string `json:"id"`
	Target     string `json:"target,omitempty"`
	Required   bool   `json:"required"`
	EnvExposed bool   `json:"envExposed"`
}

type DockerfileAnalysis struct {
	Path             string        `json:"path"`
	Frontend         string        `json:"frontend,omitempty"`
	FrontendApproved bool          `json:"frontendApproved"`
	Stages           []string      `json:"stages"`
	BaseImages       []string      `json:"baseImages,omitempty"`
	Args             []string      `json:"args"`
	ExposedPorts     []string      `json:"exposedPorts,omitempty"`
	SecretMounts     []SecretMount `json:"secretMounts"`
	Ambiguities      []Warning     `json:"ambiguities"`
	RefusedFeatures  []Warning     `json:"refusedFeatures"`
	NetworkNeeds     []NetworkNeed `json:"networkNeeds,omitempty"`
	Readiness        []Readiness   `json:"readiness,omitempty"`
	steps            dockerfileSteps
}

// Readiness severities, fixers and confidences (ADR 0050).
const (
	WillFail = "will-fail"
	MayFail  = "may-fail"
	Advice   = "advice"

	Developer     = "developer"
	Administrator = "administrator"

	Certain  = "certain"
	Likely   = "likely"
	Possible = "possible"
)

// NetworkNeed is a destination a build step is predicted to reach, found statically: an egress preset or exact
// host:port destinations (ADR 0050). Severity is what happens to the build when the destination is unreachable.
type NetworkNeed struct {
	Path         string   `json:"path"`
	Line         int      `json:"line,omitempty"`
	Kind         string   `json:"kind"`
	Cause        string   `json:"cause"`
	Preset       string   `json:"preset,omitempty"`
	Destinations []string `json:"destinations,omitempty"`
	Confidence   string   `json:"confidence"`
	Severity     string   `json:"severity"`
	Fixer        string   `json:"fixer,omitempty"`
	Fix          string   `json:"fix,omitempty"`
	DevOnly      bool     `json:"devOnly,omitempty"`
	trigger      string
	skipEnv      []string
}

// Readiness is one predicted build problem with who can fix it and how (ADR 0050). Blocking marks an item that
// repeats a reason admission already refuses the build for.
type Readiness struct {
	Code        string `json:"code"`
	Severity    string `json:"severity"`
	Fixer       string `json:"fixer"`
	Confidence  string `json:"confidence"`
	Message     string `json:"message"`
	Fix         string `json:"fix"`
	Alternative string `json:"alternative,omitempty"`
	Path        string `json:"path,omitempty"`
	Line        int    `json:"line,omitempty"`
	Blocking    bool   `json:"blocking,omitempty"`
	// Requestable is the egress a project member may ask to open for the project alone (ADR 0052).
	Requestable *EgressEntries `json:"requestable,omitempty"`
}

type EgressEntries struct {
	Presets      []string `json:"presets"`
	Destinations []string `json:"destinations"`
	// Internal lists the destinations that can only be requested as internal: private addresses and non-public names.
	Internal []string `json:"internal,omitempty"`
}

type Result struct {
	Findings    []Finding            `json:"findings"`
	Dockerfiles []DockerfileAnalysis `json:"dockerfiles"`
	Warnings    []Warning            `json:"warnings"`
}

// InputFile is a snapshot path; Content is present only for files the controller chose to send.
type InputFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Content []byte `json:"content,omitempty"`
}

type Input struct {
	Files             []InputFile `json:"files"`
	ApprovedFrontends []string    `json:"approvedFrontends,omitempty"`
	// Dockerfile, when set, is the only file analysed as a Dockerfile, whatever its name; Context is the build
	// context directory its predictions use instead of the Dockerfile's own directory.
	Dockerfile string `json:"dockerfile,omitempty"`
	Context    string `json:"context,omitempty"`
}
