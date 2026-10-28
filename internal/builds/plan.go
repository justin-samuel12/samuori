package builds

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/secrets"
	"samuori.invalid/samuori/internal/sources"
)

type (
	Plan           = buildplan.Plan
	PlanRecipe     = buildplan.PlanRecipe
	PlanDockerfile = buildplan.PlanDockerfile
	PlanSecret     = buildplan.PlanSecret
	PlanLimits     = buildplan.PlanLimits
	PlanCache      = buildplan.PlanCache
)

var DefaultLimits = buildplan.DefaultLimits

func resolvePlan(ctx context.Context, doc *buildspec.Document, specDigest string, snap sources.Snapshot, manifest sources.Manifest, rec *recipes.Statuses, orgID string, eff policy.Effective, bound map[string]secrets.Binding) (Plan, error) {
	p := Plan{Version: 1, SnapshotDigest: snap.Digest, SpecDigest: specDigest, Method: doc.Spec.Build.Method, Platforms: []string{"linux/amd64"},
		Secrets: []PlanSecret{}, IsolationProfile: string(isolation.VMPerJob), Limits: DefaultLimits, Policies: map[string]string{}}
	if doc.Spec.Target != nil && len(doc.Spec.Target.Platforms) > 0 {
		p.Platforms = doc.Spec.Target.Platforms
	}
	for kind, v := range eff.Versions {
		p.Policies[kind] = v
	}
	for _, a := range doc.Spec.Build.Args {
		if p.BuildArgs == nil {
			p.BuildArgs = map[string]string{}
		}
		p.BuildArgs[a.Name] = a.Value
	}
	for _, s := range doc.Spec.Secrets {
		if s.Usage == "" || s.Usage == "build" {
			ps := PlanSecret{ID: s.ID, Required: s.IsRequired(), Target: s.MountTarget()}
			if b, ok := bound[s.ID]; ok && b.Allows("build") {
				ps.BindingID, ps.BindingVersion = b.ID, b.Version
			} else if ps.Required {
				return Plan{}, fmt.Errorf("secret %q is not bound for this project", s.ID)
			}
			p.Secrets = append(p.Secrets, ps)
		}
	}
	presets := eff.Egress.DockerfilePresets
	switch doc.Spec.Build.Method {
	case "recipe":
		ref := doc.Spec.Build.Recipe
		r, _, err := rec.Get(ctx, orgID, ref.ID, ref.Version)
		if err != nil {
			return Plan{}, err
		}
		if r.Custom != nil {
			if !ref.SystemPackages.IsZero() {
				return Plan{}, errors.New("custom recipes install their own system packages; remove spec.build.recipe.systemPackages")
			}
			if ref.Frontend != nil || ref.Path != "" {
				return Plan{}, errors.New("a frontend and an application folder need a built-in recipe; remove spec.build.recipe.frontend and path")
			}
			if err := customPlan(&p, doc, r); err != nil {
				return Plan{}, err
			}
			presets = r.EgressPresets
			break
		}
		constraint, kind := "", ""
		if app := doc.Spec.Application; app != nil {
			kind = app.Type
			if app.Runtime != nil {
				constraint = app.Runtime.VersionConstraint
			}
		}
		rt, err := r.ResolveRuntime(constraint)
		if err != nil {
			return Plan{}, err
		}
		presets = r.EgressPresets
		images := rt.Images(kind)
		p.Recipe = &PlanRecipe{ID: r.ID, Version: r.Version, Runtime: rt.Version, BuildImage: images[0], RunImage: images[len(images)-1],
			Install: ref.Install, Build: ref.Build, Start: ref.Start, Test: ref.Test, AppType: kind}
		if rt.Source == recipes.SourceOrganization {
			p.Recipe.RuntimeID, p.Recipe.RuntimeSource = rt.ID, rt.Source
		}
		if doc.Spec.Application != nil {
			p.Recipe.Port = doc.Spec.Application.Port
		}
		pkgs, distros, problems := rt.SystemPackages(kind, ref.SystemPackages)
		if len(problems) > 0 {
			return Plan{}, errors.New(problems[0].Message)
		}
		p.Recipe.SystemPackages = pkgs
		presets = append(slices.Clone(presets), distros...)
		p.Recipe.Path = ref.Path
		if ref.Frontend != nil {
			fe, nodePresets, err := frontendPlan(ctx, rec, orgID, ref.Frontend)
			if err != nil {
				return Plan{}, err
			}
			p.Recipe.Frontend = fe
			presets = append(presets, nodePresets...)
		}
	case "railpack", "buildpacks":
		// The provider's presets apply instead; see railpackPlan and buildpacksPlan.
		presets = nil
	case "dockerfile":
		df := doc.Spec.Build.Dockerfile
		ctxDir, name := df.Context, df.Path
		if ctxDir == "" {
			ctxDir = "."
		}
		if name == "" {
			name = "Dockerfile"
		}
		full := path.Clean(path.Join(ctxDir, name))
		for _, f := range manifest.Files {
			if f.Path == full {
				p.Dockerfile = &PlanDockerfile{Context: path.Clean(ctxDir), Path: full, Digest: f.Digest, Target: df.Target}
			}
		}
		if p.Dockerfile == nil {
			return Plan{}, errors.New("the Dockerfile is not in the upload")
		}
	}
	p.Validation = validationFor(doc)
	cat, err := egress.Builtin()
	if err != nil {
		return Plan{}, err
	}
	var refused []string
	if p.Egress, refused = cat.Resolve(presets, eff.Egress); len(refused) > 0 {
		return Plan{}, errors.New("the egress policy does not allow " + strings.Join(refused, ", "))
	}
	return p, nil
}

