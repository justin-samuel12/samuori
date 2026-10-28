package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/pipelines"
)

func (a *API) pipelineError(w http.ResponseWriter, r *http.Request, err error) {
	var inv *pipelines.Invalid
	switch {
	case errors.Is(err, pipelines.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "your roles do not permit this")
	case errors.Is(err, pipelines.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, pipelines.ErrActive):
		apigen.WriteProblem(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, pipelines.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", "the project already has a pipeline with that name")
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", inv.Reason)
	default:
		a.internalError(w, r, err)
	}
}

type pipelineView struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	ProjectID           string    `json:"projectId"`
	SpecID              string    `json:"specId"`
	SpecRevision        int       `json:"specRevision"`
	RepoURL             string    `json:"repoUrl"`
	Refs                []string  `json:"refs"`
	Username            string    `json:"username,omitempty"`
	HasToken            bool      `json:"hasToken"`
	AllowPrivateNetwork bool      `json:"allowPrivateNetwork"`
	HookScheme          string    `json:"hookScheme"`
	OwnerID             string    `json:"ownerId"`
	Enabled             bool      `json:"enabled"`
	HookPath            string    `json:"hookPath"`
	CreatedAt           time.Time `json:"createdAt"`
	Secret              string    `json:"secret,omitempty"`
}

func toPipeline(p pipelines.Pipeline) pipelineView {
	return pipelineView{ID: p.ID, Name: p.Name, ProjectID: p.ProjectID, SpecID: p.SpecID, SpecRevision: p.SpecRevision, RepoURL: p.RepoURL, Refs: p.Refs, Username: p.Username,
		HasToken: p.HasToken, AllowPrivateNetwork: p.AllowPrivateNetwork, HookScheme: p.HookScheme, OwnerID: p.OwnerID, Enabled: p.Enabled, HookPath: "/api/v1/hooks/pipelines/" + p.ID, CreatedAt: p.CreatedAt}
}

type pipelineRunView struct {
	ID              string    `json:"id"`
	PipelineID      string    `json:"pipelineId"`
	Trigger         string    `json:"trigger"`
	Ref             string    `json:"ref"`
	RequestedCommit string    `json:"requestedCommit,omitempty"`
	Commit          string    `json:"commit,omitempty"`
	State           string    `json:"state"`
	FailureCode     string    `json:"failureCode,omitempty"`
	Message         string    `json:"message,omitempty"`
	SkippedEntries  int       `json:"skippedEntries"`
	SnapshotID      string    `json:"snapshotId,omitempty"`
	BuildRunID      string    `json:"buildRunId,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func toPipelineRun(r pipelines.Run) pipelineRunView {
	return pipelineRunView{ID: r.ID, PipelineID: r.PipelineID, Trigger: r.Trigger, Ref: r.Ref, RequestedCommit: r.RequestedCommit, Commit: r.Commit, State: r.State, FailureCode: r.FailureCode,
		Message: r.FailureMessage, SkippedEntries: r.SkippedEntries, SnapshotID: r.SnapshotID, BuildRunID: r.BuildRunID, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (a *API) ListPipelines(w http.ResponseWriter, r *http.Request, params apigen.ListPipelinesParams) {
	project := ""
	if params.ProjectId != nil {
		project = *params.ProjectId
	}
	list, err := a.pipelines.List(r.Context(), principal(r), project)
	if err != nil {
		a.pipelineError(w, r, err)
		return
	}
	out := struct {
		Items []pipelineView `json:"items"`
	}{Items: []pipelineView{}}
	for _, p := range list {
		out.Items = append(out.Items, toPipeline(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreatePipeline(w http.ResponseWriter, r *http.Request, _ apigen.CreatePipelineParams) {
	var in struct {
		Name                string   `json:"name"`
		ProjectID           string   `json:"projectId"`
		SpecID              string   `json:"specId"`
		SpecRevision        int      `json:"specRevision"`
		RepoURL             string   `json:"repoUrl"`
		Refs                []string `json:"refs"`
		Username            string   `json:"username"`
		Token               string   `json:"token"`
		AllowPrivateNetwork bool     `json:"allowPrivateNetwork"`
		HookScheme          string   `json:"hookScheme"`
		OwnerID             string   `json:"ownerId"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	p, secret, err := a.pipelines.Create(r.Context(), principal(r), pipelines.Pipeline{Name: in.Name, ProjectID: in.ProjectID, SpecID: in.SpecID, SpecRevision: in.SpecRevision, RepoURL: in.RepoURL,
		Refs: in.Refs, Username: in.Username, AllowPrivateNetwork: in.AllowPrivateNetwork, HookScheme: in.HookScheme, OwnerID: in.OwnerID}, in.Token, requestID(w))
	if err != nil {
		a.pipelineError(w, r, err)
		return
	}
	v := toPipeline(p)
	v.Secret = secret
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, v)
}

