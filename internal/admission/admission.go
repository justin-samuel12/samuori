// Package admission decides, before scheduling, whether a spec revision can build a snapshot. The dry run and
// real admission share this code (docs/architecture/build-execution.md#admission-before-scheduling).
package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/discovery"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/recipedef"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/secrets"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
)

// EnginePlatforms is the BuildKit engine capability matrix. Windows is deferred (OD-7b).
var EnginePlatforms = map[string]bool{"linux/amd64": true, "linux/arm64": true}

const maxDockerfileBytes = 1 << 20

type Decision struct {
	PolicyID      string   `json:"policyId"`
	PolicyVersion int      `json:"policyVersion"`
	Kind          string   `json:"-"`
	Outcome       string   `json:"outcome"`
	Reason        string   `json:"reason,omitempty"`
	Requestable   []string `json:"requestableRepositories,omitempty"`
}

type SecretReadiness struct {
	ID          string `json:"id"`
	Required    bool   `json:"required"`
	Type        string `json:"type"`
	Usage       string `json:"usage"`
	MountTarget string `json:"mountTarget"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
}

type Report struct {
	Admissible     bool                `json:"admissible"`
	SchemaErrors   []buildspec.Issue   `json:"schemaErrors"`
	SemanticErrors []buildspec.Issue   `json:"semanticErrors"`
	Decisions      []Decision          `json:"policyDecisions"`
	Secrets        []SecretReadiness   `json:"secretReadiness"`
	PlatformIssues []discovery.Warning `json:"platformIssues"`
	Warnings       []discovery.Warning `json:"warnings"`
	SecretBearing  bool                `json:"secretBearing"`
	// BaseImagePins are the FROM tags admission resolved to digests; the plan carries them (ADR 0049).
	BaseImagePins []buildplan.BaseImagePin `json:"baseImagePins,omitempty"`
	// Readiness predicts problems the build is likely to meet; it is advice and never changes Admissible (ADR 0050).
	Readiness []discovery.Readiness `json:"readiness"`
	needs     []discovery.NetworkNeed
	analysis  *discovery.DockerfileAnalysis
	// providers are the provider versions a worker must have installed, such as the approved Railpack.
	providers map[string]string
}

// Analyzer analyses snapshot files statically in the discovery sandbox.
type Analyzer interface {
	Analyse(ctx context.Context, in discovery.Input) (discovery.Result, error)
}

// Capacity answers whether any enrolled worker may build uploaded source for the given platforms.
type Capacity interface {
	// UntrustedCapacity considers only pools in locations, when the project is restricted to some, and only engines
	// that deliver secrets for a secret-bearing build.
	UntrustedCapacity(ctx context.Context, orgID string, locations []string, req isolation.Requirement) (bool, []string, error)
	ProjectLocations(ctx context.Context, projectID string) ([]string, error)
}

// Bindings returns a project's active secret bindings by logical ID.
type Bindings interface {
	ActiveFor(ctx context.Context, orgID, projectID string) (map[string]secrets.Binding, error)
}

// ImageSources decides private upstream images pulled through authorizing mirrors (ADR 0030).
type ImageSources interface {
	Admit(ctx context.Context, orgID, projectID, image string) (private, allowed bool, err error)
}

// TagResolver returns the digest a public image tag points to now: the index digest for a multi-platform image.
type TagResolver interface {
	ResolveTag(ctx context.Context, ref string) (string, error)
}

type Checker struct {
	imageSources ImageSources
	tags         TagResolver
	bindings     Bindings
	capacity     Capacity
	specs        *specs.Service
	sources      *sources.Service
	recipes      *recipes.Statuses
	policies     *policy.Store
	analyzer     Analyzer
	methods      MethodPolicy
	allowances   EgressAllowances
}

// EgressAllowances lists a project's active egress allowances (ADR 0052).
type EgressAllowances interface {
	ProjectAllowances(ctx context.Context, orgID, projectID string) ([]egress.Allowance, error)
}

// UseEgressAllowances widens each project's egress by its approved allowances.
func (c *Checker) UseEgressAllowances(a EgressAllowances) { c.allowances = a }

// ProjectEgress widens eff's egress policy by the project's active allowances for method.
func ProjectEgress(ctx context.Context, a EgressAllowances, eff *policy.Effective, orgID, projectID, method string) error {
	eff.Egress = eff.Egress.ForMethod(method)
	if a == nil || projectID == "" {
		return nil
	}
	all, err := a.ProjectAllowances(ctx, orgID, projectID)
	if err != nil {
		return err
	}
	eff.Egress = eff.Egress.ForProject(method, all)
	return nil
}

// MethodPolicy checks the build method against the administrator's policy and provider approvals (ADR 0047).
type MethodPolicy interface {
	MethodAllowed(ctx context.Context, orgID, method, pool, project string) error
	Config(ctx context.Context, orgID, name string) (providers.Config, error)
}

func (c *Checker) UseMethodPolicy(m MethodPolicy) { c.methods = m }

func NewChecker(sp *specs.Service, src *sources.Service, r *recipes.Statuses, p *policy.Store, a Analyzer, c Capacity) *Checker {
	return &Checker{specs: sp, sources: src, recipes: r, policies: p, analyzer: a, capacity: c}
}

func (c *Checker) UseBindings(b Bindings) { c.bindings = b }

// UseImageSources checks base images from private upstreams against the approved image sources.
func (c *Checker) UseImageSources(s ImageSources) { c.imageSources = s }

// UseTagResolver lets the base-images policy pin tags at admission; without it such tags are refused.
func (c *Checker) UseTagResolver(t TagResolver) { c.tags = t }

// Check evaluates revision n of specID against snapshotID with the organization's current policies.
func (c *Checker) Check(ctx context.Context, p authz.Principal, roles []authz.Role, specID string, n int, snapshotID string) (Report, error) {
	return c.check(ctx, p, roles, specID, n, snapshotID, true)
}

// CheckForPeer is Check for a build delegated to a federation peer: every rule applies except local worker
// capacity, which the peer's own admission decides (ADR 0037).
func (c *Checker) CheckForPeer(ctx context.Context, p authz.Principal, roles []authz.Role, specID string, n int, snapshotID string) (Report, error) {
	return c.check(ctx, p, roles, specID, n, snapshotID, false)
}

func (c *Checker) check(ctx context.Context, p authz.Principal, roles []authz.Role, specID string, n int, snapshotID string, placement bool) (Report, error) {
	sp, rev, err := c.specs.Revision(ctx, p, roles, specID, n)
	if err != nil {
		return Report{}, err
	}
	snap, err := c.sources.GetSnapshot(ctx, p, roles, snapshotID)
	if err != nil {
		return Report{}, err
	}
	if snap.ProjectID != sp.ProjectID {
		return Report{}, sources.ErrNotFound
	}
	if snap.DeletedAt != nil {
		return Report{}, sources.ErrDeleted
	}
	eff, err := c.policies.Effective(ctx, p.OrgID)
	if err != nil {
		return Report{}, err
	}
	r, err := c.evaluate(ctx, p.OrgID, sp.ProjectID, rev.Document, &snap, eff, true)
	if err != nil {
		return r, err
	}
	if !placement {
		return r, nil
	}
	if err := c.poolCheck(ctx, p.OrgID, sp.ProjectID, rev.Document, &r); err != nil {
		return r, err
	}
	return r, nil
}

// poolCheck refuses admission when no verified pool can build uploaded source (always untrusted) for the spec's
// platforms; uploaded source never runs without a verified isolation profile.
func (c *Checker) poolCheck(ctx context.Context, orgID, projectID string, raw json.RawMessage, r *Report) error {
	var d struct {
		Spec struct {
			Target struct {
				Platforms []string `json:"platforms"`
			} `json:"target"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(raw, &d)
	platforms := d.Spec.Target.Platforms
	if len(platforms) == 0 {
		platforms = []string{"linux/amd64"}
	}
	locations, err := c.capacity.ProjectLocations(ctx, projectID)
	if err != nil {
		return err
	}
	ok, reasons, err := c.capacity.UntrustedCapacity(ctx, orgID, locations, isolation.Requirement{Platforms: platforms, Secrets: r.SecretBearing, Providers: r.providers, BaseImagePins: len(r.BaseImagePins) > 0})
	if err != nil || ok {
		return err
	}
	if len(locations) > 0 {
		reasons = append(reasons, "project restricted to "+strings.Join(locations, ", "))
	}
	if slices.Contains(reasons, "engine-cannot-deliver-secrets") {
		r.PlatformIssues = append(r.PlatformIssues, discovery.Warning{Code: "engine-cannot-deliver-secrets", Message: "The verified workers that could build this use an engine that delivers no secrets (Buildah). Secret-bearing builds need a BuildKit pool."})
	}
	if slices.Contains(reasons, "engine-cannot-pin-base-images") {
		r.PlatformIssues = append(r.PlatformIssues, discovery.Warning{Code: "engine-cannot-pin-base-images", Message: "The verified workers that could build this use an engine that cannot build pinned base images (Buildah). Builds whose base image tags are pinned at admission need a BuildKit pool."})
	}
	if slices.Contains(reasons, "platform-unsupported") {
		r.PlatformIssues = append(r.PlatformIssues, discovery.Warning{Code: "platform-unsupported", Message: "No enrolled worker builds for " + strings.Join(platforms, ", ") + ". Choose a processor architecture your workers support, or ask an administrator to add a worker for it."})
	}
	if slices.Contains(reasons, "provider-missing") {
		var need []string
		for name, v := range r.providers {
			need = append(need, name+" "+v)
		}
		sort.Strings(need)
		r.PlatformIssues = append(r.PlatformIssues, discovery.Warning{Code: "provider-missing", Message: "No verified worker has " + strings.Join(need, ", ") + ". " + providerHint(r.providers)})
	}
	r.PlatformIssues = append(r.PlatformIssues, discovery.Warning{Code: "no-verified-pool", Message: "No verified worker can build uploaded source for " + strings.Join(platforms, ", ") + " yet (" + strings.Join(reasons, ", ") + "). An administrator must approve a pool's isolation profile, and its workers must pass their own self-tests."})
	r.Admissible = false
	return nil
}

