package buildspec

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

const (
	APIVersion = "samuori.invalid/v1alpha1"
	Kind       = "ImageBuild"
)

type Document struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

type Metadata struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type Spec struct {
	Application *Application   `json:"application,omitempty"`
	Build       Build          `json:"build"`
	Secrets     []Secret       `json:"secrets,omitempty"`
	Target      *Target        `json:"target,omitempty"`
	Validation  *Validation    `json:"validation,omitempty"`
	Outputs     *Outputs       `json:"outputs,omitempty"`
	Publication *Publication   `json:"publication,omitempty"`
	Extensions  map[string]any `json:"-"`
}

type Application struct {
	Type       string   `json:"type,omitempty"`
	Runtime    *Runtime `json:"runtime,omitempty"`
	Port       int      `json:"port,omitempty"`
	HealthPath string   `json:"healthPath,omitempty"`
}

type Runtime struct {
	Language          string `json:"language"`
	Framework         string `json:"framework,omitempty"`
	VersionConstraint string `json:"versionConstraint,omitempty"`
}

type Build struct {
	Method     string      `json:"method"`
	Recipe     *Recipe     `json:"recipe,omitempty"`
	Dockerfile *Dockerfile `json:"dockerfile,omitempty"`
	Railpack   *Railpack   `json:"railpack,omitempty"`
	Buildpacks *Buildpacks `json:"buildpacks,omitempty"`
	Args       []BuildArg  `json:"args,omitempty"`
	Engine     *Engine     `json:"engine,omitempty"`
}

type Recipe struct {
	ID      string  `json:"id"`
	Version string  `json:"version"`
	Install Command `json:"install,omitzero"`
	Build   Command `json:"build,omitzero"`
	Start   Command `json:"start,omitzero"`
	Test    Command `json:"test,omitzero"`
	// Parameters are a custom recipe's values (ADR 0041).
	Parameters     map[string]string `json:"parameters,omitempty"`
	SystemPackages *SystemPackages   `json:"systemPackages,omitempty"`
	// Path is the application's folder in the upload; the upload's root when empty.
	Path     string    `json:"path,omitempty"`
	Frontend *Frontend `json:"frontend,omitempty"`
}

// Frontend is a browser application built with Node.js and served with the recipe's application from one image by
// Samuori's web launcher (ADR 0055). Empty fields take the defaults below.
type Frontend struct {
	Path        string   `json:"path"`
	Install     Command  `json:"install,omitzero"`
	Build       Command  `json:"build,omitzero"`
	Output      string   `json:"output,omitempty"`
	Node        string   `json:"node,omitempty"`
	APIPrefixes []string `json:"apiPrefixes,omitempty"`
	BackendPort int      `json:"backendPort,omitempty"`
}

// EffectiveBackendPort is the port the application listens on behind the launcher.
func (f *Frontend) EffectiveBackendPort() int {
	if f.BackendPort > 0 {
		return f.BackendPort
	}
	return DefaultFrontendBackendPort
}

const (
	DefaultFrontendInstall     = "npm ci"
	DefaultFrontendBuild       = "npm run build"
	DefaultFrontendOutput      = "dist"
	DefaultFrontendAPIPrefix   = "/api"
	DefaultFrontendBackendPort = 8000
)

// SystemPackages are OS packages a built-in recipe installs in its build stage and final image.
type SystemPackages struct {
	Build   []string `json:"build,omitempty"`
	Runtime []string `json:"runtime,omitempty"`
}

func (s *SystemPackages) IsZero() bool { return s == nil || len(s.Build) == 0 && len(s.Runtime) == 0 }

// Command is either a shell string or an argument vector; exactly one is set.
type Command struct {
	Shell string
	Argv  []string
}

func (c Command) IsZero() bool { return c.Shell == "" && len(c.Argv) == 0 }

func (c Command) MarshalJSON() ([]byte, error) {
	if len(c.Argv) > 0 {
		return json.Marshal(c.Argv)
	}
	return json.Marshal(c.Shell)
}

func (c *Command) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '[' {
		return json.Unmarshal(b, &c.Argv)
	}
	return json.Unmarshal(b, &c.Shell)
}

type Dockerfile struct {
	Context       string                  `json:"context,omitempty"`
	Path          string                  `json:"path,omitempty"`
	Target        string                  `json:"target,omitempty"`
	NamedContexts map[string]NamedContext `json:"namedContexts,omitempty"`
}

