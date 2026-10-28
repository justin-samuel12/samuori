package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"samuori.invalid/samuori/internal/admission"
	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/discovery"
	"samuori.invalid/samuori/internal/discovery/jobs"
	"samuori.invalid/samuori/internal/discovery/sandbox"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/specs"
)

func (a *API) StartDiscovery(w http.ResponseWriter, r *http.Request, snapshotID apigen.SnapshotId, _ apigen.StartDiscoveryParams) {
	op, err := a.discovery.Start(r.Context(), principal(r), operationRoles(r.Context()), snapshotID, requestID(w))
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, toOperation(op, requestID(w)))
}

// discoveryView mirrors the Discovery schema; detector results keep their contract field names.
type discoveryView struct {
	ID          string                         `json:"id"`
	ProjectID   string                         `json:"projectId"`
	SnapshotID  string                         `json:"snapshotId"`
	Status      string                         `json:"status"`
	Findings    []discovery.Finding            `json:"findings"`
	Dockerfiles []discovery.DockerfileAnalysis `json:"dockerfiles"`
	Warnings    []discovery.Warning            `json:"warnings"`
	Sandbox     *sandbox.Sandbox               `json:"sandbox,omitempty"`
	Error       *apigen.Problem                `json:"error,omitempty"`
	CreatedAt   time.Time                      `json:"createdAt"`
}