// Impact reports which specs a proposed policy document would newly block, using each project's newest upload.
func (c *Checker) Impact(ctx context.Context, orgID, kind string, doc json.RawMessage, latestSnapshot func(projectID string) *sources.Snapshot) ([]string, int, error) {
	current, err := c.policies.Effective(ctx, orgID)
	if err != nil {
		return nil, 0, err
	}
	proposed := current
	if err := proposed.Set(kind, doc); err != nil {
		return nil, 0, err
	}
	all, revs, err := c.specs.LatestRevisions(ctx, orgID)
	if err != nil {
		return nil, 0, err
	}
	blocked := []string{}
	projects := map[string]bool{}
	for i, sp := range all {
		snap := latestSnapshot(sp.ProjectID)
		before, err := c.evaluate(ctx, orgID, sp.ProjectID, revs[i].Document, snap, current, false)
		if err != nil {
			return nil, 0, err
		}
		after, err := c.evaluate(ctx, orgID, sp.ProjectID, revs[i].Document, snap, proposed, false)
		if err != nil {
			return nil, 0, err
		}
		if policyClean(before) && !policyClean(after) {
			blocked = append(blocked, sp.ID)
			projects[sp.ProjectID] = true
		}
	}
	return blocked, len(projects), nil
}

func policyClean(r Report) bool {
	for _, d := range r.Decisions {
		if d.Outcome == "block" {
			return false
		}
	}
	for _, i := range r.SemanticErrors {
		if strings.HasPrefix(i.Rule, "arg-policy:") {
			return false
		}
	}
	return true
}

