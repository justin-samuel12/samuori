package api

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"samuori.invalid/samuori/api/schemas"
	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/notify"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/publication"
	"samuori.invalid/samuori/internal/recipedef"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/tenancy"
	"samuori.invalid/samuori/internal/workers"
)

type importItem struct {
	Section string `json:"section"`
	Name    string `json:"name"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
}

type importAttention struct {
	Section string `json:"section"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Link    string `json:"link,omitempty"`
}

type importSummary struct {
	Create    int `json:"create"`
	Update    int `json:"update"`
	Unchanged int `json:"unchanged"`
	Skip      int `json:"skip"`
	Failed    int `json:"failed"`
}

type importResult struct {
	DryRun             bool              `json:"dryRun"`
	Complete           bool              `json:"complete"`
	SourceOrganization string            `json:"sourceOrganization,omitempty"`
	Items              []importItem      `json:"items"`
	NeedsAttention     []importAttention `json:"needsAttention"`
	Summary            importSummary     `json:"summary"`
}

const (
	actCreate    = "create"
	actUpdate    = "update"
	actUnchanged = "unchanged"
	actSkip      = "skip"
	importReason = "imported from a configuration file"
)

var (
	configSchemaOnce sync.Once
	configSchema     *jsonschema.Schema
	configSchemaErr  error
)

// parseConfiguration reads a YAML or JSON document and checks it against the configuration schema. Problems come back
// as JSON-pointer paths with messages.
func parseConfiguration(data []byte) (configFile, []apigen.ProblemDetail, error) {
	configSchemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemas.ConfigurationV1Alpha1))
		if err != nil {
			configSchemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if configSchemaErr = c.AddResource(schemas.ConfigurationV1Alpha1ID, doc); configSchemaErr == nil {
			configSchema, configSchemaErr = c.Compile(schemas.ConfigurationV1Alpha1ID)
		}
	})
	if configSchemaErr != nil {
		return configFile{}, nil, configSchemaErr
	}
	problem := func(path, msg string) []apigen.ProblemDetail {
		return []apigen.ProblemDetail{{Field: ptr(path), Code: ptr("schema"), Reason: ptr(msg)}}
	}
	var generic any
	if err := yaml.Unmarshal(data, &generic); err != nil {
		return configFile{}, problem("/", "the file is not YAML or JSON: "+err.Error()), nil
	}
	js, err := json.Marshal(generic)
	if err != nil {
		return configFile{}, problem("/", "the file is not a configuration document: "+err.Error()), nil
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		return configFile{}, problem("/", err.Error()), nil
	}
	if err := configSchema.Validate(instance); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return configFile{}, nil, err
		}
		var out []apigen.ProblemDetail
		printer := message.NewPrinter(language.English)
		var walk func(*jsonschema.ValidationError)
		walk = func(e *jsonschema.ValidationError) {
			if len(e.Causes) == 0 {
				out = append(out, problem("/"+strings.Join(e.InstanceLocation, "/"), e.ErrorKind.LocalizedString(printer))...)
			}
			for _, c := range e.Causes {
				walk(c)
			}
		}
		walk(ve)
		sort.Slice(out, func(i, j int) bool { return *out[i].Field < *out[j].Field })
		return configFile{}, out, nil
	}
	var f configFile
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return configFile{}, problem("/", err.Error()), nil
	}
	return f, nil, nil
}

