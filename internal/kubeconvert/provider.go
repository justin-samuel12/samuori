package kubeconvert

import (
	"slices"

	"samuori.invalid/samuori/internal/providers"
)

// PlanInput is what a provider plans one run from. ComposeFile is the Compose file's path below the input root;
// Questions are those of the last run that asked any, against which Answers were validated.
type PlanInput struct {
	Name        string
	Input       Input
	Options     Options
	Answers     map[string]string
	Questions   []Question
	ComposeFile string
}

// ToolPlan is the argv after the tool path, the output directories to create under the sandbox's /out, and the one
// the tool writes its artifacts to. Paths in Args are sandbox paths (convertjob.InputRoot, convertjob.OutputRoot,
// convertjob.FilesRoot). Files are configuration files for the tool; Keep, when set, limits what leaves the step.
type ToolPlan struct {
	Args      []string
	Mkdirs    []string
	OutputDir string
	Files     map[string]string `json:",omitempty"`
	Keep      []string          `json:",omitempty"`
	Notes     []Finding         `json:",omitempty"`
}

// ToolOutput is the untrusted result a worker reported. Files are keyed by path below OutputDir.
type ToolOutput struct {
	ExitCode       int
	TimedOut       bool
	Stdout, Stderr string
	Files          map[string][]byte
}

// Interpretation is a provider's reading of its output: bundle files, its own warnings as findings and, for
// providers that ask, the questions it answered with defaults.
type Interpretation struct {
	Files     []File
	Findings  []Finding
	Questions []Question
}

// Provider adapts one conversion tool. Providers never run anything themselves: the worker runs the planned argv in
// the conversion sandbox, and Samuori post-processes the interpretation.
type Provider interface {
	Name() string
	Formats() []string
	Inputs() []string
	Plan(in PlanInput) (ToolPlan, error)
	Interpret(in PlanInput, plan ToolPlan, out ToolOutput) (Interpretation, error)
}

// Asker is a provider that returns questions; it validates a user's answers against the questions of the last run.
type Asker interface {
	CheckAnswers(questions []Question, answers map[string]string) error
}

// ComposeChecker refuses, before a run is queued, Compose files the provider cannot read, and notes settings it may
// not carry over.
type ComposeChecker interface {
	CheckCompose(content []byte) ([]Finding, error)
}

var registry = map[string]Provider{"kompose": Kompose{}, "move2kube": Move2Kube{}}

// Lookup returns the provider for a conversion entry of the provider catalog.
func Lookup(name string) (Provider, bool) {
	d, ok := providers.Lookup(name)
	if !ok || d.Kind != providers.Conversion {
		return nil, false
	}
	p, ok := registry[name]
	return p, ok
}

func supports(p Provider, kind string) bool { return slices.Contains(p.Inputs(), kind) }