// evaluate checks a revision against eff. Only with resolve does it look up tags the base-images policy pins; the
// impact preview treats them as pinnable instead of querying registries for every spec.
func (c *Checker) evaluate(ctx context.Context, orgID, projectID string, raw json.RawMessage, snap *sources.Snapshot, eff policy.Effective, resolve bool) (Report, error) {
	r := Report{SchemaErrors: []buildspec.Issue{}, SemanticErrors: []buildspec.Issue{}, Decisions: []Decision{}, Secrets: []SecretReadiness{}, PlatformIssues: []discovery.Warning{}, Warnings: []discovery.Warning{}, Readiness: []discovery.Readiness{}}
	opts := buildspec.DefaultOptions()
	opts.ArgRules = eff.ArgRules()
	res, err := buildspec.Validate(raw, opts)
	if err != nil {
		return r, err
	}
	r.SchemaErrors = append(r.SchemaErrors, res.SchemaErrors...)
	r.SemanticErrors = append(r.SemanticErrors, res.SemanticErrors...)
	if res.Document == nil {
		return r, nil
	}
	d := res.Document
	if err := ProjectEgress(ctx, c.allowances, &eff, orgID, projectID, d.Spec.Build.Method); err != nil {
		return r, err
	}
	semantic := func(rule, ptr, format string, args ...any) {
		r.SemanticErrors = append(r.SemanticErrors, buildspec.Issue{Rule: rule, Path: ptr, Message: fmt.Sprintf(format, args...)})
	}
	warn := func(code, format string, args ...any) {
		r.Warnings = append(r.Warnings, discovery.Warning{Code: code, Message: fmt.Sprintf(format, args...)})
	}

	platforms := []string{"linux/amd64"}
	if d.Spec.Target != nil && len(d.Spec.Target.Platforms) > 0 {
		platforms = d.Spec.Target.Platforms
	}
	for _, pl := range platforms {
		if !EnginePlatforms[pl] && !strings.HasPrefix(pl, "windows/") {
			r.PlatformIssues = append(r.PlatformIssues, discovery.Warning{Code: "engine-platform", Message: "The build engine cannot build " + pl + "; supported: linux/amd64, linux/arm64."})
		}
	}

	if c.methods != nil {
		if err := c.methods.MethodAllowed(ctx, orgID, d.Spec.Build.Method, "", projectID); err != nil {
			var refused *providers.Invalid
			if !errors.As(err, &refused) {
				return r, err
			}
			r.SemanticErrors = append(r.SemanticErrors, buildspec.Issue{Path: "/spec/build/method", Rule: "build-method-not-allowed", Message: refused.Reason})
		}
	}
	var images []string
	var mounts []discovery.SecretMount
	var from fromImages
	switch d.Spec.Build.Method {
	case "recipe":
		images = c.recipeChecks(ctx, orgID, d, platforms, &r, semantic, warn)
	case "dockerfile":
		var err error
		images, mounts, from, err = c.dockerfileChecks(ctx, d, snap, eff, &r, semantic, warn, resolve)
		if err != nil {
			return r, err
		}
	case providers.MethodRailpack:
		var err error
		if images, err = c.railpackChecks(ctx, orgID, d, snap, &r, semantic, warn); err != nil {
			return r, err
		}
	case providers.MethodBuildpacks:
		var err error
		if images, err = c.buildpacksChecks(ctx, orgID, d, snap, &r, semantic, warn); err != nil {
			return r, err
		}
	}
	imageMetadataWarnings(d, eff, warn)
	if resolve && d.Spec.Build.Method != "dockerfile" {
		r.needs = c.methodReadiness(ctx, d, snap, &r)
	}
	if presets, ok := c.egressPresets(ctx, orgID, d, eff); ok {
		dec := Decision{PolicyID: eff.IDs[policy.KindEgress], PolicyVersion: atoi(eff.Versions[policy.KindEgress]), Kind: policy.KindEgress, Outcome: "allow"}
		cat, err := egress.Builtin()
		if err != nil {
			return r, err
		}
		_, refused := cat.Resolve(presets, eff.Egress)
		if len(refused) > 0 {
			dec.Outcome, dec.Reason = "block", "The build needs network access to "+strings.Join(refused, ", ")+", which the egress policy does not allow. Ask an administrator to allow it, or request access for this project."
		}
		r.Decisions = append(r.Decisions, dec)
		r.Readiness = append(r.Readiness, predictEgress(cat, r.needs, d.Spec.Build.Method, presets, eff.Egress, refused)...)
	}
	if len(images) > 0 {
		dec := Decision{PolicyID: eff.IDs[policy.KindBaseImages], PolicyVersion: atoi(eff.Versions[policy.KindBaseImages]), Kind: policy.KindBaseImages, Outcome: "allow"}
		var refused []string
		unpinned := false
		for _, img := range images {
			if why := eff.BaseImageAllowed(img); why != "" {
				refused = append(refused, img+" ("+why+")")
				unpinned = unpinned || why == policy.ErrNotPinned
				if repo := eff.UnlistedRepository(img); repo != "" && !slices.Contains(dec.Requestable, repo) {
					dec.Requestable = append(dec.Requestable, repo)
				}
				continue
			}
			private := false
			if c.imageSources != nil {
				var allowed bool
				if private, allowed, err = c.imageSources.Admit(ctx, orgID, projectID, img); err != nil {
					return r, err
				}
				if private && !allowed {
					refused = append(refused, img+" (no approved image source lets this project's team pull it)")
					continue
				}
			}
			if !eff.NeedsPin(img) || slices.ContainsFunc(r.BaseImagePins, func(p buildplan.BaseImagePin) bool { return p.Ref == img }) {
				continue
			}
			pin, why := c.pin(ctx, img, from, private, resolve)
			if why != "" {
				refused = append(refused, img+" ("+why+")")
			} else if pin.Image != "" {
				r.BaseImagePins = append(r.BaseImagePins, pin)
			}
		}
		if len(refused) > 0 {
			dec.Outcome, dec.Reason = "block", "Base images not allowed: "+strings.Join(refused, "; ")
			if len(dec.Requestable) > 0 {
				dec.Reason += ". A project developer can request an unlisted repository on the project page; an organization administrator approves it."
			}
			if unpinned {
				dec.Reason += ". An organization administrator can let Samuori pin tags to their current digest at admission in the base-images policy (Administration > Recipes & base images)."
			}
			r.BaseImagePins = nil
		} else if len(r.BaseImagePins) > 0 {
			sort.Slice(r.BaseImagePins, func(i, j int) bool { return r.BaseImagePins[i].Ref < r.BaseImagePins[j].Ref })
			var pinned []string
			for _, p := range r.BaseImagePins {
				pinned = append(pinned, p.Ref+" → "+p.Image)
			}
			dec.Reason = "Pinned at admission: " + strings.Join(pinned, "; ")
		}
		if len(dec.Reason) > 1000 {
			dec.Reason = dec.Reason[:1000] + "…"
		}
		r.Decisions = append(r.Decisions, dec)
	}

	bound := map[string]secrets.Binding{}
	if c.bindings != nil && len(d.Spec.Secrets) > 0 {
		if bound, err = c.bindings.ActiveFor(ctx, orgID, projectID); err != nil {
			return r, err
		}
	}
	declared := map[string]buildspec.Secret{}
	for _, s := range d.Spec.Secrets {
		declared[s.ID] = s
		r.SecretBearing = r.SecretBearing || s.Usage == "" || s.Usage == "build"
		typ, usage := s.Type, s.Usage
		if typ == "" {
			typ = "text"
		}
		if usage == "" {
			usage = "build"
		}
		sr := SecretReadiness{ID: s.ID, Required: s.IsRequired(), Type: typ, Usage: usage, MountTarget: s.MountTarget()}
		b, ok := bound[s.ID]
		switch {
		case ok && b.Allows(usage):
			sr.Status = "ready"
		case ok:
			sr.Status, sr.Reason = "missing", fmt.Sprintf("The project's binding for %q is for %s use only. Ask an administrator to bind it for %s.", s.ID, b.Usage, usage)
		case !s.IsRequired():
			sr.Status, sr.Reason = "unbound", "Optional and not bound for this project, so it will not be mounted."
		default:
			sr.Status, sr.Reason = "missing", fmt.Sprintf("No secret is bound to %q for this project. Ask an administrator to bind one under Administration → Secrets.", s.ID)
		}
		r.Secrets = append(r.Secrets, sr)
	}
	for _, m := range mounts {
		s, ok := declared[m.ID]
		switch {
		case !ok:
			semantic("secret-undeclared", "/spec/secrets", "the Dockerfile mounts secret %q, which the spec does not declare", m.ID)
		case s.Mount != nil && m.Target != "" && s.Mount.Target != m.Target:
			semantic("secret-target-mismatch", "/spec/secrets", "secret %q is mounted at %s in the Dockerfile but %s in the spec", m.ID, m.Target, s.Mount.Target)
		}
	}
	if d.Spec.Build.Method == "dockerfile" && snap != nil {
		used := map[string]bool{}
		for _, m := range mounts {
			used[m.ID] = true
		}
		for id, s := range declared {
			if !used[id] && (s.Usage == "" || s.Usage == "build") {
				warn("secret-unused", "Secret %q is declared for the build but the Dockerfile never mounts it.", id)
			}
		}
	}
	sort.Slice(r.SemanticErrors, func(i, j int) bool { return r.SemanticErrors[i].Path < r.SemanticErrors[j].Path })
	specReadiness(d, r.analysis, &r)
	discovery.SortReadiness(r.Readiness)

	r.Admissible = len(r.SchemaErrors) == 0 && len(r.SemanticErrors) == 0 && len(r.PlatformIssues) == 0
	for _, dec := range r.Decisions {
		if dec.Outcome == "block" || dec.Outcome == "require-exception" {
			r.Admissible = false
		}
	}
	for _, s := range r.Secrets {
		if s.Required && s.Status != "ready" {
			r.Admissible = false
		}
	}
	return r, nil
}

