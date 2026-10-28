package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/admission"
	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
)

type revisionInput struct {
	Document   json.RawMessage `json:"document"`
	EditorMode string          `json:"editorMode"`
	Note       string          `json:"note"`
}

type revisionView struct {
	SpecID     string            `json:"specId"`
	Revision   int               `json:"revision"`
	Digest     string            `json:"digest"`
	Document   json.RawMessage   `json:"document"`
	EditorMode string            `json:"editorMode,omitempty"`
	CreatedBy  *apigen.Principal `json:"createdBy,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	Warnings   []specWarning     `json:"warnings,omitempty"`
}

func toRevision(r specs.Revision) revisionView {
	return revisionView{SpecID: r.SpecID, Revision: r.Revision, Digest: r.Digest, Document: r.Document, EditorMode: r.EditorMode,
		CreatedBy: &apigen.Principal{Id: r.CreatedBy, Type: apigen.PrincipalTypeUser}, CreatedAt: r.CreatedAt}
}

func toSpec(s specs.Spec) apigen.BuildSpec {
	return apigen.BuildSpec{Id: s.ID, ProjectId: s.ProjectID, Name: s.Name, LatestRevision: s.LatestRevision, CreatedAt: &s.CreatedAt}
}

func (a *API) ListSpecs(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.ListSpecsParams) {
	limit, cursor := pageParams(params.Limit, params.Cursor)
	name := ""
	if params.Name != nil {
		name = *params.Name
	}
	list, err := a.specs.List(r.Context(), principal(r), operationRoles(r.Context()), projectID, name, cursor, limit+1)
	if err != nil {
		a.specsError(w, r, err)
		return
	}
	out := apigen.BuildSpecList{Items: make([]apigen.BuildSpec, 0, len(list))}
	if len(list) > limit {
		list = list[:limit]
		next := list[limit-1].ID
		out.NextCursor = &next
	}
	for _, s := range list {
		out.Items = append(out.Items, toSpec(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateSpec(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, _ apigen.CreateSpecParams) {
	var in revisionInput
	if !decodeBody(w, r, &in) {
		return
	}
	_, rev, err := a.specs.Create(r.Context(), principal(r), operationRoles(r.Context()), projectID, in.Document, in.EditorMode, in.Note, requestID(w))
	if err != nil {
		a.specsError(w, r, err)
		return
	}
	w.Header().Set("ETag", specs.ETag(rev.Revision))
	v := toRevision(rev)
	v.Warnings = a.tagWarnings(r.Context(), projectID, rev.Document)
	writeJSON(w, http.StatusCreated, v)
}

func (a *API) GetSpec(w http.ResponseWriter, r *http.Request, id apigen.SpecId) {
	s, err := a.specs.Get(r.Context(), principal(r), operationRoles(r.Context()), id)
	if err != nil {
		a.specsError(w, r, err)
		return
	}
	w.Header().Set("ETag", specs.ETag(s.LatestRevision))
	writeJSON(w, http.StatusOK, toSpec(s))
}

func (a *API) ListSpecRevisions(w http.ResponseWriter, r *http.Request, id apigen.SpecId, params apigen.ListSpecRevisionsParams) {
	limit, cursor := pageParams(params.Limit, params.Cursor)
	after, _ := strconv.Atoi(cursor)
	revs, err := a.specs.Revisions(r.Context(), principal(r), operationRoles(r.Context()), id, after, limit+1)
	if err != nil {
		a.specsError(w, r, err)
		return
	}
	out := struct {
		Items      []revisionView `json:"items"`
		NextCursor *string        `json:"nextCursor,omitempty"`
	}{Items: make([]revisionView, 0, len(revs))}
	if len(revs) > limit {
		revs = revs[:limit]
		next := strconv.Itoa(revs[limit-1].Revision)
		out.NextCursor = &next
	}
	for _, rev := range revs {
		out.Items = append(out.Items, toRevision(rev))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateSpecRevision(w http.ResponseWriter, r *http.Request, id apigen.SpecId, params apigen.CreateSpecRevisionParams) {
	var in revisionInput
	if !decodeBody(w, r, &in) {
		return
	}
	rev, err := a.specs.AddRevision(r.Context(), principal(r), operationRoles(r.Context()), id, params.IfMatch, in.Document, in.EditorMode, in.Note, requestID(w))
	if err != nil {
		a.specsError(w, r, err)
		return
	}
	w.Header().Set("ETag", specs.ETag(rev.Revision))
	v := toRevision(rev)
	if sp, err := a.specs.Get(r.Context(), principal(r), operationRoles(r.Context()), id); err == nil {
		v.Warnings = a.tagWarnings(r.Context(), sp.ProjectID, rev.Document)
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *API) GetSpecRevision(w http.ResponseWriter, r *http.Request, id apigen.SpecId, n apigen.Revision) {
	_, rev, err := a.specs.Revision(r.Context(), principal(r), operationRoles(r.Context()), id, n)
	if err != nil {
		a.specsError(w, r, err)
		return
	}
	w.Header().Set("ETag", specs.ETag(rev.Revision))
	writeJSON(w, http.StatusOK, toRevision(rev))
}

func (a *API) CheckAdmission(w http.ResponseWriter, r *http.Request, id apigen.SpecId, n apigen.Revision, _ apigen.CheckAdmissionParams) {
	var in apigen.AdmissionCheckRequest
	if !decodeBody(w, r, &in) {
		return
	}
	rep, err := a.admission.Check(r.Context(), principal(r), operationRoles(r.Context()), id, n, in.SnapshotId)
	if err != nil {
		a.specsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, admissionView(rep))
}

type issueView struct {
	Field  string `json:"field"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

func issues(list []buildspec.Issue) []issueView {
	out := make([]issueView, 0, len(list))
	for _, i := range list {
		out = append(out, issueView{Field: i.Path, Code: i.Rule, Reason: i.Message})
	}
	return out
}

func admissionView(r admission.Report) any {
	return struct {
		admission.Report
		SchemaErrors   []issueView `json:"schemaErrors"`
		SemanticErrors []issueView `json:"semanticErrors"`
	}{r, issues(r.SchemaErrors), issues(r.SemanticErrors)}
}

func (a *API) specsError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *specs.Invalid
	switch {
	case errors.As(err, &invalid):
		details := make([]apigen.ProblemDetail, 0)
		for _, i := range append(invalid.Result.SchemaErrors, invalid.Result.SemanticErrors...) {
			details = append(details, apigen.ProblemDetail{Field: ptr(i.Path), Code: ptr(i.Rule), Reason: ptr(i.Message)})
		}
		writeJSON(w, http.StatusUnprocessableEntity, apigen.Problem{Code: "invalid_spec", Message: "the BuildSpec is not valid; see details", RequestId: requestID(w), Details: &details})
	case errors.Is(err, specs.ErrPrecondition):
		apigen.WriteProblem(w, http.StatusPreconditionFailed, "stale_revision", "the spec changed since you loaded it; reload, review the latest revision and reapply your change")
	case errors.Is(err, specs.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "spec_exists", err.Error())
	case errors.Is(err, specs.ErrNotFound), errors.Is(err, sources.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	default:
		a.tenancyError(w, r, err)
	}
}

func (a *API) ListRecipes(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	all, statuses, err := a.recipes.All(r.Context(), p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := apigen.RecipeList{Items: []apigen.Recipe{}}
	for _, rc := range all {
		st := statuses[rc.ID+"@"+rc.Version]
		if st.Status == recipes.Withdrawn && !p.IsOrgAdmin() {
			continue
		}
		list := func(l []string) []string { return append([]string{}, l...) }
		item := apigen.Recipe{Id: rc.ID, Version: rc.Version, DisplayName: rc.DisplayName, Description: rc.Description, Languages: list(rc.Languages),
			ApplicationTypes: list(rc.ApplicationTypes), Platforms: list(rc.Platforms), Status: apigen.RecipeStatus(st.Status), Runtimes: []apigen.RecipeRuntime{}}
		if st.Reason != "" {
			item.StatusReason = ptr(st.Reason)
		}
		for _, rt := range rc.Listed(p.IsOrgAdmin()) {
			item.Runtimes = append(item.Runtimes, toRecipeRuntime(rt, p.IsOrgAdmin()))
		}
		defaults := map[string]string{}
		if rc.Defaults.Install != "" {
			defaults["install"] = rc.Defaults.Install
		}
		if rc.Defaults.Build != "" {
			defaults["build"] = rc.Defaults.Build
		}
		item.Defaults = &defaults
		if d := rc.Custom; d != nil {
			custom, secrets := true, append([]string{}, d.Secrets...)
			params := make([]apigen.RecipeParameter, 0, len(d.Parameters))
			for _, p := range d.Parameters {
				rp := apigen.RecipeParameter{Name: p.Name, Type: apigen.RecipeParameterType(p.Type), Required: p.Required}
				if p.Description != "" {
					rp.Description = ptr(p.Description)
				}
				if p.Default != "" {
					rp.Default = ptr(p.Default)
				}
				if len(p.Enum) > 0 {
					enum := append([]string{}, p.Enum...)
					rp.Enum = &enum
				}
				params = append(params, rp)
			}
			item.Custom, item.Secrets, item.Parameters = &custom, &secrets, &params
		}
		out.Items = append(out.Items, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) SetRecipeStatus(w http.ResponseWriter, r *http.Request, id, version string, _ apigen.SetRecipeStatusParams) {
	var in apigen.RecipeStatusWrite
	if !decodeBody(w, r, &in) {
		return
	}
	err := a.recipes.Set(r.Context(), principal(r), id, version, string(in.Status), in.Reason, requestID(w))
	switch {
	case errors.Is(err, recipes.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, recipes.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators manage recipes")
	case err != nil:
		a.internalError(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func toPolicy(p policy.Policy) apigen.Policy {
	var doc map[string]any
	_ = json.Unmarshal(p.Document, &doc)
	return apigen.Policy{Id: p.ID, Kind: apigen.PolicyKind(p.Kind), Version: p.Version, Document: doc, UpdatedAt: &p.CreatedAt}
}

func (a *API) ListPolicies(w http.ResponseWriter, r *http.Request) {
	list, err := a.policies.List(r.Context(), principal(r).OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := apigen.PolicyList{Items: make([]apigen.Policy, 0, len(list))}
	for _, p := range list {
		out.Items = append(out.Items, toPolicy(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// policyChange validates a proposed document against the current policy and measures what it would newly block.
func (a *API) policyChange(r *http.Request, id string, in apigen.PolicyWrite) (policy.Policy, []byte, []string, int, error) {
	p := principal(r)
	current, err := a.policies.ByID(r.Context(), p.OrgID, id)
	if err != nil {
		return policy.Policy{}, nil, nil, 0, err
	}
	if string(in.Kind) != current.Kind {
		return policy.Policy{}, nil, nil, 0, &policy.Invalid{Reason: "this policy is of kind " + current.Kind}
	}
	raw, _ := json.Marshal(in.Document)
	doc, err := policy.Validate(current.Kind, raw)
	if err != nil {
		return policy.Policy{}, nil, nil, 0, err
	}
	blocked, projects, err := a.admission.Impact(r.Context(), p.OrgID, current.Kind, doc, a.latestSnapshots(r, p))
	return current, doc, blocked, projects, err
}

const confirmTTL = 15 * time.Minute

// confirmation binds a dry run to the organization, policy, version and exact document it previewed (TM-92).
func (a *API) confirmation(orgID, policyID string, version int, doc []byte, exp time.Time) string {
	sum := sha256.Sum256(doc)
	mac := hmac.New(sha256.New, a.confirmKey)
	fmt.Fprintf(mac, "%s\x00%s\x00%d\x00%x\x00%d", orgID, policyID, version, sum, exp.Unix())
	return strconv.FormatInt(exp.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *API) confirmed(token, orgID, policyID string, version int, doc []byte) bool {
	ts, _, ok := strings.Cut(token, ".")
	unix, err := strconv.ParseInt(ts, 10, 64)
	if !ok || err != nil || time.Now().Unix() > unix {
		return false
	}
	return hmac.Equal([]byte(token), []byte(a.confirmation(orgID, policyID, version, doc, time.Unix(unix, 0))))
}

func (a *API) ReplacePolicy(w http.ResponseWriter, r *http.Request, id apigen.Id, params apigen.ReplacePolicyParams) {
	var in apigen.PolicyWrite
	if !decodeBody(w, r, &in) {
		return
	}
	version, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(params.IfMatch, "W/"), `"`))
	if err != nil {
		apigen.WriteProblem(w, http.StatusPreconditionFailed, "stale_policy", "If-Match must be the policy version you read")
		return
	}
	p := principal(r)
	if !p.IsOrgAdmin() {
		a.policyError(w, r, policy.ErrForbidden)
		return
	}
	current, doc, blocked, projects, err := a.policyChange(r, id, in)
	if err != nil {
		a.policyError(w, r, err)
		return
	}
	reason := ""
	if in.Reason != nil {
		reason = strings.TrimSpace(*in.Reason)
	}
	if len(blocked) > 0 {
		if params.XSamuoriConfirm == nil || version != current.Version || !a.confirmed(*params.XSamuoriConfirm, p.OrgID, id, current.Version, doc) {
			apigen.WriteProblem(w, http.StatusPreconditionRequired, "confirmation_required", fmt.Sprintf("this change would block %d spec(s) in %d project(s); preview it with a dry run and confirm it", len(blocked), projects))
			return
		}
		if len(reason) < 10 {
			apigen.WriteProblem(w, http.StatusUnprocessableEntity, "reason_required", "a change that blocks specs needs a reason of at least 10 characters")
			return
		}
	}
	saved, err := a.policies.Replace(r.Context(), p, id, string(in.Kind), version, doc, reason, requestID(w))
	if err != nil {
		a.policyError(w, r, err)
		return
	}
	w.Header().Set("ETag", `"`+strconv.Itoa(saved.Version)+`"`)
	writeJSON(w, http.StatusOK, toPolicy(saved))
}

func (a *API) DryRunPolicy(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DryRunPolicyParams) {
	var in apigen.PolicyWrite
	if !decodeBody(w, r, &in) {
		return
	}
	// The impact covers every spec in the organization, so a team or project administrator must not see it.
	if !principal(r).IsOrgAdmin() {
		a.policyError(w, r, policy.ErrForbidden)
		return
	}
	current, doc, blocked, projects, err := a.policyChange(r, id, in)
	if err != nil {
		a.policyError(w, r, err)
		return
	}
	summary := "No existing spec would be newly blocked."
	out := apigen.PolicyImpact{AffectedProjects: &projects, NewlyBlocked: &blocked, Summary: &summary}
	if len(blocked) > 0 {
		summary = strconv.Itoa(len(blocked)) + " spec(s) in " + strconv.Itoa(projects) + " project(s) would be blocked. Dockerfile specs are checked against each project's newest upload."
		token := a.confirmation(principal(r).OrgID, id, current.Version, doc, time.Now().Add(confirmTTL))
		out.Confirmation = &token
	}
	writeJSON(w, http.StatusOK, out)
}

// latestSnapshots looks up each project's newest upload once, as an administrator sees it.
func (a *API) latestSnapshots(r *http.Request, p authz.Principal) func(string) *sources.Snapshot {
	cache := map[string]*sources.Snapshot{}
	roles := []authz.Role{authz.Administrator}
	return func(projectID string) *sources.Snapshot {
		if s, ok := cache[projectID]; ok {
			return s
		}
		var snap *sources.Snapshot
		if list, err := a.sources.ListSnapshots(r.Context(), p, roles, projectID, "", 1); err == nil && len(list) == 1 {
			snap = &list[0]
		}
		cache[projectID] = snap
		return snap
	}
}

func (a *API) policyError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *policy.Invalid
	switch {
	case errors.As(err, &invalid):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_policy", invalid.Reason)
	case errors.Is(err, policy.ErrUnsupported):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "unsupported_policy", "this policy kind cannot be edited yet")
	case errors.Is(err, policy.ErrConflict):
		apigen.WriteProblem(w, http.StatusPreconditionFailed, "stale_policy", "the policy changed since you loaded it; reload and reapply your change")
	case errors.Is(err, policy.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, policy.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators change policies")
	default:
		a.internalError(w, r, err)
	}
}

func pageParams(limit *int, cursor *string) (int, string) {
	l, c := 50, ""
	if limit != nil {
		l = *limit
	}
	if cursor != nil {
		c = *cursor
	}
	return l, c
}

func ptr[T any](v T) *T { return &v }
