// Package recipedef is the declarative recipe format: stages and steps that Samuori renders into a Dockerfile itself
// (ADR 0041). Custom and built-in recipes share it and its renderer (ADR 0047). Every rendered value is validated,
// and every command is written in JSON exec form, so neither a recipe nor a developer's value can add Dockerfile
// instructions.
package recipedef

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Definition struct {
	ID               string      `json:"id" yaml:"id"`
	Version          string      `json:"version" yaml:"version"`
	DisplayName      string      `json:"displayName" yaml:"displayName"`
	Description      string      `json:"description,omitempty" yaml:"description,omitempty"`
	Languages        []string    `json:"languages,omitempty" yaml:"languages,omitempty"`
	ApplicationTypes []string    `json:"applicationTypes,omitempty" yaml:"applicationTypes,omitempty"`
	Platforms        []string    `json:"platforms" yaml:"platforms"`
	EgressPresets    []string    `json:"egressPresets,omitempty" yaml:"egressPresets,omitempty"`
	Secrets          []string    `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Parameters       []Parameter `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	// RunsAsRoot is the recorded reason a final image may run as root; without it the final stage must end as a
	// non-root user.
	RunsAsRoot string  `json:"runsAsRoot,omitempty" yaml:"runsAsRoot,omitempty"`
	Stages     []Stage `json:"stages" yaml:"stages"`
}

type Parameter struct {
	Name        string   `json:"name" yaml:"name"`
	Type        string   `json:"type" yaml:"type"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
	Default     string   `json:"default,omitempty" yaml:"default,omitempty"`
	Required    bool     `json:"required,omitempty" yaml:"required,omitempty"`
	Pattern     string   `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Enum        []string `json:"enum,omitempty" yaml:"enum,omitempty"`
}

type Stage struct {
	Name string `json:"name" yaml:"name"`
	// Image is a base image pinned by digest; From names an earlier stage; Runtime ("build" or "run", built-in
	// recipes only) takes the plan's resolved runtime image. Exactly one is set.
	Image   string `json:"image,omitempty" yaml:"image,omitempty"`
	From    string `json:"from,omitempty" yaml:"from,omitempty"`
	Runtime string `json:"runtime,omitempty" yaml:"runtime,omitempty"`
	Steps   []Step `json:"steps" yaml:"steps"`
}

// Step is exactly one of its fields.
type Step struct {
	Workdir       string    `json:"workdir,omitempty" yaml:"workdir,omitempty"`
	CopySource    *Copy     `json:"copySource,omitempty" yaml:"copySource,omitempty"`
	CopyFromStage *CopyFrom `json:"copyFromStage,omitempty" yaml:"copyFromStage,omitempty"`
	Run           *Run      `json:"run,omitempty" yaml:"run,omitempty"`
	Env           *Pair     `json:"env,omitempty" yaml:"env,omitempty"`
	User          string    `json:"user,omitempty" yaml:"user,omitempty"`
	Expose        string    `json:"expose,omitempty" yaml:"expose,omitempty"`
	Label         *Pair     `json:"label,omitempty" yaml:"label,omitempty"`
	Cmd           []string  `json:"cmd,omitempty" yaml:"cmd,omitempty"`
	// Start and ExposeAppPort (built-in recipes only) write the BuildSpec's start command and, for an http-service,
	// its port.
	Start         bool `json:"start,omitempty" yaml:"start,omitempty"`
	ExposeAppPort bool `json:"exposeAppPort,omitempty" yaml:"exposeAppPort,omitempty"`
}

type Copy struct {
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
}

type CopyFrom struct {
	Stage string `json:"stage" yaml:"stage"`
	From  string `json:"from" yaml:"from"`
	To    string `json:"to" yaml:"to"`
	Owner string `json:"owner,omitempty" yaml:"owner,omitempty"`
}

// Run is a command in argv form, a shell script, or (built-in recipes only) the BuildSpec's install or build
// command. Parameters reach a shell script only as build arguments ($name), never as text pasted into it.
type Run struct {
	Argv    []string `json:"argv,omitempty" yaml:"argv,omitempty"`
	Shell   string   `json:"shell,omitempty" yaml:"shell,omitempty"`
	Command string   `json:"command,omitempty" yaml:"command,omitempty"`
	Network string   `json:"network,omitempty" yaml:"network,omitempty"`
	Secrets []string `json:"secrets,omitempty" yaml:"secrets,omitempty"`
}