// egressPresets returns the ecosystem presets the build needs: the recipe's, or the policy's Dockerfile presets.
func (c *Checker) egressPresets(ctx context.Context, orgID string, d *buildspec.Document, eff policy.Effective) ([]string, bool) {
	switch d.Spec.Build.Method {
	case "recipe":
		recipe, _, err := c.recipes.Get(ctx, orgID, d.Spec.Build.Recipe.ID, d.Spec.Build.Recipe.Version)
		if err != nil {
			return nil, false
		}
		presets := append(slices.Clone(recipe.EgressPresets), recipePackagePresets(recipe, d)...)
		if d.Spec.Build.Recipe.Frontend != nil {
			node, _, err := c.recipes.Get(ctx, orgID, buildplan.FrontendRecipe, buildplan.FrontendRecipeVersion)
			if err != nil {
				return nil, false
			}
			presets = append(presets, node.EgressPresets...)
		}
		return presets, true
	case "dockerfile":
		return eff.Egress.DockerfilePresets, true
	case providers.MethodRailpack, providers.MethodBuildpacks:
		if c.methods == nil {
			return nil, false
		}
		cfg, err := c.methods.Config(ctx, orgID, d.Spec.Build.Method)
		return cfg.EgressPresets, err == nil && cfg.Enabled
	}
	return nil, false
}

