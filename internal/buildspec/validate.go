// Package buildspec parses and validates BuildSpec documents (docs/architecture/buildspec.md).
package buildspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"samuori.invalid/samuori/api/schemas"
	"samuori.invalid/samuori/internal/secretscan"
)

type Issue struct {
	Rule    string `json:"rule"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

type Result struct {
	Document       *Document `json:"-"`
	SchemaErrors   []Issue   `json:"schemaErrors,omitempty"`
	SemanticErrors []Issue   `json:"semanticErrors,omitempty"`
}

func (r Result) Valid() bool { return len(r.SchemaErrors) == 0 && len(r.SemanticErrors) == 0 }

// ArgRule rejects build arguments by name or value pattern, regardless of confirmedNotSecret.
type ArgRule struct {
	ID           string
	NamePattern  *regexp.Regexp
	ValuePattern *regexp.Regexp
	Reason       string
}

type Options struct {
	SupportedOS map[string]bool
	ArgRules    []ArgRule
}

func DefaultOptions() Options {
	return Options{SupportedOS: map[string]bool{"linux": true}}
}

var (
	compileOnce sync.Once
	compiled    *jsonschema.Schema
	compileErr  error
	printer     = message.NewPrinter(language.English)
)

func schema() (*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemas.BuildSpecV1Alpha1))
		if err != nil {
			compileErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(schemas.BuildSpecV1Alpha1ID, doc); err != nil {
			compileErr = err
			return
		}
		compiled, compileErr = c.Compile(schemas.BuildSpecV1Alpha1ID)
	})
	return compiled, compileErr
}

// Validate accepts YAML or JSON and applies schema then semantic rules.
func Validate(data []byte, opts Options) (Result, error) {
	var generic any
	if err := yaml.Unmarshal(data, &generic); err != nil {
		return Result{SchemaErrors: []Issue{{Rule: "parse", Path: "/", Message: err.Error()}}}, nil
	}
	js, err := json.Marshal(generic)
	if err != nil {
		return Result{SchemaErrors: []Issue{{Rule: "parse", Path: "/", Message: err.Error()}}}, nil
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		return Result{}, fmt.Errorf("decode instance: %w", err)
	}
	sch, err := schema()
	if err != nil {
		return Result{}, fmt.Errorf("compile schema: %w", err)
	}
	if err := sch.Validate(instance); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return Result{}, err
		}
		return Result{SchemaErrors: leafIssues(ve)}, nil
	}
	var doc Document
	if err := json.Unmarshal(js, &doc); err != nil {
		return Result{}, fmt.Errorf("decode document: %w", err)
	}
	issues := append(semantic(&doc, opts), secretLiterals(generic, "")...)
	return Result{Document: &doc, SemanticErrors: issues}, nil
}

// secretLiterals reports string values that look like credentials (TM-81). Paths are reported, values never are.
func secretLiterals(v any, path string) []Issue {
	var out []Issue
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			out = append(out, secretLiterals(child, path+"/"+strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"))...)
		}
	case []any:
		for i, child := range x {
			out = append(out, secretLiterals(child, fmt.Sprintf("%s/%d", path, i))...)
		}
	case string:
		for _, r := range secretscan.Match([]byte(x)) {
			out = append(out, Issue{Rule: "secret-literal", Path: path, Message: r.Message})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func leafIssues(ve *jsonschema.ValidationError) []Issue {
	if len(ve.Causes) == 0 {
		return []Issue{{
			Rule:    "schema",
			Path:    "/" + strings.Join(ve.InstanceLocation, "/"),
			Message: ve.ErrorKind.LocalizedString(printer),
		}}
	}
	var out []Issue
	for _, c := range ve.Causes {
		out = append(out, leafIssues(c)...)
	}
	return out
}

func semantic(d *Document, opts Options) []Issue {
	var issues []Issue
	add := func(rule, path, format string, args ...any) {
		issues = append(issues, Issue{Rule: rule, Path: path, Message: fmt.Sprintf(format, args...)})
	}

	seenSecrets := map[string]int{}
	for i, s := range d.Spec.Secrets {
		if first, dup := seenSecrets[s.ID]; dup {
			add("duplicate-secret-id", fmt.Sprintf("/spec/secrets/%d/id", i), "secret %q is already declared at /spec/secrets/%d", s.ID, first)
			continue
		}
		seenSecrets[s.ID] = i
	}

	seenArgs := map[string]int{}
	for i, a := range d.Spec.Build.Args {
		path := fmt.Sprintf("/spec/build/args/%d", i)
		if first, dup := seenArgs[a.Name]; dup {
			add("duplicate-build-arg", path+"/name", "build argument %q is already declared at /spec/build/args/%d", a.Name, first)
		} else {
			seenArgs[a.Name] = i
		}
		for _, r := range opts.ArgRules {
			if (r.NamePattern != nil && r.NamePattern.MatchString(a.Name)) || (r.ValuePattern != nil && r.ValuePattern.MatchString(a.Value)) {
				add("arg-policy:"+r.ID, path, "build argument %q is not allowed: %s", a.Name, r.Reason)
			}
		}
	}

	if d.Spec.Build.Method == "railpack" {
		if len(d.Spec.Build.Args) > 0 {
			add("railpack-build-args", "/spec/build/args", "the Railpack frontend ignores build arguments; set commands under spec.build.railpack or in the project's railpack.json")
		}
		if d.Spec.Target != nil && len(d.Spec.Target.Platforms) > 1 {
			add("railpack-one-platform", "/spec/target/platforms", "a Railpack build produces one platform; create one BuildSpec per platform")
		}
		envs := map[string]string{}
		for i, s := range d.Spec.Secrets {
			if s.Usage != "" && s.Usage != "build" {
				continue
			}
			env := RailpackSecretEnv(s.ID)
			if other, clash := envs[env]; clash {
				add("railpack-secret-env-clash", fmt.Sprintf("/spec/secrets/%d/id", i), "secrets %q and %q both reach Railpack build steps as %s; rename one", other, s.ID, env)
			}
			envs[env] = s.ID
			if slices.Contains(RailpackProxyEnv, env) {
				add("railpack-secret-env-reserved", fmt.Sprintf("/spec/secrets/%d/id", i), "secret %q would reach Railpack build steps as %s, which carries the build's egress proxy; rename it", s.ID, env)
			}
			if s.Mount != nil {
				add("railpack-secret-mount", fmt.Sprintf("/spec/secrets/%d/mount", i), "Railpack build steps read secret %q from the environment variable %s, not from a file", s.ID, env)
			}
		}
	}

	if d.Spec.Build.Method == "buildpacks" {
		issues = append(issues, buildpacksRules(d)...)
	}
	issues = append(issues, frontendRules(d)...)
	issues = append(issues, imageMetadataRules(d)...)
	issues = append(issues, imageTagRules(d)...)

	if d.Spec.Target != nil {
		for i, p := range d.Spec.Target.Platforms {
			os, _, _ := strings.Cut(p, "/")
			if !opts.SupportedOS[os] {
				add("unsupported-platform", fmt.Sprintf("/spec/target/platforms/%d", i), "%s targets are not supported yet; supported: %s", os, keys(opts.SupportedOS))
			}
		}
	}
	return issues
}

func keys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k, ok := range m {
		if ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func buildpacksRules(d *Document) []Issue {
	var issues []Issue
	add := func(rule, path, format string, args ...any) {
		issues = append(issues, Issue{Rule: rule, Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if len(d.Spec.Build.Args) > 0 {
		add("buildpacks-build-args", "/spec/build/args", "buildpacks read configuration from environment variables; set them under spec.build.buildpacks.env")
	}
	if d.Spec.Target != nil && len(d.Spec.Target.Platforms) > 1 {
		add("buildpacks-one-platform", "/spec/target/platforms", "a buildpacks build produces one platform; create one BuildSpec per platform")
	}
	build := map[string]bool{}
	for i, s := range d.Spec.Secrets {
		if s.Usage != "" && s.Usage != "build" {
			continue
		}
		build[s.ID] = true
		if s.Mount != nil {
			add("buildpacks-secret-mount", fmt.Sprintf("/spec/secrets/%d/mount", i), "buildpacks read secret %q from a service binding, not from a mount path; bind it under spec.build.buildpacks.bindings", s.ID)
		}
	}
	var bindings []BuildpacksBinding
	if bp := d.Spec.Build.Buildpacks; bp != nil {
		bindings = bp.Bindings
	}
	used := map[string]bool{}
	files := map[string]int{}
	types := map[string]string{}
	for i, b := range bindings {
		path := fmt.Sprintf("/spec/build/buildpacks/bindings/%d", i)
		if !build[b.Secret] {
			add("buildpacks-binding-secret-unknown", path+"/secret", "binding %q uses secret %q, which spec.secrets does not declare as a build secret", b.Name, b.Secret)
		}
		used[b.Secret] = true
		if t, ok := types[b.Name]; ok && t != b.Type {
			add("buildpacks-binding-type-clash", path+"/type", "binding %q is declared with types %q and %q", b.Name, t, b.Type)
		}
		types[b.Name] = b.Type
		key := b.BindingKey()
		if key == "type" || key == "provider" {
			add("buildpacks-binding-key-reserved", path+"/key", "the binding file %q holds the binding's metadata; choose another key", key)
		}
		if first, dup := files[b.Name+"/"+key]; dup {
			add("buildpacks-binding-duplicate", path+"/key", "binding %q already has the file %q at /spec/build/buildpacks/bindings/%d", b.Name, key, first)
		} else {
			files[b.Name+"/"+key] = i
		}
	}
	for i, s := range d.Spec.Secrets {
		if build[s.ID] && !used[s.ID] {
			add("buildpacks-secret-unbound", fmt.Sprintf("/spec/secrets/%d/id", i), "build secret %q reaches buildpacks only through a binding; add it to spec.build.buildpacks.bindings", s.ID)
		}
	}
	return issues
}

func frontendRules(d *Document) []Issue {
	r := d.Spec.Build.Recipe
	if d.Spec.Build.Method != "recipe" || r == nil || r.Frontend == nil {
		return nil
	}
	var issues []Issue
	add := func(rule, path, msg string) { issues = append(issues, Issue{Rule: rule, Path: path, Message: msg}) }
	f := r.Frontend
	app := d.Spec.Application
	switch {
	case app == nil || app.Type != "http-service":
		add("frontend-http-service", "/spec/application/type", "a frontend is served with an http-service application")
	case app.Port == 0:
		add("frontend-port", "/spec/application/port", "set the port the image listens on; the web launcher serves the frontend there")
	case app.Port == f.EffectiveBackendPort():
		add("frontend-port-clash", "/spec/build/recipe/frontend/backendPort", "the application must listen on a different port from the image's port")
	}
	if r.Start.IsZero() {
		add("frontend-start", "/spec/build/recipe/start", "the web launcher starts the application with its start command; set one")
	}
	if d.Spec.Target != nil && len(d.Spec.Target.Platforms) > 1 {
		add("frontend-one-platform", "/spec/target/platforms", "an image with a frontend is built for one platform; create one BuildSpec per platform")
	}
	if r.Path != "" && f.Path == r.Path {
		add("frontend-same-folder", "/spec/build/recipe/frontend/path", "the frontend and the application need separate folders")
	}
	return issues
}