type Pair struct {
	Name  string `json:"name" yaml:"name"`
	Value string `json:"value" yaml:"value"`
}

const (
	maxStages, maxSteps, maxParams, maxSecrets = 8, 64, 32, 32
	NetworkNone, NetworkEgress                 = "none", "egress"
	CommandInstall, CommandBuild               = "install", "build"
)

var (
	idRE        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	semverRE    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	stageRE     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	paramRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	secretRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	pinnedRE    = regexp.MustCompile(`^[a-z0-9.-]+(?::[0-9]+)?/[a-z0-9._/-]+:[A-Za-z0-9._-]+@sha256:[a-f0-9]{64}$`)
	absPathRE   = regexp.MustCompile(`^/[A-Za-z0-9._/@+-]{0,255}$`)
	ctxPathRE   = regexp.MustCompile(`^[A-Za-z0-9._@+-][A-Za-z0-9._/@+-]{0,255}$`)
	userRE      = regexp.MustCompile(`^([0-9]{1,10}|[a-z_][a-z0-9_-]{0,31})(:([0-9]{1,10}|[a-z_][a-z0-9_-]{0,31}))?$`)
	envNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	labelNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,127}$`)
	safeValueRE = regexp.MustCompile(`^[A-Za-z0-9 ._:/@+,=%-]{0,512}$`)
	refRE       = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]{0,63})\}`)
	secretish   = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[_-]?key|private[_-]?key|credential|access[_-]?key)`)
	platforms   = []string{"linux/amd64", "linux/arm64"}
	proxyArgs   = []string{"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "NO_PROXY", "ALL_PROXY"}

	defaultString = regexp.MustCompile(`^[A-Za-z0-9._:/@+=-]{0,256}$`)
	versionRE     = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}([.-][A-Za-z0-9]+){0,4}$`)

	builtinArgRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	builtinPinnedRE = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
	builtinTargetRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,255}$`)
	builtinEnvRE    = regexp.MustCompile(`^[A-Za-z0-9._:/@+,=%$-]{1,512}$`)
)

// Problem is one validation finding, with where it is.
type Problem struct {
	Path, Message string
}

// Invalid lists every problem a definition or a set of parameter values has.
type Invalid struct{ Problems []Problem }

func (e *Invalid) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Path + ": " + p.Message
	}
	return "the recipe is not valid: " + strings.Join(parts, "; ")
}

// Parse reads a definition from YAML or JSON and validates it. Unknown fields are refused.
func Parse(text []byte) (Definition, error) {
	var d Definition
	dec := yaml.NewDecoder(bytes.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return Definition{}, &Invalid{[]Problem{{"/", "cannot read the recipe: " + err.Error()}}}
	}
	return d, d.Validate()
}

// Validate checks the whole definition, including that rendering it with each parameter's default succeeds.
func (d Definition) Validate() error { return d.validate(false) }

// ValidateBuiltin checks a built-in recipe shipped with the release, including that it renders for a sample plan.
func (d Definition) ValidateBuiltin() error { return d.validate(true) }

func (d Definition) validate(builtin bool) error {
	var ps []Problem
	add := func(path, format string, args ...any) { ps = append(ps, Problem{path, fmt.Sprintf(format, args...)}) }
	if !idRE.MatchString(d.ID) {
		add("/id", "lower-case letters, digits and dashes, starting with a letter or digit")
	}
	if !semverRE.MatchString(d.Version) {
		add("/version", "a version such as 1.0.0")
	}
	if strings.TrimSpace(d.DisplayName) == "" || len(d.DisplayName) > 128 {
		add("/displayName", "required, at most 128 characters")
	}
	if len(d.Platforms) == 0 {
		add("/platforms", "list at least one of %s", strings.Join(platforms, ", "))
	}
	for i, p := range d.Platforms {
		if !slices.Contains(platforms, p) {
			add(fmt.Sprintf("/platforms/%d", i), "%q is not supported; use %s", p, strings.Join(platforms, " or "))
		}
	}
	if len(d.Secrets) > maxSecrets {
		add("/secrets", "at most %d", maxSecrets)
	}
	for i, s := range d.Secrets {
		if !secretRE.MatchString(s) {
			add(fmt.Sprintf("/secrets/%d", i), "%q is not a secret ID", s)
		}
	}
	if len(d.Parameters) > maxParams {
		add("/parameters", "at most %d", maxParams)
	}
	seen := map[string]bool{}
	for i, p := range d.Parameters {
		at := fmt.Sprintf("/parameters/%d", i)
		switch {
		case !paramRE.MatchString(p.Name):
			add(at+"/name", "letters, digits and underscores, starting with a letter or underscore")
		case seen[p.Name]:
			add(at+"/name", "%q is declared twice", p.Name)
		case slices.Contains(proxyArgs, strings.ToUpper(p.Name)):
			add(at+"/name", "%q is reserved for the build proxy", p.Name)
		}
		seen[p.Name] = true
		if !slices.Contains([]string{"string", "version", "port", "path"}, p.Type) {
			add(at+"/type", "string, version, port or path")
		}
		if p.Pattern != "" {
			if _, err := regexp.Compile("^(?:" + p.Pattern + ")$"); err != nil || p.Type != "string" {
				add(at+"/pattern", "a valid regular expression, for string parameters only")
			}
		}
		if p.Default != "" {
			if err := p.check(p.Default); err != nil {
				add(at+"/default", "%s", err.Error())
			}
		} else if !p.Required {
			add(at+"/default", "an optional parameter needs a default")
		}
	}
	if len(d.Stages) == 0 || len(d.Stages) > maxStages {
		add("/stages", "between 1 and %d stages", maxStages)
	}
	names := map[string]bool{}
	for i, st := range d.Stages {
		at := fmt.Sprintf("/stages/%d", i)
		switch {
		case builtin && st.Name == "" && i == len(d.Stages)-1:
		case !stageRE.MatchString(st.Name):
			add(at+"/name", "lower-case letters, digits and dashes, starting with a letter")
		case names[st.Name]:
			add(at+"/name", "%q is used twice", st.Name)
		}
		switch {
		case st.Runtime != "" && !builtin:
			add(at+"/runtime", "only built-in recipes take their images from the plan")
		case builtin && countSet(st.Image != "", st.From != "", st.Runtime != "") != 1:
			add(at, "set exactly one of image, from or runtime")
		case st.Runtime != "" && st.Runtime != "build" && st.Runtime != "run":
			add(at+"/runtime", "build or run")
		case !builtin && (st.Image == "") == (st.From == ""):
			add(at, "set exactly one of image or from")
		case st.Image != "" && !pinnedRE.MatchString(st.Image):
			add(at+"/image", "an image pinned by digest, such as registry/repo:tag@sha256:<64 hex>")
		case st.From != "" && !names[st.From]:
			add(at+"/from", "%q is not an earlier stage", st.From)
		}
		if len(st.Steps) > maxSteps {
			add(at+"/steps", "at most %d steps", maxSteps)
		}
		for j, s := range st.Steps {
			n := countSet(s.Workdir != "", s.CopySource != nil, s.CopyFromStage != nil, s.Run != nil, s.Env != nil, s.User != "", s.Expose != "", s.Label != nil, len(s.Cmd) > 0)
			if planned := countSet(s.Start, s.ExposeAppPort); planned > 0 && !builtin {
				add(fmt.Sprintf("%s/steps/%d", at, j), "start and exposeAppPort are for built-in recipes only")
			} else {
				n += planned
			}
			if n != 1 {
				add(fmt.Sprintf("%s/steps/%d", at, j), "each step is exactly one of workdir, copySource, copyFromStage, run, env, user, expose, label or cmd")
			}
			if s.CopyFromStage != nil && !names[s.CopyFromStage.Stage] {
				add(fmt.Sprintf("%s/steps/%d/copyFromStage/stage", at, j), "%q is not an earlier stage", s.CopyFromStage.Stage)
			}
			if r := s.Run; r != nil {
				switch {
				case r.Command != "" && !builtin:
					add(fmt.Sprintf("%s/steps/%d/run/command", at, j), "only built-in recipes run the BuildSpec's commands")
				case r.Command != "" && r.Command != CommandInstall && r.Command != CommandBuild:
					add(fmt.Sprintf("%s/steps/%d/run/command", at, j), "install or build")
				case r.Command != "" && (len(r.Argv) > 0 || r.Shell != "" || len(r.Secrets) > 0):
					add(fmt.Sprintf("%s/steps/%d/run", at, j), "a command step has no argv, shell or secrets; it mounts every build secret of the plan")
				case r.Command == "" && (len(r.Argv) == 0) == (r.Shell == ""):
					add(fmt.Sprintf("%s/steps/%d/run", at, j), "set exactly one of argv or shell")
				}
				if r.Network != "" && r.Network != NetworkNone && r.Network != NetworkEgress {
					add(fmt.Sprintf("%s/steps/%d/run/network", at, j), "none or egress")
				}
				for _, id := range r.Secrets {
					if !slices.Contains(d.Secrets, id) {
						add(fmt.Sprintf("%s/steps/%d/run/secrets", at, j), "%q is not in the recipe's secrets list", id)
					}
				}
				if refRE.MatchString(r.Shell) {
					add(fmt.Sprintf("%s/steps/%d/run/shell", at, j), "use $name for parameters in a shell script; ${name} would be pasted in as text")
				}
			}
		}
		names[st.Name] = true
	}
	if len(ps) > 0 {
		return &Invalid{ps}
	}
	if builtin {
		if len(d.Parameters) > 0 || len(d.Secrets) > 0 {
			return &Invalid{[]Problem{{"/", "a built-in recipe has no parameters or secrets of its own"}}}
		}
		if _, err := d.RenderBuiltin(sampleBound, []string{"SAMPLE_ARG"}); err != nil {
			return &Invalid{[]Problem{{"/stages", err.Error()}}}
		}
		return nil
	}
	defaults := map[string]string{}
	for _, p := range d.Parameters {
		defaults[p.Name] = p.Default
		if p.Default == "" {
			defaults[p.Name] = p.sample()
		}
	}
	if _, err := d.Render(defaults, nil, nil); err != nil {
		var inv *Invalid
		if errors.As(err, &inv) {
			return err
		}
		return &Invalid{[]Problem{{"/stages", err.Error()}}}
	}
	return nil
}

var sampleBound = Bound{
	BuildImage: "registry.invalid/build:1@sha256:" + strings.Repeat("0", 64),
	RunImage:   "registry.invalid/run:1@sha256:" + strings.Repeat("0", 64),
	Install:    []string{"install"}, Build: []string{"build"}, Start: []string{"start"},
	Secrets: []Mount{{ID: "sample", Target: "/run/secrets/sample", Required: true}},
	AppType: "http-service", Port: 8080,
}

func countSet(set ...bool) int {
	n := 0
	for _, s := range set {
		if s {
			n++
		}
	}
	return n
}

func (p Parameter) sample() string {
	switch p.Type {
	case "port":
		return "8080"
	case "version":
		return "1.0"
	case "path":
		return "app"
	}
	if len(p.Enum) > 0 {
		return p.Enum[0]
	}
	return "value"
}

func (p Parameter) check(v string) error {
	if len(p.Enum) > 0 && !slices.Contains(p.Enum, v) {
		return fmt.Errorf("must be one of %s", strings.Join(p.Enum, ", "))
	}
	switch p.Type {
	case "port":
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 {
			return errors.New("must be a port from 1 to 65535")
		}
	case "version":
		if !versionRE.MatchString(v) {
			return errors.New("must be a version such as 2.1 or 22.04")
		}
	case "path":
		if strings.Contains(v, "..") || !(ctxPathRE.MatchString(v) || absPathRE.MatchString(v)) {
			return errors.New("must be a path without ..")
		}
	default:
		if !defaultString.MatchString(v) {
			return errors.New("letters, digits and . _ : / @ + = - only, at most 256 characters")
		}
		if p.Pattern != "" && !regexp.MustCompile("^(?:"+p.Pattern+")$").MatchString(v) {
			return fmt.Errorf("must match %s", p.Pattern)
		}
	}
	return nil
}

// Resolve checks a developer's values against the parameters and fills in defaults. Unknown names are refused.
func (d Definition) Resolve(values map[string]string) (map[string]string, error) {
	var ps []Problem
	out := map[string]string{}
	for name := range values {
		if !slices.ContainsFunc(d.Parameters, func(p Parameter) bool { return p.Name == name }) {
			ps = append(ps, Problem{"/spec/build/recipe/parameters/" + name, "this recipe has no such parameter"})
		}
	}
	for _, p := range d.Parameters {
		v, ok := values[p.Name]
		if !ok || v == "" {
			v = p.Default
		}
		if v == "" {
			ps = append(ps, Problem{"/spec/build/recipe/parameters/" + p.Name, "required"})
			continue
		}
		if err := p.check(v); err != nil {
			ps = append(ps, Problem{"/spec/build/recipe/parameters/" + p.Name, err.Error()})
			continue
		}
		out[p.Name] = v
	}
	if len(ps) > 0 {
		return nil, &Invalid{ps}
	}
	return out, nil
}

// Images is every base image the recipe pulls.
func (d Definition) Images() []string {
	var out []string
	for _, st := range d.Stages {
		if st.Image != "" {
			out = append(out, st.Image)
		}
	}
	return out
}

// Mount is a build secret as the plan admits it.
type Mount struct {
	ID, Target string
	Required   bool
}

func exec(argv []string) string {
	raw, _ := json.Marshal(argv)
	return string(raw)
}

func isRoot(user string) bool {
	u, _, _ := strings.Cut(user, ":")
	return u == "0" || u == "root"
}

// ErrUnknownStep is a step with no kind this release knows: a validated recipe never has one, so a newer release
// defined it.
var ErrUnknownStep = errors.New("recipedef: step of a kind this release does not know")

// Bound is what a built-in recipe takes from the build plan: the resolved runtime images, the BuildSpec's commands
// in argv form (empty when unset), its build secrets in plan order, and the application type and port.
type Bound struct {
	BuildImage, RunImage  string
	Install, Build, Start []string
	Secrets               []Mount
	AppType               string
	Port                  int
	// BuildPackages and RunPackages are OS packages installed right after FROM in the stages on the build and run
	// images.
	BuildPackages, RunPackages Packages
	// SourcePath is the application's folder in the build context; Frontend adds a frontend stage and makes the
	// web launcher the image's command (ADR 0055).
	SourcePath string
	Frontend   *Frontend
}

// Frontend is a resolved frontend: its folder, pinned Node.js image and commands, the folder its build writes, the
// path prefixes the launcher forwards and the port the application listens on behind it.
type Frontend struct {
	Path, Image, Output string
	Install, Build      []string
	APIPrefixes         []string
	BackendPort         int
}

// LauncherContext is the named build context the worker places the web launcher in, as /samuori-web.
const LauncherContext = "samuoriweb"

const frontendStage = "samuori-frontend"

// Packages are OS package names for one image and its package manager, apk or apt.
type Packages struct {
	Manager string
	Names   []string
}

func (b *Bound) packagesFor(runtime string) Packages {
	switch {
	case b == nil:
		return Packages{}
	case runtime == "build":
		return b.BuildPackages
	case runtime == "run":
		return b.RunPackages
	}
	return Packages{}
}

var packageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,127}$`)