// recipePackagePresets returns the distribution presets a built-in recipe's system packages download from.
func recipePackagePresets(recipe recipes.Recipe, d *buildspec.Document) []string {
	app := d.Spec.Application
	if recipe.Custom != nil || app == nil || app.Runtime == nil {
		return nil
	}
	rt, err := recipe.ResolveRuntime(app.Runtime.VersionConstraint)
	if err != nil {
		return nil
	}
	_, presets, _ := rt.SystemPackages(app.Type, d.Spec.Build.Recipe.SystemPackages)
	return presets
}

func (c *Checker) recipeChecks(ctx context.Context, orgID string, d *buildspec.Document, platforms []string, r *Report, semantic func(string, string, string, ...any), warn func(string, string, ...any)) []string {
	ref := d.Spec.Build.Recipe
	recipe, st, err := c.recipes.Get(ctx, orgID, ref.ID, ref.Version)
	switch {
	case err != nil:
		semantic("recipe-unknown", "/spec/build/recipe", "recipe %s version %s does not exist", ref.ID, ref.Version)
		return nil
	case st.Status == recipes.Withdrawn:
		semantic("recipe-withdrawn", "/spec/build/recipe", "recipe %s %s has been withdrawn by an administrator: %s", ref.ID, ref.Version, st.Reason)
		return nil
	case st.Status == recipes.Deprecated:
		warn("recipe-deprecated", "Recipe %s %s is deprecated: %s", ref.ID, ref.Version, st.Reason)
	}
	if recipe.Custom != nil {
		if ref.Frontend != nil || ref.Path != "" {
			semantic("frontend-custom-recipe", "/spec/build/recipe", "a frontend and an application folder need a built-in recipe")
		}
		return c.customRecipeChecks(d, recipe, platforms, r, semantic)
	}
	app := d.Spec.Application
	if app == nil || app.Runtime == nil {
		return nil
	}
	if !contains(recipe.Languages, app.Runtime.Language) {
		semantic("recipe-language", "/spec/application/runtime/language", "%s cannot build %s; it supports %s", recipe.DisplayName, app.Runtime.Language, strings.Join(recipe.Languages, ", "))
	}
	if app.Type != "" && !contains(recipe.ApplicationTypes, app.Type) {
		semantic("recipe-application-type", "/spec/application/type", "%s does not support %s applications", recipe.DisplayName, app.Type)
	}
	for _, pl := range platforms {
		if EnginePlatforms[pl] && !contains(recipe.Platforms, pl) {
			r.PlatformIssues = append(r.PlatformIssues, discovery.Warning{Code: "recipe-platform", Message: recipe.DisplayName + " does not support " + pl + "; it supports " + strings.Join(recipe.Platforms, ", ") + "."})
		}
	}
	rt, err := recipe.ResolveRuntime(app.Runtime.VersionConstraint)
	if err != nil {
		semantic("runtime-unavailable", "/spec/application/runtime/versionConstraint", "%s", err.Error())
		return nil
	}
	if _, _, problems := rt.SystemPackages(app.Type, ref.SystemPackages); len(problems) > 0 {
		for _, p := range problems {
			semantic("system-packages-unsupported", p.Path, "%s", p.Message)
		}
	}
	images := rt.Images(app.Type)
	if f := ref.Frontend; f != nil {
		node, _, err := c.recipes.Get(ctx, orgID, buildplan.FrontendRecipe, buildplan.FrontendRecipeVersion)
		if err != nil {
			semantic("frontend-runtime", "/spec/build/recipe/frontend", "the frontend needs the Node.js recipe: %s", err.Error())
			return images
		}
		nrt, err := node.ResolveRuntime(f.Node)
		if err != nil {
			semantic("frontend-runtime", "/spec/build/recipe/frontend/node", "%s", err.Error())
			return images
		}
		images = append(images, nrt.BuildImage)
	}
	return images
}