// frontendPlan resolves a frontend's defaults and pins its Node.js image through the built-in Node.js recipe, so
// the organization's Node.js runtimes and egress presets apply to it as to any Node.js build (ADR 0055).
func frontendPlan(ctx context.Context, rec *recipes.Statuses, orgID string, f *buildspec.Frontend) (*buildplan.PlanFrontend, []string, error) {
	node, _, err := rec.Get(ctx, orgID, buildplan.FrontendRecipe, buildplan.FrontendRecipeVersion)
	if err != nil {
		return nil, nil, fmt.Errorf("the frontend needs the Node.js recipe: %w", err)
	}
	rt, err := node.ResolveRuntime(f.Node)
	if err != nil {
		return nil, nil, fmt.Errorf("frontend: %w", err)
	}
	fe := &buildplan.PlanFrontend{Path: f.Path, Image: rt.BuildImage, Node: rt.Version, Install: f.Install, Build: f.Build, Output: f.Output,
		APIPrefixes: f.APIPrefixes, BackendPort: f.EffectiveBackendPort(), Launcher: buildplan.LauncherContract}
	if fe.Install.IsZero() {
		fe.Install = buildspec.Command{Shell: buildspec.DefaultFrontendInstall}
	}
	if fe.Build.IsZero() {
		fe.Build = buildspec.Command{Shell: buildspec.DefaultFrontendBuild}
	}
	if fe.Output == "" {
		fe.Output = buildspec.DefaultFrontendOutput
	}
	if len(fe.APIPrefixes) == 0 {
		fe.APIPrefixes = []string{buildspec.DefaultFrontendAPIPrefix}
	}
	return fe, node.EgressPresets, nil
}

// validationFor picks the runtime test: the spec's profile, or one implied by the application type.
func validationFor(doc *buildspec.Document) buildplan.PlanValidation {
	v := buildplan.PlanValidation{Profile: buildplan.ProfileNone, Path: "/", TimeoutSeconds: 60}
	kind, port := "", 0
	if app := doc.Spec.Application; app != nil {
		kind, port = app.Type, app.Port
		if app.HealthPath != "" {
			v.Path = app.HealthPath
		}
	}
	switch kind {
	case "http-service":
		v.Profile = buildplan.ProfileHTTPStartup
	case "static-site":
		v.Profile = buildplan.ProfileStaticServe
	case "worker", "database":
		v.Profile = buildplan.ProfileProcessStart
	case "cli-task":
		v.Profile = buildplan.ProfileCLISmoke
	}
	if doc.Spec.Build.Recipe != nil && doc.Spec.Build.Recipe.ID == "static-site" && port == 0 {
		port = 8080
	}
	if val := doc.Spec.Validation; val != nil {
		if val.Profile != "" {
			v.Profile = val.Profile
		}
		if val.TimeoutSeconds > 0 {
			v.TimeoutSeconds = min(val.TimeoutSeconds, 600)
		}
	}
	v.Port = port
	if out := doc.Spec.Outputs; out != nil {
		for _, e := range out.ExpectedSensitiveContent {
			v.Expected = append(v.Expected, buildplan.ExpectedOutput{Path: e.Path, Reason: e.Reason})
		}
		v.SBOM = out.SBOM != nil && *out.SBOM
	}
	return v
}

// customPlan resolves a custom recipe's parameters and checks that the spec declares every secret the recipe
// mounts. Parameters become build arguments, which the rendered Dockerfile declares (ADR 0041).
func customPlan(p *Plan, doc *buildspec.Document, r recipes.Recipe) error {
	ref := doc.Spec.Build.Recipe
	params, err := r.Custom.Resolve(ref.Parameters)
	if err != nil {
		return err
	}
	for _, id := range r.Custom.Secrets {
		if !slices.ContainsFunc(p.Secrets, func(s PlanSecret) bool { return s.ID == id }) {
			return fmt.Errorf("recipe %s uses secret %q; add it to the spec's secrets", r.ID, id)
		}
	}
	if p.BuildArgs == nil {
		p.BuildArgs = map[string]string{}
	}
	for k, v := range params {
		if _, clash := p.BuildArgs[k]; clash {
			return fmt.Errorf("build argument %q has the same name as a recipe parameter", k)
		}
		p.BuildArgs[k] = v
	}
	images := r.Custom.Images()
	p.Recipe = &PlanRecipe{ID: r.ID, Version: r.Version, Custom: r.Custom, Parameters: params}
	if len(images) > 0 {
		p.Recipe.BuildImage, p.Recipe.RunImage = images[0], images[len(images)-1]
	}
	if app := doc.Spec.Application; app != nil {
		p.Recipe.AppType, p.Recipe.Port = app.Type, app.Port
	}
	return nil
}