// Railpack configures the railpack method (ADR 0047). The commands override what Railpack would choose; they run in
// build steps like recipe commands, never during planning.
type Railpack struct {
	Context      string `json:"context,omitempty"`
	BuildCommand string `json:"buildCommand,omitempty"`
	StartCommand string `json:"startCommand,omitempty"`
}

// RailpackSecretEnv is the environment variable a Railpack build step reads a logical secret from: the ID in upper
// case with '-' and '.' replaced by '_'.
func RailpackSecretEnv(id string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(id))
}

// RailpackProxyEnv are the variables the worker delivers the egress proxy in, so no secret may use them.
var RailpackProxyEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"}

// Buildpacks configures the buildpacks method (ADR 0047). Env holds non-secret buildpack configuration (BP_*, BPE_*,
// BPL_*); Bindings turn logical build secrets into CNB service bindings, materialized for the lifecycle step only.
type Buildpacks struct {
	Context  string              `json:"context,omitempty"`
	Env      map[string]string   `json:"env,omitempty"`
	Bindings []BuildpacksBinding `json:"bindings,omitempty"`
}

// BuildpacksBinding places one logical secret as the file Key (the secret ID when empty) of the binding Name of
// type Type.
type BuildpacksBinding struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Secret string `json:"secret"`
	Key    string `json:"key,omitempty"`
}

// BindingKey is the file name the binding's secret is written to.
func (b BuildpacksBinding) BindingKey() string {
	if b.Key == "" {
		return b.Secret
	}
	return b.Key
}

type NamedContext struct {
	Path  string `json:"path,omitempty"`
	Image string `json:"image,omitempty"`
}

type BuildArg struct {
	Name               string `json:"name"`
	Value              string `json:"value"`
	ConfirmedNotSecret bool   `json:"confirmedNotSecret,omitempty"`
}

type Engine struct {
	Preference string `json:"preference,omitempty"`
}

type Secret struct {
	ID          string       `json:"id"`
	Type        string       `json:"type,omitempty"`
	Required    *bool        `json:"required,omitempty"`
	Usage       string       `json:"usage,omitempty"`
	Description string       `json:"description,omitempty"`
	Mount       *SecretMount `json:"mount,omitempty"`
}

func (s Secret) IsRequired() bool { return s.Required == nil || *s.Required }

func (s Secret) MountTarget() string {
	if s.Mount != nil {
		return s.Mount.Target
	}
	return "/run/secrets/" + s.ID
}

type SecretMount struct {
	Target string `json:"target"`
}

type Target struct {
	Platforms []string `json:"platforms,omitempty"`
}

type Validation struct {
	Profile        string   `json:"profile,omitempty"`
	ExtraChecks    []string `json:"extraChecks,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
}

type Outputs struct {
	Image                    *ImageOutput        `json:"image,omitempty"`
	SBOM                     *bool               `json:"sbom,omitempty"`
	Provenance               *bool               `json:"provenance,omitempty"`
	ExpectedSensitiveContent []SensitiveDeclared `json:"expectedSensitiveContent,omitempty"`
}

type ImageOutput struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// Tags a publication applies when the publisher gives none: OCI tags, or {version} for the build's version
	// (ADR 0054). Absent means ["{version}"].
	Tags []string `json:"tags,omitempty"`
}

type SensitiveDeclared struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type Publication struct {
	Mode           string   `json:"mode,omitempty"`
	DestinationRef string   `json:"destinationRef,omitempty"`
	RequestedTags  []string `json:"requestedTags,omitempty"`
}

type specFields Spec

func (s *Spec) UnmarshalJSON(b []byte) error {
	var fields specFields
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*s = Spec(fields)
	for k, v := range raw {
		if !strings.HasPrefix(k, "x-") {
			continue
		}
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			return err
		}
		if s.Extensions == nil {
			s.Extensions = map[string]any{}
		}
		s.Extensions[k] = val
	}
	return nil
}

// MarshalJSON keeps declared fields in struct order and appends x-* extensions sorted by key.
func (s Spec) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(specFields(s))
	if err != nil || len(s.Extensions) == 0 {
		return b, err
	}
	names := make([]string, 0, len(s.Extensions))
	for k := range s.Extensions {
		names = append(names, k)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	buf.Write(b[:len(b)-1])
	for _, k := range names {
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(s.Extensions[k])
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