// fromImages are a Dockerfile's FROM images and the names that would replace one in BuildKit: user-declared named
// contexts and stage names.
type fromImages struct {
	images   map[string]bool
	contexts map[string]bool
	names    map[string]bool
}

// pin resolves a FROM tag the base-images policy pins (ADR 0049), or says why it cannot be pinned. Without resolve
// it returns an empty pin for a tag that would be resolved.
func (c *Checker) pin(ctx context.Context, img string, from fromImages, private, resolve bool) (buildplan.BaseImagePin, string) {
	name, err := buildplan.ContextName(img)
	switch {
	case !from.images[img] || from.contexts[img]:
		return buildplan.BaseImagePin{}, "only Dockerfile FROM images are pinned at admission; pin it by digest"
	case err != nil:
		return buildplan.BaseImagePin{}, "it is not a valid image reference"
	case from.names[name]:
		return buildplan.BaseImagePin{}, "a named build context or stage called " + name + " would replace it, so Samuori cannot pin it; pin it by digest"
	case private:
		return buildplan.BaseImagePin{}, "it comes from a private image source, and Samuori pins tags only on public registries; pin it by digest"
	case !resolve:
		return buildplan.BaseImagePin{}, ""
	case c.tags == nil:
		return buildplan.BaseImagePin{}, "this server cannot resolve tags; pin it by digest"
	}
	digest, err := c.tags.ResolveTag(ctx, img)
	if err != nil {
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		return buildplan.BaseImagePin{}, "Samuori could not resolve the tag to a digest: " + msg + ". Check that the tag exists and its registry is reachable, then build again, or pin it by digest"
	}
	return buildplan.BaseImagePin{Ref: img, Image: policy.Repository(policy.Normalize(img)) + "@" + digest}, ""
}