// installArgv runs as root because some run images default to an unprivileged user; the recipe's own user step
// follows.
func (p Packages) installArgv() ([]string, error) {
	for _, n := range p.Names {
		if !packageNameRE.MatchString(n) {
			return nil, fmt.Errorf("invalid system package name %q", n)
		}
	}
	names := strings.Join(p.Names, " ")
	switch p.Manager {
	case "apk":
		return []string{"/bin/sh", "-c", "apk add --no-cache " + names}, nil
	case "apt":
		return []string{"/bin/sh", "-c", "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends " + names + " && rm -rf /var/lib/apt/lists/*"}, nil
	}
	return nil, fmt.Errorf("unknown package manager %q", p.Manager)
}

// Render writes the Dockerfile for resolved parameter values. Parameters and the build arguments named in args are
// declared as ARG in every stage, so shell steps read them as $name. mounts are the admitted build secrets by ID;
// a step's secret with no admitted mount is left out, as an unbound optional secret is.
func (d Definition) Render(params map[string]string, mounts map[string]Mount, args []string) ([]byte, error) {
	return d.render(params, mounts, args, nil)
}

// RenderBuiltin writes the Dockerfile of a built-in recipe for a plan, in the built-in recipes' established form
// (docs/architecture/build-execution.md#recipe-rendering).
func (d Definition) RenderBuiltin(b Bound, args []string) ([]byte, error) {
	return d.render(nil, nil, args, &b)
}