func (a *API) ListSnapshotDiscoveries(w http.ResponseWriter, r *http.Request, snapshotID apigen.SnapshotId, params apigen.ListSnapshotDiscoveriesParams) {
	list, err := a.discovery.List(r.Context(), principal(r), operationRoles(r.Context()), snapshotID)
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	out := struct {
		Items      []discoveryView `json:"items"`
		NextCursor *string         `json:"nextCursor,omitempty"`
	}{Items: make([]discoveryView, 0, len(list))}
	for _, d := range list {
		out.Items = append(out.Items, view(d, requestID(w), false))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x discoveryView) string { return x.ID }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetDiscovery(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	d, err := a.discovery.Get(r.Context(), principal(r), operationRoles(r.Context()), id)
	if errors.Is(err, jobs.ErrNotFound) {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if d.Status == jobs.Completed {
		eff, err := a.policies.Effective(r.Context(), principal(r).OrgID)
		var project []egress.Allowance
		if err == nil {
			project, err = a.approvals.ProjectAllowances(r.Context(), principal(r).OrgID, d.ProjectID)
		}
		if err == nil {
			err = admission.DiscoveryReadiness(&d.Result, eff.Egress, project)
		}
		if err != nil {
			a.internalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, view(d, requestID(w), true))
}

func view(d jobs.Discovery, reqID string, full bool) discoveryView {
	v := discoveryView{ID: d.ID, ProjectID: d.ProjectID, SnapshotID: d.SnapshotID, Status: d.Status, CreatedAt: d.CreatedAt}
	if full {
		v.Findings, v.Dockerfiles, v.Warnings = d.Result.Findings, d.Result.Dockerfiles, d.Result.Warnings
	}
	if v.Findings == nil {
		v.Findings = []discovery.Finding{}
	}
	if v.Dockerfiles == nil {
		v.Dockerfiles = []discovery.DockerfileAnalysis{}
	}
	if v.Warnings == nil {
		v.Warnings = []discovery.Warning{}
	}
	if full && d.Status != jobs.Running {
		v.Sandbox = &d.Sandbox
	}
	if d.ErrorCode != "" {
		v.Error = &apigen.Problem{Code: d.ErrorCode, Message: d.ErrorMessage, RequestId: reqID}
	}
	return v
}

var specName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ListSpecSuggestions derives starting specs from the snapshot's newest completed discovery
// (docs/architecture/buildspec.md#suggested-specs). It only reads stored results and saves nothing.
func (a *API) ListSpecSuggestions(w http.ResponseWriter, r *http.Request, snapshotID apigen.SnapshotId, params apigen.ListSpecSuggestionsParams) {
	ctx, p, roles := r.Context(), principal(r), operationRoles(r.Context())
	name := ""
	if params.Name != nil {
		if name = *params.Name; !specName.MatchString(name) {
			apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "name must be lowercase letters, digits and -, at most 63 characters")
			return
		}
	}
	list, err := a.discovery.List(ctx, p, roles, snapshotID)
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	id, running := "", false
	for _, d := range list {
		if d.Status == jobs.Completed {
			id = d.ID
			break
		}
		running = running || d.Status == jobs.Running
	}
	if id == "" {
		if running {
			apigen.WriteProblem(w, http.StatusConflict, "discovery_running", "discovery of this snapshot is still running; ask again when it finishes")
		} else {
			apigen.WriteProblem(w, http.StatusConflict, "discovery_required", "this snapshot has no completed discovery; start one with POST /snapshots/"+snapshotID+"/discoveries")
		}
		return
	}
	d, err := a.discovery.Get(ctx, p, roles, id)
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	project, err := a.tenancy.GetProject(ctx, p, roles, d.ProjectID)
	if err != nil {
		a.tenancyError(w, r, err)
		return
	}
	eff, err := a.policies.Effective(ctx, p.OrgID)
	var allowances []egress.Allowance
	if err == nil {
		allowances, err = a.approvals.ProjectAllowances(ctx, p.OrgID, d.ProjectID)
	}
	if err == nil {
		err = admission.DiscoveryReadiness(&d.Result, eff.Egress, allowances)
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	all, statuses, err := a.recipes.All(ctx, p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	choices := make([]specs.RecipeChoice, 0, len(all))
	for _, rc := range all {
		choices = append(choices, specs.RecipeChoice{ID: rc.ID, Version: rc.Version, Languages: rc.Languages, ApplicationTypes: rc.ApplicationTypes,
			Approved: statuses[rc.ID+"@"+rc.Version].Status == recipes.Approved})
	}
	pol, err := a.providers.Policy(ctx, p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	allowed := map[string]bool{}
	for _, m := range providers.Methods {
		err := a.providers.MethodAllowed(ctx, p.OrgID, m, "", d.ProjectID)
		var refused *providers.Invalid
		if err != nil && !errors.As(err, &refused) {
			a.internalError(w, r, err)
			return
		}
		allowed[m] = err == nil
	}
	items, notes := specs.Suggest(specs.SuggestInput{Project: project.Name, Name: name, Result: d.Result, Recipes: choices, Default: pol.Default,
		Allowed: func(m string) bool { return allowed[m] }})

	type suggestionView struct {
		Method         string              `json:"method"`
		Title          string              `json:"title"`
		SourceSubpath  string              `json:"sourceSubpath"`
		Recommended    bool                `json:"recommended"`
		Reasons        []string            `json:"reasons"`
		Warnings       []discovery.Warning `json:"warnings"`
		Valid          bool                `json:"valid"`
		Document       json.RawMessage     `json:"document"`
		CanonicalYAML  string              `json:"canonicalYaml"`
		Digest         string              `json:"digest,omitempty"`
		SchemaErrors   []issueView         `json:"schemaErrors"`
		SemanticErrors []issueView         `json:"semanticErrors"`
	}
	out := struct {
		DiscoveryID string              `json:"discoveryId"`
		Items       []suggestionView    `json:"items"`
		Warnings    []discovery.Warning `json:"warnings"`
	}{DiscoveryID: d.ID, Items: []suggestionView{}, Warnings: append([]discovery.Warning{}, notes...)}
	for _, s := range items {
		raw, err := json.Marshal(s.Document)
		if err == nil {
			var rd specs.Rendering
			if rd, err = a.specs.Render(ctx, p.OrgID, raw); err == nil {
				out.Items = append(out.Items, suggestionView{s.Method, s.Title, s.SourceSubpath, s.Recommended, append([]string{}, s.Reasons...),
					append([]discovery.Warning{}, s.Warnings...), rd.Valid, rd.Document, rd.YAML, rd.Digest, issues(rd.Schema), issues(rd.Semantics)})
			}
		}
		if err != nil {
			a.internalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}