func (a *API) ImportConfiguration(w http.ResponseWriter, r *http.Request, params apigen.ImportConfigurationParams) {
	p := principal(r)
	if !p.IsOrgAdmin() {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators import configuration")
		return
	}
	dryRun := params.DryRun == nil || *params.DryRun
	if !dryRun && params.IdempotencyKey == nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "idempotency_key_required", "applying an import needs an Idempotency-Key header")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return
	}
	f, problems, err := parseConfiguration(data)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, apigen.Problem{Code: "invalid_configuration", Message: "the file is not a valid Samuori configuration; see details", RequestId: requestID(w), Details: &problems})
		return
	}
	im, err := a.newImporter(r, p, !dryRun, requestID(w))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	im.res.SourceOrganization = f.Metadata.Organization
	if err := im.run(f.Spec); err != nil {
		a.internalError(w, r, err)
		return
	}
	res := im.res
	if !dryRun {
		why := fmt.Sprintf("from %q: %d created, %d updated, %d unchanged, %d skipped, %d failed", f.Metadata.Organization, res.Summary.Create, res.Summary.Update, res.Summary.Unchanged, res.Summary.Skip, res.Summary.Failed)
		outcome := "succeeded"
		if !res.Complete {
			outcome = "failed"
		}
		if err := a.db.InTx(context.WithoutCancel(r.Context()), func(tx *sql.Tx) error {
			return audit.Append(r.Context(), a.db, tx, audit.Event{Actor: p, Action: "configuration.import", ResourceType: "configuration", ResourceID: p.OrgID, Outcome: outcome, Reason: why, RequestID: requestID(w)})
		}); err != nil {
			a.internalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// importer compares a configuration with the organization item by item, in dependency order, and with apply set
// makes each change through the service the portal uses. Names resolve to existing items or to items created
// earlier in the same import; a planned item has no ID yet, so planned holds its name.
type importer struct {
	a     *API
	r     *http.Request
	ctx   context.Context
	p     authz.Principal
	apply bool
	reqID string
	res   importResult

	teams, projects, pools, registries, channels map[string]string
	profiles                                     map[string]publication.Profile
}

const planned = "planned"

func (a *API) newImporter(r *http.Request, p authz.Principal, apply bool, reqID string) (*importer, error) {
	im := &importer{a: a, r: r, ctx: r.Context(), p: p, apply: apply, reqID: reqID, res: importResult{DryRun: !apply, Complete: true, Items: []importItem{}, NeedsAttention: []importAttention{}},
		teams: map[string]string{}, projects: map[string]string{}, pools: map[string]string{}, registries: map[string]string{}, channels: map[string]string{}, profiles: map[string]publication.Profile{}}
	n, err := a.configNames(im.ctx, p)
	if err != nil {
		return nil, err
	}
	for id, name := range n.teams {
		im.teams[name] = id
	}
	for id, ref := range n.projects {
		im.projects[ref] = id
	}
	for id, name := range n.pools {
		im.pools[name] = id
	}
	return im, nil
}

// item records one planned change and, when applying, makes it. It reports whether the change happened or would.
func (im *importer) item(section, name, action, reason string, change func() error) bool {
	it := importItem{Section: section, Name: name, Action: action, Reason: reason}
	ok := action == actCreate || action == actUpdate
	if ok && im.apply {
		if err := change(); err != nil {
			it.Outcome, it.Error, ok = "failed", im.describe(err), false
			im.res.Complete = false
			im.res.Summary.Failed++
		} else {
			it.Outcome = "applied"
		}
	}
	switch action {
	case actCreate:
		im.res.Summary.Create++
	case actUpdate:
		im.res.Summary.Update++
	case actUnchanged:
		im.res.Summary.Unchanged++
	default:
		im.res.Summary.Skip++
	}
	im.res.Items = append(im.res.Items, it)
	return ok
}

func (im *importer) attention(section, name, kind, link, format string, args ...any) {
	im.res.NeedsAttention = append(im.res.NeedsAttention, importAttention{Section: section, Name: name, Kind: kind, Message: fmt.Sprintf(format, args...), Link: link})
}

// describe turns a service's refusal into its message; anything else is logged and named only by the request ID.
func (im *importer) describe(err error) string {
	var (
		pol  *policy.Invalid
		prov *providers.Invalid
		rec  *recipes.Invalid
		def  *recipedef.Invalid
		pub  *publication.Invalid
		inUs *publication.InUse
		not  *notify.Invalid
		aud  *audit.Invalid
	)
	switch {
	case errors.As(err, &pol), errors.As(err, &prov), errors.As(err, &rec), errors.As(err, &def), errors.As(err, &pub), errors.As(err, &inUs), errors.As(err, &not), errors.As(err, &aud),
		errors.Is(err, auth.ErrSessionTimeouts), errors.Is(err, policy.ErrConflict), errors.Is(err, recipes.ErrRuntimeExists), errors.Is(err, recipes.ErrExists), errors.Is(err, recipes.ErrNotDraft):
		return err.Error()
	case errors.Is(err, publication.ErrConflict), errors.Is(err, notify.ErrConflict), errors.Is(err, workers.ErrConflict), errors.Is(err, tenancy.ErrConflict):
		return "the name is already taken"
	case errors.Is(err, workers.ErrInvalid):
		return "the name, location or isolation profile is not valid"
	}
	im.a.log.Error("configuration import item failed", "id", im.reqID, "error", err)
	return "internal error; quote request ID " + im.reqID + " to an operator"
}

func (im *importer) run(s configSpec) error {
	for _, step := range []func(configSpec) error{im.importTeams, im.importIdentity, im.importAudit, im.importPolicies, im.importBuildMethods, im.importPools,
		im.importProviders, im.importRecipes, im.importCustomRecipes, im.importRegistries, im.importProfiles, im.importDestinations, im.importChannels, im.importRules} {
		if err := step(s); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) importTeams(s configSpec) error {
	for _, name := range s.Teams {
		if _, ok := im.teams[name]; ok {
			im.item("teams", name, actUnchanged, "exists", nil)
			continue
		}
		var id string
		if im.item("teams", name, actCreate, "new team, without members or role bindings", func() error {
			t, err := im.a.tenancy.CreateTeam(im.ctx, im.p, name, im.reqID)
			id = t.ID
			return err
		}) {
			im.teams[name] = cmp.Or(id, planned)
		}
	}
	return nil
}

func (im *importer) importIdentity(s configSpec) error {
	if s.Identity == nil {
		return nil
	}
	want := auth.SessionTimeouts{Idle: time.Duration(s.Identity.Session.IdleTimeoutMinutes) * time.Minute, Max: time.Duration(s.Identity.Session.MaxSessionHours) * time.Hour,
		Warning: time.Duration(s.Identity.Session.WarningSeconds) * time.Second}
	cur, err := im.a.auth.SessionTimeouts(im.ctx, im.p.OrgID)
	if err != nil {
		return err
	}
	login, err := im.a.auth.LocalLogin(im.ctx, im.p.OrgID)
	if err != nil {
		return err
	}
	switch {
	case want.Validate() != nil:
		im.item("identity", "session timeouts", actSkip, want.Validate().Error(), nil)
	case want == cur:
		im.item("identity", "session timeouts", actUnchanged, "same", nil)
	default:
		im.item("identity", "session timeouts", actUpdate, fmt.Sprintf("idle %s, maximum %s, warning %s", want.Idle, want.Max, want.Warning), func() error {
			return im.a.auth.SetSessionTimeouts(im.ctx, im.p, want, im.reqID)
		})
	}
	if s.Identity.LocalSignIn == login {
		im.item("identity", "local sign-in", actUnchanged, "same", nil)
	} else {
		im.item("identity", "local sign-in", actUpdate, "local passwords for "+s.Identity.LocalSignIn, func() error {
			return im.a.auth.SetLocalLogin(im.ctx, im.p, s.Identity.LocalSignIn, im.reqID)
		})
	}
	return nil
}

func (im *importer) importAudit(s configSpec) error {
	if s.AuditRetention == nil {
		return nil
	}
	cur, err := im.a.retention.Policy(im.ctx, im.p.OrgID)
	if err != nil {
		return err
	}
	want := audit.Policy{RetentionDays: s.AuditRetention.RetentionDays, ExportEnabled: s.AuditRetention.ExportEnabled, RequireExport: s.AuditRetention.RequireExport}
	if cur.RetentionDays == want.RetentionDays && cur.ExportEnabled == want.ExportEnabled && cur.RequireExport == want.RequireExport {
		im.item("auditRetention", "audit retention", actUnchanged, "same", nil)
		return nil
	}
	im.item("auditRetention", "audit retention", actUpdate, fmt.Sprintf("retention %d days, scheduled export %t, purge only exported %t", want.RetentionDays, want.ExportEnabled, want.RequireExport), func() error {
		_, err := im.a.retention.SetPolicy(im.ctx, im.p, want, im.reqID)
		return err
	})
	return nil
}

// importPolicies replaces each policy whose document differs. A change that would newly block existing specs is
// left for the Policies page, which previews the impact and asks for a reason (TM-92).
func (im *importer) importPolicies(s configSpec) error {
	kinds := make([]string, 0, len(s.Policies))
	for k := range s.Policies {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		doc, err := policy.Validate(kind, s.Policies[kind])
		if err != nil {
			im.item("policies", kind, actSkip, im.describe(err), nil)
			continue
		}
		cur, err := im.a.policies.ByKind(im.ctx, im.p.OrgID, kind)
		if errors.Is(err, policy.ErrNotFound) {
			if err = im.a.policies.EnsureOrg(im.ctx, im.p.OrgID); err == nil {
				cur, err = im.a.policies.ByKind(im.ctx, im.p.OrgID, kind)
			}
		}
		if err != nil {
			return err
		}
		if canonical, err := policy.Validate(kind, cur.Document); err == nil && bytes.Equal(canonical, doc) {
			im.item("policies", kind, actUnchanged, fmt.Sprintf("same as version %d", cur.Version), nil)
			continue
		}
		blocked, projects, err := im.a.admission.Impact(im.ctx, im.p.OrgID, kind, doc, im.a.latestSnapshots(im.r, im.p))
		if err != nil {
			return err
		}
		if len(blocked) > 0 {
			im.item("policies", kind, actSkip, fmt.Sprintf("would newly block %d spec(s) in %d project(s)", len(blocked), projects), nil)
			im.attention("policies", kind, "review", "/admin/policies", "The %s policy in the file would newly block %d spec(s) in %d project(s). Change it on the Policies page, which previews the impact and asks for a reason.", kind, len(blocked), projects)
			continue
		}
		im.item("policies", kind, actUpdate, fmt.Sprintf("new version after %d", cur.Version), func() error {
			_, err := im.a.policies.Replace(im.ctx, im.p, cur.ID, kind, cur.Version, doc, importReason, im.reqID)
			return err
		})
	}
	return nil
}

func (im *importer) importBuildMethods(s configSpec) error {
	if s.BuildMethods == nil {
		return nil
	}
	want := providers.Policy{Allowed: append([]string{}, s.BuildMethods.Allowed...), Default: s.BuildMethods.Default}
	if err := want.Validate(); err != nil {
		im.item("buildMethods", "build method policy", actSkip, im.describe(err), nil)
		return nil
	}
	cur, err := im.a.providers.Policy(im.ctx, im.p.OrgID)
	if err != nil {
		return err
	}
	if slices.Equal(slices.Sorted(slices.Values(cur.Allowed)), want.Allowed) && cur.Default == want.Default {
		im.item("buildMethods", "build method policy", actUnchanged, "same", nil)
		return nil
	}
	im.item("buildMethods", "build method policy", actUpdate, "allowed "+strings.Join(want.Allowed, ", "), func() error {
		_, err := im.a.providers.SetPolicy(im.ctx, im.p, want, im.reqID)
		return err
	})
	return nil
}

func (im *importer) importPools(s configSpec) error {
	existing, err := im.a.workers.ListPools(im.ctx, im.p.OrgID)
	if err != nil {
		return err
	}
	byName := map[string]workers.Pool{}
	for _, pl := range existing {
		byName[pl.Name] = pl
	}
	for _, c := range s.WorkerPools {
		cur, ok := byName[c.Name]
		if !ok {
			var id string
			if im.item("workerPools", c.Name, actCreate, c.IsolationProfile+" in "+cmp.Or(c.Location, "the default location")+"; workers enrol and pass self-tests before it runs builds", func() error {
				pl, err := im.a.workers.CreatePool(im.ctx, im.p, workers.Pool{Name: c.Name, Location: c.Location, IsolationProfile: c.IsolationProfile, TrustDomain: c.TrustDomain, TrustedWorkloadsOnly: c.TrustedWorkloadsOnly}, im.reqID)
				id = pl.ID
				return err
			}) {
				im.pools[c.Name] = cmp.Or(id, planned)
			}
			continue
		}
		if cur.IsolationProfile != c.IsolationProfile || cur.TrustDomain != c.TrustDomain {
			im.item("workerPools", c.Name, actSkip, "isolation profile or trust domain differs", nil)
			im.attention("workerPools", c.Name, "review", "/admin/workers", "Pool %s here uses %s (trust domain %q); the file says %s (%q). Changing either approves the pool again, so change it on the Workers page.", c.Name, cur.IsolationProfile, cur.TrustDomain, c.IsolationProfile, c.TrustDomain)
			continue
		}
		if (c.Location == "" || c.Location == cur.Location) && c.TrustedWorkloadsOnly == cur.TrustedWorkloadsOnly {
			im.item("workerPools", c.Name, actUnchanged, "same", nil)
			continue
		}
		im.item("workerPools", c.Name, actUpdate, "location or trusted workloads changed", func() error {
			loc, trusted := cmp.Or(c.Location, cur.Location), c.TrustedWorkloadsOnly
			_, err := im.a.workers.UpdatePool(im.ctx, im.p, cur.ID, workers.PoolUpdate{Location: &loc, TrustedWorkloadsOnly: &trusted}, im.reqID)
			return err
		})
	}
	return nil
}

// resolve maps names onto IDs and returns the names it could not resolve.
func resolve(list []string, ids map[string]string) ([]string, []string) {
	out, missing := []string{}, []string{}
	for _, n := range list {
		if id, ok := ids[n]; ok {
			out = append(out, id)
		} else {
			missing = append(missing, n)
		}
	}
	return out, missing
}

// importProviders skips a provider whose pool or project restriction does not resolve in full, because a shorter
// or empty list would allow it more widely than the file says.
func (im *importer) importProviders(s configSpec) error {
	if s.BuildMethods == nil {
		return nil
	}
	for _, c := range s.BuildMethods.Providers {
		d, ok := providers.Lookup(c.Name)
		if !ok {
			im.item("providers", c.Name, actSkip, "this release has no such provider", nil)
			continue
		}
		pools, missingPools := resolve(c.Pools, im.pools)
		projects, missingProjects := resolve(c.Projects, im.projects)
		if missing := append(missingPools, missingProjects...); len(missing) > 0 {
			im.item("providers", c.Name, actSkip, "restricted to pools or projects that do not exist here: "+strings.Join(missing, ", "), nil)
			im.attention("providers", c.Name, "reference", "/admin/providers", "%s is restricted to %s, which do not exist here; configure it on the Providers page.", d.DisplayName, strings.Join(missing, ", "))
			continue
		}
		want := providers.Config{Enabled: c.Enabled, Version: c.Version, Artifacts: c.Artifacts, Pools: pools, Projects: projects, EgressPresets: append([]string{}, c.EgressPresets...),
			TimeoutSeconds: c.TimeoutSeconds, RunAsRoot: c.RunAsRoot}
		if slices.Contains(pools, planned) || slices.Contains(projects, planned) {
			im.item("providers", c.Name, actUpdate, "settings from the file, with pools created by this import", nil)
			continue
		}
		if err := want.Validate(d); err != nil {
			im.item("providers", c.Name, actSkip, im.describe(err), nil)
			continue
		}
		cur, err := im.a.providers.Config(im.ctx, im.p.OrgID, c.Name)
		if err != nil {
			return err
		}
		_ = cur.Validate(d)
		if a, b := mustJSON(cur), mustJSON(want); bytes.Equal(a, b) {
			im.item("providers", c.Name, actUnchanged, "same", nil)
			continue
		}
		state := "disabled"
		if want.Enabled {
			state = "enabled " + want.Version
		}
		im.item("providers", c.Name, actUpdate, state, func() error {
			_, err := im.a.providers.Set(im.ctx, im.p, c.Name, want, im.reqID)
			return err
		})
	}
	return nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (im *importer) importRecipes(s configSpec) error {
	if s.Recipes == nil {
		return nil
	}
	cat := im.a.recipes.Catalog()
	_, statuses, err := im.a.recipes.All(im.ctx, im.p.OrgID)
	if err != nil {
		return err
	}
	for _, c := range s.Recipes.Statuses {
		name := c.Recipe + "@" + c.Version
		if _, ok := cat.Get(c.Recipe, c.Version); !ok {
			im.item("recipeStatuses", name, actSkip, "this release does not ship this recipe version", nil)
			continue
		}
		if st := statuses[name]; st.Status == c.Status && (st.Reason == c.Reason || c.Status == recipes.Approved && st.Reason == "") {
			im.item("recipeStatuses", name, actUnchanged, "same", nil)
			continue
		}
		im.item("recipeStatuses", name, actUpdate, c.Status+": "+c.Reason, func() error {
			return im.a.recipes.Set(im.ctx, im.p, c.Recipe, c.Version, c.Status, c.Reason, im.reqID)
		})
	}
	for _, c := range s.Recipes.ShippedRuntimes {
		name := c.Recipe + "@" + c.Version + " runtime " + c.Label
		shipped, _, err := im.a.recipes.ListRuntimes(im.ctx, im.p, c.Recipe, c.Version)
		i := slices.IndexFunc(shipped, func(rt recipes.Runtime) bool { return rt.Version == c.Label })
		if errors.Is(err, recipes.ErrNotFound) || err == nil && i < 0 {
			im.item("shippedRuntimes", name, actSkip, "this release does not ship this runtime", nil)
			continue
		}
		if err != nil {
			return err
		}
		if rt := shipped[i]; rt.Status == c.Status && rt.Reason == c.Reason || c.Status == recipes.Approved && (rt.Status == "" || rt.Status == recipes.Approved) {
			im.item("shippedRuntimes", name, actUnchanged, "same", nil)
			continue
		}
		im.item("shippedRuntimes", name, actUpdate, c.Status+": "+c.Reason, func() error {
			return im.a.recipes.SetShippedRuntimeStatus(im.ctx, im.p, c.Recipe, c.Version, c.Label, c.Status, c.Reason, im.reqID)
		})
	}
	for _, c := range s.Recipes.OrganizationRuntimes {
		im.importOrgRuntime(c)
	}
	return nil
}

func (im *importer) importOrgRuntime(c configOrgRuntime) {
	name := c.Recipe + "@" + c.Version + " runtime " + c.Label
	rc, ok := im.a.recipes.Catalog().Get(c.Recipe, c.Version)
	if !ok {
		im.item("organizationRuntimes", name, actSkip, "this release does not ship this recipe version", nil)
		return
	}
	if slices.ContainsFunc(rc.Runtimes, func(rt recipes.Runtime) bool { return rt.Version == c.Label }) {
		im.item("organizationRuntimes", name, actSkip, "this release ships this runtime itself", nil)
		return
	}
	_, org, err := im.a.recipes.ListRuntimes(im.ctx, im.p, c.Recipe, c.Version)
	if err != nil {
		im.item("organizationRuntimes", name, actSkip, im.describe(err), nil)
		return
	}
	i := slices.IndexFunc(org, func(o recipes.OrgRuntime) bool { return o.Label == c.Label && o.Status != recipes.Withdrawn })
	approve := func(id string) error {
		_, err := im.a.recipes.DecideRuntime(im.ctx, im.p, c.Recipe, c.Version, id, "approve", importReason, im.reqID)
		return err
	}
	if i < 0 {
		im.item("organizationRuntimes", name, actCreate, "as "+c.Status, func() error {
			o, err := im.a.recipes.CreateRuntime(im.ctx, im.p, c.Recipe, c.Version, recipes.RuntimeInput{Label: c.Label, BuildImage: c.BuildImage, RunImage: c.RunImage, Note: c.Note}, im.reqID)
			if err != nil || c.Status != recipes.Approved {
				return err
			}
			return approve(o.ID)
		})
		return
	}
	cur := org[i]
	switch {
	case cur.BuildImage != c.BuildImage || cur.RunImage != c.RunImage:
		im.item("organizationRuntimes", name, actSkip, "a runtime with this label and other images exists", nil)
		im.attention("organizationRuntimes", name, "review", "/admin/recipes", "Runtime %s of %s %s uses other images here; organization runtimes never change, so withdraw it and add the imported one on the Recipes page.", c.Label, c.Recipe, c.Version)
	case cur.Status == c.Status:
		im.item("organizationRuntimes", name, actUnchanged, "same", nil)
	case cur.Status == recipes.Draft:
		im.item("organizationRuntimes", name, actUpdate, "approve the draft", func() error { return approve(cur.ID) })
	default:
		im.item("organizationRuntimes", name, actSkip, "approved here; an import never undoes an approval", nil)
	}
}

var customOrder = []string{recipes.Draft, recipes.Approved, recipes.Deprecated, recipes.Withdrawn}

// steps are the decisions that take a custom version from one status to a later one.
func steps(from, to string) []string {
	var out []string
	for i := slices.Index(customOrder, from) + 1; i <= slices.Index(customOrder, to); i++ {
		out = append(out, map[string]string{recipes.Approved: "approve", recipes.Deprecated: "deprecate", recipes.Withdrawn: "withdraw"}[customOrder[i]])
	}
	return out
}

func (im *importer) importCustomRecipes(s configSpec) error {
	if s.Recipes == nil || len(s.Recipes.Custom) == 0 {
		return nil
	}
	list, err := im.a.recipes.ListCustom(im.ctx, im.p)
	if err != nil {
		return err
	}
	existing := map[string]recipes.CustomVersion{}
	for _, v := range list {
		existing[v.Definition.ID+"@"+v.Definition.Version] = v
	}
	builtin := map[string]bool{}
	for _, rc := range im.a.recipes.Catalog().All() {
		builtin[rc.ID] = true
	}
	for _, c := range s.Recipes.Custom {
		text := mustJSON(c.Definition)
		d, err := recipedef.Parse(text)
		name := c.Definition.ID + "@" + c.Definition.Version
		if err != nil {
			im.item("customRecipes", name, actSkip, im.describe(err), nil)
			continue
		}
		if builtin[d.ID] {
			im.item("customRecipes", name, actSkip, "a built-in recipe has this ID", nil)
			continue
		}
		reason := c.Reason
		if len(strings.TrimSpace(reason)) < 3 {
			reason = importReason
		}
		decide := func(from string) error {
			for _, action := range steps(from, c.Status) {
				if _, err := im.a.recipes.DecideCustom(im.ctx, im.p, d.ID, d.Version, action, reason, im.reqID); err != nil {
					return err
				}
			}
			return nil
		}
		cur, ok := existing[name]
		if !ok {
			im.item("customRecipes", name, actCreate, "as "+c.Status, func() error {
				if _, err := im.a.recipes.CreateCustom(im.ctx, im.p, text, im.reqID); err != nil {
					return err
				}
				return decide(recipes.Draft)
			})
			continue
		}
		same := bytes.Equal(mustJSON(cur.Definition), mustJSON(d))
		later := slices.Index(customOrder, c.Status) > slices.Index(customOrder, cur.Status)
		switch {
		case !same && cur.Status != recipes.Draft:
			im.item("customRecipes", name, actSkip, "a "+cur.Status+" version with another definition exists", nil)
			im.attention("customRecipes", name, "review", "/admin/recipes", "%s %s is %s here with another definition; versions that left draft never change, so give the imported definition a new version.", d.ID, d.Version, cur.Status)
		case !same:
			im.item("customRecipes", name, actUpdate, "replace the draft, then "+c.Status, func() error {
				if _, err := im.a.recipes.ReplaceDraft(im.ctx, im.p, d.ID, d.Version, text, im.reqID); err != nil {
					return err
				}
				return decide(recipes.Draft)
			})
		case cur.Status == c.Status:
			im.item("customRecipes", name, actUnchanged, "same", nil)
		case later:
			im.item("customRecipes", name, actUpdate, cur.Status+" to "+c.Status, func() error { return decide(cur.Status) })
		default:
			im.item("customRecipes", name, actSkip, cur.Status+" here; an import never moves a version back", nil)
		}
	}
	return nil
}

func (im *importer) importRegistries(s configSpec) error {
	if len(s.Registries)+len(s.CredentialProfiles)+len(s.Destinations) == 0 {
		return nil
	}
	existing, err := im.a.publications.Registries(im.ctx, im.p)
	if err != nil {
		return err
	}
	byName := map[string]publication.Registry{}
	for _, r := range existing {
		byName[r.Name] = r
		im.registries[r.Name] = r.ID
	}
	for _, c := range s.Registries {
		want := publication.Registry{Name: c.Name, Kind: c.Kind, Endpoint: c.Endpoint, AccountID: c.AccountID, Region: c.Region, PlainHTTP: c.PlainHTTP, AllowPrivate: c.AllowPrivateNetwork}
		cur, ok := byName[c.Name]
		if !ok {
			var id string
			if im.item("registries", c.Name, actCreate, c.Kind+" "+cmp.Or(c.Endpoint, c.AccountID+" "+c.Region), func() error {
				r, err := im.a.publications.CreateRegistry(im.ctx, im.p, want, im.reqID)
				id = r.ID
				return err
			}) {
				im.registries[c.Name] = cmp.Or(id, planned)
			}
			continue
		}
		switch {
		case cur.Kind != c.Kind:
			delete(im.registries, c.Name)
			im.item("registries", c.Name, actSkip, "a "+cur.Kind+" registry has this name here", nil)
			im.attention("registries", c.Name, "review", "/admin/registries", "Registry %s is %s here and %s in the file; a registry's kind never changes.", c.Name, cur.Kind, c.Kind)
		case (c.Kind == "ecr" || cur.Endpoint == c.Endpoint) && cur.AccountID == c.AccountID && cur.Region == c.Region && cur.PlainHTTP == c.PlainHTTP && cur.AllowPrivate == c.AllowPrivateNetwork:
			im.item("registries", c.Name, actUnchanged, "same", nil)
		default:
			im.item("registries", c.Name, actUpdate, "connection settings from the file", func() error {
				_, err := im.a.publications.UpdateRegistry(im.ctx, im.p, cur.ID, want, im.reqID)
				return err
			})
		}
	}
	return nil
}

// importProfiles creates only anonymous credentials, which hold nothing secret; every other kind is listed for an
// administrator to add with its secret. Existing credentials are never changed by an import.
func (im *importer) importProfiles(s configSpec) error {
	if len(s.CredentialProfiles)+len(s.Destinations) == 0 {
		return nil
	}
	existing, err := im.a.publications.Profiles(im.ctx, im.p)
	if err != nil {
		return err
	}
	byName := map[string]publication.Profile{}
	for _, pr := range existing {
		byName[pr.Name] = pr
		im.profiles[pr.Name] = pr
	}
	for _, c := range s.CredentialProfiles {
		regID, regOK := im.registries[c.Registry]
		cur, ok := byName[c.Name]
		scopes := slices.Sorted(slices.Values(c.RepositoryScopes))
		switch {
		case ok && cur.RegistryID == regID && cur.Type == c.Type && cur.Purpose == c.Purpose && slices.Equal(slices.Sorted(slices.Values(cur.Scopes)), scopes):
			im.item("credentialProfiles", c.Name, actUnchanged, "same", nil)
		case ok:
			delete(im.profiles, c.Name)
			im.item("credentialProfiles", c.Name, actSkip, "credentials with this name and other settings exist", nil)
			im.attention("credentialProfiles", c.Name, "review", "/admin/registries", "Credentials %s exist here with another registry, type, purpose or scopes; an import never changes credentials.", c.Name)
		case !regOK:
			im.item("credentialProfiles", c.Name, actSkip, "registry "+c.Registry+" is not in this organization or the file", nil)
			im.attention("credentialProfiles", c.Name, "reference", "/admin/registries", "Credentials %s belong to registry %s, which does not exist here.", c.Name, c.Registry)
		case c.Type != "anonymous":
			im.item("credentialProfiles", c.Name, actSkip, c.Type+" credentials hold a secret, which is never exported", nil)
			im.attention("credentialProfiles", c.Name, "credential", "/admin/registries", "Add %s credentials %s (%s) for registry %s under Registries with their secret, then import again to add what uses them.", c.Type, c.Name, c.Purpose, c.Registry)
		default:
			want := publication.Profile{Name: c.Name, RegistryID: regID, Type: c.Type, Purpose: c.Purpose, Scopes: scopes}
			if im.item("credentialProfiles", c.Name, actCreate, "anonymous "+c.Purpose+" access, no secret", func() error {
				pr, err := im.a.publications.CreateProfile(im.ctx, im.p, want, im.reqID)
				want = pr
				return err
			}) {
				if want.ID == "" {
					want.ID = planned
				}
				im.profiles[c.Name] = want
			}
		}
	}
	return nil
}

// importDestinations drops allowed projects that do not exist here and says so: fewer projects is narrower access.
func (im *importer) importDestinations(s configSpec) error {
	if len(s.Destinations) == 0 {
		return nil
	}
	existing, err := im.a.publications.Destinations(im.ctx, im.p)
	if err != nil {
		return err
	}
	byName := map[string]publication.Destination{}
	for _, d := range existing {
		byName[d.Name] = d
	}
	for _, c := range s.Destinations {
		regID, regOK := im.registries[c.Registry]
		prof, profOK := im.profiles[c.PushProfile]
		if !regOK || !profOK {
			im.item("destinations", c.Name, actSkip, "needs registry "+c.Registry+" and credentials "+c.PushProfile+" here first", nil)
			im.attention("destinations", c.Name, "reference", "/admin/registries", "Destination %s needs registry %s and push credentials %s; add them and import again.", c.Name, c.Registry, c.PushProfile)
			continue
		}
		projects, missing := resolve(c.AllowedProjects, im.projects)
		if len(missing) > 0 {
			im.attention("destinations", c.Name, "reference", "/admin/registries", "Destination %s is imported without projects that do not exist here: %s. Allow them under Registries once they exist.", c.Name, strings.Join(missing, ", "))
		}
		slices.Sort(projects)
		want := publication.Destination{Name: c.Name, RegistryID: regID, Repository: c.Repository, PushProfileID: prof.ID, AllowedProjects: projects,
			ProtectedTags: slices.Sorted(slices.Values(c.ProtectedTags)), ImmutableTags: c.ImmutableTags, RequiresApproval: c.RequiresApproval,
			AcceptsRestricted: c.AcceptsRestricted, SignImages: c.SignImages, AttachProvenance: c.AttachProvenance, PromotionOnly: c.PromotionOnly}
		if want.ProtectedTags == nil {
			want.ProtectedTags = []string{}
		}
		cur, ok := byName[c.Name]
		if !ok {
			im.item("destinations", c.Name, actCreate, c.Repository+" in "+c.Registry, func() error {
				_, err := im.a.publications.CreateDestination(im.ctx, im.p, want, im.reqID)
				return err
			})
			continue
		}
		if cur.RegistryID != regID {
			im.item("destinations", c.Name, actSkip, "a destination with this name is in another registry", nil)
			im.attention("destinations", c.Name, "review", "/admin/registries", "Destination %s is in another registry here; a destination's registry never changes.", c.Name)
			continue
		}
		cur.AllowedProjects = slices.Sorted(slices.Values(cur.AllowedProjects))
		cur.ProtectedTags = slices.Sorted(slices.Values(cur.ProtectedTags))
		if cur.AllowedProjects == nil {
			cur.AllowedProjects = []string{}
		}
		if cur.ProtectedTags == nil {
			cur.ProtectedTags = []string{}
		}
		want.ID = cur.ID
		if bytes.Equal(mustJSON(cur), mustJSON(want)) {
			im.item("destinations", c.Name, actUnchanged, "same", nil)
			continue
		}
		im.item("destinations", c.Name, actUpdate, "settings from the file", func() error {
			_, err := im.a.publications.UpdateDestination(im.ctx, im.p, cur.ID, want, im.reqID)
			return err
		})
	}
	return nil
}

// importChannels creates email channels only: the others need a secret (a signing key Samuori shows once, or an
// incoming webhook URL), so they are listed for an administrator to add.
func (im *importer) importChannels(s configSpec) error {
	if len(s.NotificationChannels)+len(s.NotificationRules) == 0 {
		return nil
	}
	existing, err := im.a.notify.Channels(im.ctx, im.p)
	if err != nil {
		return err
	}
	byName := map[string]notify.Channel{}
	for _, c := range existing {
		byName[c.Name] = c
		im.channels[c.Name] = c.ID
	}
	for _, c := range s.NotificationChannels {
		cur, ok := byName[c.Name]
		setEnabled := func(id string) error {
			if c.Enabled {
				return nil
			}
			return im.a.notify.SetEnabled(im.ctx, im.p, id, false, im.reqID)
		}
		switch {
		case !ok && c.Kind == "email" && !im.a.notify.EmailReady:
			im.item("notificationChannels", c.Name, actSkip, "email needs an SMTP server in this controller's configuration", nil)
			im.attention("notificationChannels", c.Name, "reference", "/admin/notifications", "Email channel %s needs smtp.host in the controller configuration; add the channel after configuring it.", c.Name)
		case !ok && c.Kind == "email":
			var id string
			if im.item("notificationChannels", c.Name, actCreate, "email to "+strings.Join(c.Recipients, ", "), func() error {
				ch, _, err := im.a.notify.CreateChannel(im.ctx, im.p, notify.Channel{Name: c.Name, Kind: c.Kind, Recipients: c.Recipients, AllowPrivateNetwork: c.AllowPrivateNetwork}, "", im.reqID)
				if id = ch.ID; err != nil {
					return err
				}
				return setEnabled(id)
			}) {
				im.channels[c.Name] = cmp.Or(id, planned)
			}
		case !ok:
			what := "its incoming webhook URL"
			if c.Kind == "webhook" {
				what = "a new signing key, shown once, for the receiver to verify"
			}
			im.item("notificationChannels", c.Name, actSkip, c.Kind+" channels hold a secret, which is never exported", nil)
			im.attention("notificationChannels", c.Name, "secret", "/admin/notifications", "Add %s channel %s on the Notifications page with %s, then import again to add its rules.", c.Kind, c.Name, what)
		case cur.Kind != c.Kind:
			delete(im.channels, c.Name)
			im.item("notificationChannels", c.Name, actSkip, "a "+cur.Kind+" channel has this name here", nil)
			im.attention("notificationChannels", c.Name, "review", "/admin/notifications", "Channel %s is %s here and %s in the file; a channel's kind never changes.", c.Name, cur.Kind, c.Kind)
		case cur.URL == c.URL && slices.Equal(cur.Recipients, c.Recipients) && cur.AllowPrivateNetwork == c.AllowPrivateNetwork && cur.Enabled == c.Enabled:
			im.item("notificationChannels", c.Name, actUnchanged, "same; its secret stays as it is", nil)
		default:
			im.item("notificationChannels", c.Name, actUpdate, "settings from the file; its secret stays as it is", func() error {
				if _, err := im.a.notify.UpdateChannel(im.ctx, im.p, cur.ID, notify.ChannelChange{Name: c.Name, URL: c.URL, Recipients: c.Recipients, AllowPrivateNetwork: c.AllowPrivateNetwork}, im.reqID); err != nil {
					return err
				}
				if cur.Enabled != c.Enabled {
					return im.a.notify.SetEnabled(im.ctx, im.p, cur.ID, c.Enabled, im.reqID)
				}
				return nil
			})
		}
	}
	return nil
}

// importRules adds rules that do not exist yet; a rule has no name, so one with the same channel, scope and events
// is the same rule. A rule for a project that does not exist here is skipped: without its scope it would cover the
// whole organization.
func (im *importer) importRules(s configSpec) error {
	if len(s.NotificationRules) == 0 {
		return nil
	}
	existing, err := im.a.notify.Rules(im.ctx, im.p)
	if err != nil {
		return err
	}
	key := func(channel, team, project string, events []string) string {
		return channel + "\x00" + team + "\x00" + project + "\x00" + strings.Join(slices.Sorted(slices.Values(events)), ",")
	}
	have := map[string]bool{}
	for _, r := range existing {
		have[key(r.ChannelID, r.TeamID, r.ProjectID, r.Events)] = true
	}
	for _, c := range s.NotificationRules {
		name := c.Channel + ": " + strings.Join(c.Events, ", ")
		if c.Project != "" {
			name += " for " + c.Project
		} else if c.Team != "" {
			name += " for team " + c.Team
		}
		channel, ok := im.channels[c.Channel]
		team, teamOK := im.teams[c.Team]
		project, projectOK := im.projects[c.Project]
		unknown := slices.DeleteFunc(slices.Clone(c.Events), func(e string) bool { return slices.Contains(notify.Types, e) })
		switch {
		case !ok:
			im.item("notificationRules", name, actSkip, "channel "+c.Channel+" is not here yet", nil)
			im.attention("notificationRules", name, "reference", "/admin/notifications", "This rule waits for channel %s; add the channel and import again.", c.Channel)
			continue
		case c.Team != "" && !teamOK || c.Project != "" && !projectOK:
			im.item("notificationRules", name, actSkip, "its team or project does not exist here", nil)
			im.attention("notificationRules", name, "reference", "/admin/notifications", "This rule is for %s, which does not exist here; without it the rule would cover the whole organization.", cmp.Or(c.Project, "team "+c.Team))
			continue
		case len(unknown) > 0:
			im.item("notificationRules", name, actSkip, "unknown events: "+strings.Join(unknown, ", "), nil)
			continue
		}
		if c.Team == "" {
			team = ""
		}
		if c.Project == "" {
			project = ""
		}
		if have[key(channel, team, project, c.Events)] {
			im.item("notificationRules", name, actUnchanged, "exists", nil)
			continue
		}
		im.item("notificationRules", name, actCreate, "new rule", func() error {
			_, err := im.a.notify.CreateRule(im.ctx, im.p, notify.Rule{ChannelID: channel, Events: c.Events, TeamID: team, ProjectID: project}, im.reqID)
			return err
		})
	}
	return nil
}