func (d Definition) render(params map[string]string, mounts map[string]Mount, args []string, bound *Bound) ([]byte, error) {
	builtin := bound != nil
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	sub := func(v string) string {
		return refRE.ReplaceAllStringFunc(v, func(m string) string { return params[refRE.FindStringSubmatch(m)[1]] })
	}
	var ps []Problem
	bad := func(path, format string, a ...any) { ps = append(ps, Problem{path, fmt.Sprintf(format, a...)}) }
	stages, _ := json.Marshal(d.Stages)
	for _, m := range refRE.FindAllStringSubmatch(string(stages), -1) {
		if _, ok := params[m[1]]; !ok {
			bad("/stages", "${%s} is not a parameter", m[1])
		}
	}
	argNames := make([]string, 0, len(params)+len(args))
	for name := range params {
		argNames = append(argNames, name)
	}
	for _, a := range args {
		if builtin && !builtinArgRE.MatchString(a) || !builtin && !envNameRE.MatchString(a) {
			bad("/spec/build/args", "%q is not a build argument name", a)
		}
		if !slices.Contains(argNames, a) {
			argNames = append(argNames, a)
		}
	}
	slices.Sort(argNames)
	finalUser := ""
	if builtin {
		line("# Generated by Samuori from recipe %s %s. Do not edit; change the BuildSpec instead.", d.ID, d.Version)
	} else {
		line("# Generated by Samuori from custom recipe %s %s. Do not edit; change the recipe instead.", d.ID, d.Version)
	}
	if builtin && bound.Frontend != nil {
		if err := frontendStageLines(bound.Frontend, line); err != nil {
			return nil, err
		}
	}
	for i, st := range d.Stages {
		at := fmt.Sprintf("/stages/%d", i)
		base := st.Image
		if base == "" {
			base = st.From
		}
		if st.Runtime != "" {
			if !builtin {
				return nil, &Invalid{[]Problem{{at + "/runtime", "only built-in recipes take their images from the plan"}}}
			}
			base = bound.BuildImage
			if st.Runtime == "run" {
				base = bound.RunImage
			}
			if !builtinPinnedRE.MatchString(base) {
				bad(at+"/runtime", "the plan's runtime image is not pinned by digest")
			}
		}
		if st.Name == "" {
			line("FROM %s", base)
		} else {
			line("FROM %s AS %s", base, st.Name)
		}
		if !builtin || i == 0 {
			for _, a := range argNames {
				line("ARG %s", a)
			}
		}
		if pkgs := bound.packagesFor(st.Runtime); len(pkgs.Names) > 0 {
			argv, err := pkgs.installArgv()
			if err != nil {
				return nil, err
			}
			line("USER 0")
			line("RUN %s", exec(argv))
		}
		finalUser = ""
		for j, s := range st.Steps {
			sp := fmt.Sprintf("%s/steps/%d", at, j)
			switch {
			case s.Workdir != "":
				p := sub(s.Workdir)
				if !absPathRE.MatchString(p) || strings.Contains(p, "..") {
					bad(sp+"/workdir", "an absolute path without ..")
				}
				line("WORKDIR %s", p)
			case s.CopySource != nil:
				from, to := sub(s.CopySource.From), sub(s.CopySource.To)
				if !ctxPathRE.MatchString(from) || strings.Contains(from, "..") {
					bad(sp+"/copySource/from", "a path inside the upload, without ..")
				}
				if !(absPathRE.MatchString(to) || ctxPathRE.MatchString(to)) || strings.Contains(to, "..") {
					bad(sp+"/copySource/to", "a path without ..")
				}
				if builtin {
					if bound.SourcePath != "" {
						from = path.Join(bound.SourcePath, from)
					}
					line("COPY %s %s", from, to)
				} else {
					line("COPY %s", exec([]string{from, to}))
				}
			case s.CopyFromStage != nil:
				c := s.CopyFromStage
				from, to, owner := sub(c.From), sub(c.To), sub(c.Owner)
				if !absPathRE.MatchString(from) || strings.Contains(from, "..") {
					bad(sp+"/copyFromStage/from", "an absolute path without ..")
				}
				if !(absPathRE.MatchString(to) || ctxPathRE.MatchString(to)) || strings.Contains(to, "..") {
					bad(sp+"/copyFromStage/to", "a path without ..")
				}
				chown := ""
				if owner != "" {
					if !userRE.MatchString(owner) {
						bad(sp+"/copyFromStage/owner", "a user or user:group")
					}
					chown = " --chown=" + owner
				}
				if builtin {
					line("COPY --from=%s%s %s %s", c.Stage, chown, from, to)
				} else {
					line("COPY --from=%s%s %s", c.Stage, chown, exec([]string{from, to}))
				}
			case s.Run != nil && s.Run.Command != "":
				if !builtin {
					return nil, &Invalid{[]Problem{{sp + "/run/command", "only built-in recipes run the BuildSpec's commands"}}}
				}
				argv := bound.Install
				if s.Run.Command == CommandBuild {
					argv = bound.Build
				}
				if len(argv) == 0 {
					continue
				}
				var opts []string
				if s.Run.Network != NetworkEgress {
					opts = append(opts, "--network=none")
				}
				for _, m := range bound.Secrets {
					if !secretRE.MatchString(m.ID) || !builtinTargetRE.MatchString(m.Target) || strings.Contains(m.Target, "..") {
						return nil, fmt.Errorf("invalid secret %q", m.ID)
					}
					opts = append(opts, fmt.Sprintf("--mount=type=secret,id=%s,target=%s,required=%t", m.ID, m.Target, m.Required))
				}
				if len(opts) > 0 {
					line("RUN %s %s", strings.Join(opts, " "), exec(argv))
				} else {
					line("RUN %s", exec(argv))
				}
			case s.Run != nil:
				r := s.Run
				var opts []string
				if r.Network != NetworkEgress {
					opts = append(opts, "--network=none")
				}
				for _, id := range r.Secrets {
					m, ok := mounts[id]
					if !ok {
						continue
					}
					if !secretRE.MatchString(m.ID) || !absPathRE.MatchString(m.Target) || strings.Contains(m.Target, "..") {
						bad(sp+"/run/secrets", "invalid secret mount %q", id)
						continue
					}
					opts = append(opts, fmt.Sprintf("--mount=type=secret,id=%s,target=%s,required=%t", m.ID, m.Target, m.Required))
				}
				argv := []string{"/bin/sh", "-c", r.Shell}
				if len(r.Argv) > 0 {
					argv = make([]string, len(r.Argv))
					for k, a := range r.Argv {
						argv[k] = sub(a)
					}
				}
				prefix := ""
				if len(opts) > 0 {
					prefix = strings.Join(opts, " ") + " "
				}
				line("RUN %s%s", prefix, exec(argv))
			case s.Env != nil:
				v := sub(s.Env.Value)
				switch {
				case !envNameRE.MatchString(s.Env.Name):
					bad(sp+"/env/name", "letters, digits and underscores")
				case secretish.MatchString(s.Env.Name):
					bad(sp+"/env/name", "%q looks like a secret; use a secret instead of an environment value", s.Env.Name)
				case builtin && !builtinEnvRE.MatchString(v):
					bad(sp+"/env/value", "letters, digits and . _ : / @ + , = %% $ - only")
				case !builtin && !safeValueRE.MatchString(v):
					bad(sp+"/env/value", "letters, digits, spaces and . _ : / @ + , = %% - only")
				}
				if builtin {
					line("ENV %s=%s", s.Env.Name, v)
				} else {
					line("ENV %s=%q", s.Env.Name, v)
				}
			case s.User != "":
				u := sub(s.User)
				if !userRE.MatchString(u) {
					bad(sp+"/user", "a user or user:group")
				}
				finalUser = u
				line("USER %s", u)
			case s.Expose != "":
				port := sub(s.Expose)
				if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
					bad(sp+"/expose", "a port from 1 to 65535")
				}
				line("EXPOSE %s", port)
			case s.Label != nil:
				v := sub(s.Label.Value)
				if !labelNameRE.MatchString(s.Label.Name) || !safeValueRE.MatchString(v) {
					bad(sp+"/label", "a label name such as org.opencontainers.image.title and a plain value")
				}
				line("LABEL %q=%q", s.Label.Name, v)
			case len(s.Cmd) > 0:
				argv := make([]string, len(s.Cmd))
				for k, a := range s.Cmd {
					argv[k] = sub(a)
				}
				line("CMD %s", exec(argv))
			case (s.Start || s.ExposeAppPort) && !builtin:
				return nil, &Invalid{[]Problem{{sp, "start and exposeAppPort are for built-in recipes only"}}}
			case s.Start && bound.Frontend != nil:
				if len(bound.Start) == 0 || bound.Port < 1 || bound.Port > 65535 {
					return nil, errors.New("the web launcher needs the application's start command and the image's port")
				}
				f := bound.Frontend
				line("COPY --from=%s /src/%s /srv/frontend", frontendStage, f.Output)
				line("COPY --from=%s /samuori-web /usr/local/bin/samuori-web", LauncherContext)
				argv := []string{"/usr/local/bin/samuori-web", "--listen", ":" + strconv.Itoa(bound.Port), "--root", "/srv/frontend"}
				for _, prefix := range f.APIPrefixes {
					argv = append(argv, "--proxy", prefix)
				}
				argv = append(argv, "--backend", "http://127.0.0.1:"+strconv.Itoa(f.BackendPort), "--")
				line("CMD %s", exec(append(argv, bound.Start...)))
			case s.Start:
				if len(bound.Start) > 0 {
					line("CMD %s", exec(bound.Start))
				}
			case s.ExposeAppPort:
				if bound.AppType == "http-service" && bound.Port > 0 {
					line("EXPOSE %s", strconv.Itoa(bound.Port))
				}
			default:
				return nil, fmt.Errorf("%w at %s", ErrUnknownStep, sp)
			}
		}
	}
	if (finalUser == "" || isRoot(finalUser)) && strings.TrimSpace(d.RunsAsRoot) == "" {
		bad(fmt.Sprintf("/stages/%d", len(d.Stages)-1), "the final stage must end with a user step for a non-root user, or the recipe must give a runsAsRoot reason")
	}
	if len(ps) > 0 {
		return nil, &Invalid{ps}
	}
	return []byte(b.String()), nil
}