func (c *Checker) dockerfileChecks(ctx context.Context, d *buildspec.Document, snap *sources.Snapshot, eff policy.Effective, r *Report, semantic func(string, string, string, ...any), warn func(string, string, ...any), resolve bool) ([]string, []discovery.SecretMount, fromImages, error) {
	from := fromImages{images: map[string]bool{}, contexts: map[string]bool{}, names: map[string]bool{}}
	if snap == nil {
		return nil, nil, from, nil
	}
	df := d.Spec.Build.Dockerfile
	manifest, err := c.sources.Manifest(*snap)
	if err != nil {
		return nil, nil, from, err
	}
	files := map[string]sources.File{}
	dirs := map[string]bool{".": true}
	for _, f := range manifest.Files {
		files[f.Path] = f
		for p := path.Dir(f.Path); p != "."; p = path.Dir(p) {
			dirs[p] = true
		}
	}
	for _, dir := range manifest.Directories {
		dirs[dir] = true
	}
	ctxDir := path.Clean(df.Context)
	if df.Context == "" {
		ctxDir = "."
	}
	dfName := df.Path
	if dfName == "" {
		dfName = "Dockerfile"
	}
	if !dirs[ctxDir] {
		semantic("context-missing", "/spec/build/dockerfile/context", "the build context %s does not exist in the upload", df.Context)
		return nil, nil, from, nil
	}
	var contextImages []string
	for name, nc := range df.NamedContexts {
		if n, err := buildplan.ContextName(name); err == nil {
			from.names[n] = true
		}
		from.names[name] = true
		switch {
		case nc.Image != "":
			contextImages = append(contextImages, nc.Image)
		case !dirs[path.Clean(nc.Path)]:
			semantic("named-context-missing", "/spec/build/dockerfile/namedContexts/"+name, "named context %s points to %s, which does not exist in the upload", name, nc.Path)
		}
	}
	sort.Strings(contextImages)
	dfPath := path.Clean(path.Join(ctxDir, dfName))
	f, ok := files[dfPath]
	if !ok {
		semantic("dockerfile-missing", "/spec/build/dockerfile/path", "%s does not exist in the upload", dfPath)
		return nil, nil, from, nil
	}
	if f.Size > maxDockerfileBytes {
		semantic("dockerfile-too-large", "/spec/build/dockerfile/path", "%s is larger than 1 MiB", dfPath)
		return nil, nil, from, nil
	}
	content, err := c.sources.ReadFile(f)
	if err != nil {
		return nil, nil, from, err
	}
	in := discovery.Input{Files: []discovery.InputFile{{Path: dfPath, Size: int64(len(content)), Content: content}}}
	if resolve {
		if in, err = c.contextInput(manifest, ctxDir); err != nil {
			return nil, nil, from, err
		}
		i := slices.IndexFunc(in.Files, func(f discovery.InputFile) bool { return f.Path == dfPath })
		if i < 0 {
			in.Files = append(in.Files, discovery.InputFile{Path: dfPath})
			i = len(in.Files) - 1
		}
		in.Files[i].Size, in.Files[i].Content = int64(len(content)), content
	}
	in.Dockerfile, in.ApprovedFrontends = dfPath, eff.Frontends.Approved
	res, err := c.analyzer.Analyse(ctx, in)
	if err != nil || len(res.Dockerfiles) != 1 || res.Dockerfiles[0].Path != dfPath {
		semantic("dockerfile-analysis", "/spec/build/dockerfile/path", "the Dockerfile could not be analysed safely")
		return nil, nil, from, nil
	}
	a := res.Dockerfiles[0]
	r.analysis, r.needs = &a, a.NetworkNeeds
	r.Readiness = append(r.Readiness, a.Readiness...)
	if a.Frontend != "" {
		dec := Decision{PolicyID: eff.IDs[policy.KindFrontends], PolicyVersion: atoi(eff.Versions[policy.KindFrontends]), Kind: policy.KindFrontends, Outcome: "allow"}
		if !eff.FrontendApproved(a.Frontend) {
			dec.Outcome, dec.Reason = "block", "The # syntax= frontend "+a.Frontend+" is not on the approved list; custom frontends run code inside the build engine."
		}
		r.Decisions = append(r.Decisions, dec)
	}
	for _, w := range a.RefusedFeatures {
		if w.Code != "frontend-not-approved" {
			semantic("dockerfile-refused:"+w.Code, "/spec/build/dockerfile/path", "%s", w.Message)
		}
	}
	for _, w := range a.Ambiguities {
		switch w.Code {
		case "base-image-variable", "secret-id-variable", "parse-error", "mount-unreadable":
			semantic("dockerfile-ambiguous:"+w.Code, "/spec/build/dockerfile/path", "%s", w.Message)
		default:
			warn(w.Code, "%s", w.Message)
		}
	}
	if df.Target != "" && !contains(a.Stages, df.Target) {
		semantic("target-missing", "/spec/build/dockerfile/target", "stage %q is not defined in %s (stages: %s)", df.Target, dfPath, strings.Join(a.Stages, ", "))
	}
	for _, img := range a.BaseImages {
		from.images[img] = true
	}
	for _, img := range contextImages {
		from.contexts[img] = true
	}
	for _, st := range a.Stages {
		from.names[strings.ToLower(st)] = true
	}
	return append(a.BaseImages, contextImages...), a.SecretMounts, from, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func atoi(s string) int {
	n := 0
	fmt.Sscan(s, &n)
	return n
}

// customRecipeChecks checks a custom recipe's parameters, secrets, application type and platforms, and returns its
// base images for the base-image and image-source policies (ADR 0041).
func (c *Checker) customRecipeChecks(d *buildspec.Document, recipe recipes.Recipe, platforms []string, r *Report, semantic func(string, string, string, ...any)) []string {
	def := recipe.Custom
	if !d.Spec.Build.Recipe.SystemPackages.IsZero() {
		semantic("system-packages-custom-recipe", "/spec/build/recipe/systemPackages", "%s is a custom recipe, which installs its own system packages; remove systemPackages or ask its author to add them", recipe.DisplayName)
	}
	if _, err := def.Resolve(d.Spec.Build.Recipe.Parameters); err != nil {
		var inv *recipedef.Invalid
		if errors.As(err, &inv) {
			for _, p := range inv.Problems {
				semantic("recipe-parameter", p.Path, "%s", p.Message)
			}
		}
	}
	for _, id := range def.Secrets {
		if !slices.ContainsFunc(d.Spec.Secrets, func(s buildspec.Secret) bool { return s.ID == id }) {
			semantic("recipe-secret", "/spec/secrets", "%s uses the secret %s; add it to the secrets the build needs", recipe.DisplayName, id)
		}
	}
	if app := d.Spec.Application; app != nil && app.Type != "" && len(def.ApplicationTypes) > 0 && !contains(def.ApplicationTypes, app.Type) {
		semantic("recipe-application-type", "/spec/application/type", "%s does not support %s applications", recipe.DisplayName, app.Type)
	}
	for _, pl := range platforms {
		if EnginePlatforms[pl] && !contains(def.Platforms, pl) {
			r.PlatformIssues = append(r.PlatformIssues, discovery.Warning{Code: "recipe-platform", Message: recipe.DisplayName + " does not support " + pl + "; it supports " + strings.Join(def.Platforms, ", ") + "."})
		}
	}
	return def.Images()
}

func providerHint(need map[string]string) string {
	if _, ok := need["buildpacks"]; ok {
		return "Buildpacks builds run on BuildKit workers of rootless-dedicated-vm pools, with nothing extra installed; Buildah and vm-per-job workers do not run them yet."
	}
	return "Railpack runs on rootless-dedicated-vm workers installed with --railpack; vm-per-job guest images do not carry it yet."
}
