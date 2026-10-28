// Package render turns a recipe build plan into a Dockerfile. Built-in and custom recipes share one declarative
// renderer (internal/recipedef, ADR 0047). Output is deterministic, and every command is written in JSON exec form
// so command text can never add Dockerfile instructions (SEC-SPC-01).
package render

import (
	"errors"
	"fmt"
	"regexp"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/recipedef"
	"samuori.invalid/samuori/internal/recipes/catalog"
)

var (
	pinned      = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
	argName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	errUnpinned = errors.New("render: base images must be pinned by digest")
)

// ErrNewerPlan means the plan needs a recipe or recipe feature this release cannot render: controller and worker
// releases differ, which is a platform failure, not the developer's.
var ErrNewerPlan = errors.New("render: the plan needs a newer worker release")

// Skew reports whether a render failure of raw comes from version skew rather than from the plan's content.
func Skew(raw []byte, err error) bool {
	return errors.Is(err, ErrNewerPlan) || errors.Is(err, recipedef.ErrUnknownStep) || buildplan.Unknown(raw)
}

func argv(c buildspec.Command) []string {
	if c.IsZero() {
		return nil
	}
	if len(c.Argv) > 0 {
		return c.Argv
	}
	return []string{"/bin/sh", "-c", c.Shell}
}

// Dockerfile renders the recipe plan. Build arguments are declared here and supplied by the engine.
func Dockerfile(p buildplan.Plan) ([]byte, error) {
	r := p.Recipe
	if p.Method != "recipe" || r == nil {
		return nil, errors.New("render: not a recipe plan")
	}
	if r.Custom != nil {
		mounts := map[string]recipedef.Mount{}
		for _, s := range p.Secrets {
			if s.BindingID != "" {
				mounts[s.ID] = recipedef.Mount{ID: s.ID, Target: s.Target, Required: s.Required}
			}
		}
		args := make([]string, 0, len(p.BuildArgs))
		for k := range p.BuildArgs {
			if _, isParam := r.Parameters[k]; !isParam {
				args = append(args, k)
			}
		}
		return r.Custom.Render(r.Parameters, mounts, args)
	}
	if !pinned.MatchString(r.BuildImage) || !pinned.MatchString(r.RunImage) {
		return nil, errUnpinned
	}
	args := sortedArgs(p.BuildArgs)
	for _, name := range args {
		if !argName.MatchString(name) {
			return nil, fmt.Errorf("render: invalid build argument name %q", name)
		}
	}
	def, ok, known, err := catalog.Definition(r.ID, r.Version)
	switch {
	case err != nil:
		return nil, fmt.Errorf("render: built-in recipes: %w", err)
	case !known:
		return nil, fmt.Errorf("%w: unknown recipe %q", ErrNewerPlan, r.ID)
	case !ok:
		return nil, fmt.Errorf("%w: unknown version %q of recipe %q", ErrNewerPlan, r.Version, r.ID)
	}
	// Built-in recipes mount every build secret of the plan, bound or not; an unbound one is never required.
	mounts := make([]recipedef.Mount, len(p.Secrets))
	for i, s := range p.Secrets {
		mounts[i] = recipedef.Mount{ID: s.ID, Target: s.Target, Required: s.Required}
	}
	b := recipedef.Bound{BuildImage: r.BuildImage, RunImage: r.RunImage,
		Install: argv(r.Install), Build: argv(r.Build), Start: argv(r.Start), Secrets: mounts, AppType: r.AppType, Port: r.Port}
	if sp := r.SystemPackages; sp != nil {
		b.BuildPackages = recipedef.Packages{Manager: sp.BuildManager, Names: sp.Build}
		b.RunPackages = recipedef.Packages{Manager: sp.RuntimeManager, Names: sp.Runtime}
	}
	b.SourcePath = r.Path
	if f := r.Frontend; f != nil {
		b.Frontend = &recipedef.Frontend{Path: f.Path, Image: f.Image, Output: f.Output, Install: argv(f.Install), Build: argv(f.Build),
			APIPrefixes: f.APIPrefixes, BackendPort: f.BackendPort}
	}
	out, err := def.RenderBuiltin(b, args)
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	return out, nil
}

func sortedArgs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