var apiPrefixRE = regexp.MustCompile(`^/[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*$`)

// frontendStageLines writes the stage that builds the frontend. Its install step reaches the build's egress
// allowlist; its build step runs without network, like a recipe's build command.
func frontendStageLines(f *Frontend, line func(string, ...any)) error {
	switch {
	case !builtinPinnedRE.MatchString(f.Image):
		return errors.New("the frontend's Node.js image is not pinned by digest")
	case !ctxPathRE.MatchString(f.Path) || strings.Contains(f.Path, ".."), !ctxPathRE.MatchString(f.Output) || strings.Contains(f.Output, ".."):
		return errors.New("the frontend's folders must be paths inside the upload, without ..")
	case f.BackendPort < 1 || f.BackendPort > 65535:
		return errors.New("the application's port behind the launcher must be from 1 to 65535")
	case len(f.APIPrefixes) == 0:
		return errors.New("the launcher needs at least one API path")
	}
	for _, p := range f.APIPrefixes {
		if !apiPrefixRE.MatchString(p) {
			return fmt.Errorf("invalid API path %q", p)
		}
	}
	line("FROM %s AS %s", f.Image, frontendStage)
	line("WORKDIR /src")
	line("COPY %s .", f.Path)
	if len(f.Install) > 0 {
		line("RUN %s", exec(f.Install))
	}
	if len(f.Build) > 0 {
		line("RUN --network=none %s", exec(f.Build))
	}
	return nil
}