func (a *API) GetPipeline(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	p, err := a.pipelines.Get(r.Context(), principal(r), id)
	if err != nil {
		a.pipelineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPipeline(p))
}

// UpdatePipeline changes a pipeline's settings; the webhook URL and secret stay the same and the token is never returned.
func (a *API) UpdatePipeline(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdatePipelineParams) {
	var in apigen.PipelineUpdate
	if !decodeBody(w, r, &in) {
		return
	}
	c := pipelines.Changes{Name: in.Name, RepoURL: in.RepoUrl, Username: in.Username, Token: in.Token, SpecRevision: in.SpecRevision}
	if in.Refs != nil {
		c.Refs = *in.Refs
	}
	p, err := a.pipelines.Update(r.Context(), principal(r), id, c, requestID(w))
	if err != nil {
		a.pipelineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPipeline(p))
}

func (a *API) DeletePipeline(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeletePipelineParams) {
	if err := a.pipelines.Delete(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.pipelineError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) SetPipelineState(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.SetPipelineStateParams) {
	var in apigen.EnabledState
	if !decodeBody(w, r, &in) {
		return
	}
	if err := a.pipelines.SetEnabled(r.Context(), principal(r), id, in.Enabled, requestID(w)); err != nil {
		a.pipelineError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) RotatePipelineHookSecret(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RotatePipelineHookSecretParams) {
	secret, err := a.pipelines.RotateHookSecret(r.Context(), principal(r), id, requestID(w))
	if err != nil {
		a.pipelineError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret})
}

func (a *API) ListPipelineRuns(w http.ResponseWriter, r *http.Request, id apigen.Id, params apigen.ListPipelineRunsParams) {
	limit, _ := pageParams(params.Limit, nil)
	list, err := a.pipelines.Runs(r.Context(), principal(r), id, limit)
	if err != nil {
		a.pipelineError(w, r, err)
		return
	}
	out := struct {
		Items []pipelineRunView `json:"items"`
	}{Items: []pipelineRunView{}}
	for _, x := range list {
		out.Items = append(out.Items, toPipelineRun(x))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) StartPipelineRun(w http.ResponseWriter, r *http.Request, id apigen.Id, params apigen.StartPipelineRunParams) {
	var in struct {
		Ref string `json:"ref"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	trigger := "manual"
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		trigger = "api"
	}
	run, err := a.pipelines.Start(r.Context(), principal(r), id, in.Ref, trigger, params.IdempotencyKey)
	if err != nil {
		a.pipelineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toPipelineRun(run))
}

// ReceivePipelineHook answers before reading the payload when verification fails; see pipelines.Service.Hook.
func (a *API) ReceivePipelineHook(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	if ok, retry := a.hookLimit.allow("hook:" + id); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		apigen.WriteProblem(w, http.StatusTooManyRequests, "rate_limited", "too many deliveries; retry later")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, pipelines.MaxHookBody+1))
	if err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "the body could not be read")
		return
	}
	res, err := a.pipelines.Hook(r.Context(), id, r.Header, body)
	switch {
	case errors.Is(err, pipelines.ErrUnverified):
		apigen.WriteProblem(w, http.StatusUnauthorized, "unverified", "the delivery could not be verified")
		return
	case errors.Is(err, pipelines.ErrBusy):
		w.Header().Set("Retry-After", "60")
		apigen.WriteProblem(w, http.StatusTooManyRequests, "busy", "the pipeline has too many queued runs")
		return
	case err != nil:
		a.internalError(w, r, err)
		return
	}
	out := map[string]string{"status": res.Status}
	if res.RunID != "" {
		out["runId"] = res.RunID
	}
	if res.Reason != "" {
		out["reason"] = res.Reason
	}
	writeJSON(w, http.StatusAccepted, out)
}
