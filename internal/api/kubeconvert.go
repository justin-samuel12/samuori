package api

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/kubeconvert"
)

func (a *API) kubeError(w http.ResponseWriter, r *http.Request, err error) {
	var inv *kubeconvert.Invalid
	switch {
	case errors.Is(err, kubeconvert.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "your roles on this project do not permit this")
	case errors.Is(err, kubeconvert.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, kubeconvert.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", "a run of this conversion is still in progress")
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", inv.Reason)
	default:
		a.internalError(w, r, err)
	}
}

type kubeRunView struct {
	ID              string                 `json:"id"`
	Number          int                    `json:"number"`
	State           string                 `json:"state"`
	ProviderVersion string                 `json:"providerVersion"`
	Options         kubeconvert.Options    `json:"options"`
	Answers         map[string]string      `json:"answers,omitempty"`
	FailureClass    string                 `json:"failureClass,omitempty"`
	Detail          string                 `json:"detail,omitempty"`
	Questions       []kubeconvert.Question `json:"questions,omitempty"`
	Revision        int                    `json:"revision,omitempty"`
	ToolDigest      string                 `json:"toolDigest,omitempty"`
	OperationID     string                 `json:"operationId,omitempty"`
	CreatedAt       time.Time              `json:"createdAt"`
	UpdatedAt       time.Time              `json:"updatedAt"`
	EndedAt         *time.Time             `json:"endedAt,omitempty"`
}

func toKubeRun(r kubeconvert.Run) kubeRunView {
	return kubeRunView{ID: r.ID, Number: r.Number, State: r.State, ProviderVersion: r.ProviderVersion, Options: r.Options, Answers: r.Answers, FailureClass: r.FailureClass, Detail: r.Detail,
		Questions: r.Questions, Revision: r.Revision, ToolDigest: r.ToolDigest, OperationID: r.OperationID, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, EndedAt: r.EndedAt}
}

type kubeConversionView struct {
	ID              string              `json:"id"`
	ProjectID       string              `json:"projectId"`
	Name            string              `json:"name"`
	Provider        string              `json:"provider"`
	Input           kubeconvert.Input   `json:"input"`
	Options         kubeconvert.Options `json:"options"`
	State           string              `json:"state"`
	CurrentRevision int                 `json:"currentRevision"`
	LatestRun       *kubeRunView        `json:"latestRun,omitempty"`
	CreatedBy       string              `json:"createdBy"`
	CreatedAt       time.Time           `json:"createdAt"`
	UpdatedAt       time.Time           `json:"updatedAt"`
}

func toKubeConversion(c kubeconvert.Conversion) kubeConversionView {
	v := kubeConversionView{ID: c.ID, ProjectID: c.ProjectID, Name: c.Name, Provider: c.Provider, Input: c.Input, Options: c.Options, State: c.State, CurrentRevision: c.CurrentRevision,
		CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if c.LatestRun != nil {
		r := toKubeRun(*c.LatestRun)
		v.LatestRun = &r
	}
	return v
}

type kubeFileInfo struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int    `json:"size"`
}

type kubeRevisionView struct {
	Number    int                   `json:"number"`
	RunID     string                `json:"runId,omitempty"`
	Parent    int                   `json:"parent,omitempty"`
	Origin    string                `json:"origin"`
	Errors    int                   `json:"errors"`
	Findings  []kubeconvert.Finding `json:"findings"`
	Lineage   kubeconvert.Lineage   `json:"lineage"`
	Files     []kubeFileInfo        `json:"files"`
	CreatedBy string                `json:"createdBy"`
	CreatedAt time.Time             `json:"createdAt"`
}

func toKubeRevision(rev kubeconvert.Revision) kubeRevisionView {
	v := kubeRevisionView{Number: rev.Number, RunID: rev.RunID, Parent: rev.Parent, Origin: rev.Origin, Errors: rev.Errors, Findings: rev.Findings, Lineage: rev.Lineage,
		Files: []kubeFileInfo{}, CreatedBy: rev.CreatedBy, CreatedAt: rev.CreatedAt}
	if v.Findings == nil {
		v.Findings = []kubeconvert.Finding{}
	}
	for _, f := range rev.Files {
		v.Files = append(v.Files, kubeFileInfo{Path: f.Path, Digest: f.Digest, Size: f.Size})
	}
	return v
}

func (a *API) ListKubeProviders(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId) {
	list, err := a.kube.Providers(r.Context(), principal(r), projectID)
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (a *API) ListKubeConversions(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.ListKubeConversionsParams) {
	limit := 50
	if params.Limit != nil {
		limit = *params.Limit
	}
	list, err := a.kube.List(r.Context(), principal(r), projectID, limit)
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	out := struct {
		Items []kubeConversionView `json:"items"`
	}{Items: []kubeConversionView{}}
	for _, c := range list {
		out.Items = append(out.Items, toKubeConversion(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateKubeConversion(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.CreateKubeConversionParams) {
	var in kubeconvert.CreateRequest
	if !decodeBody(w, r, &in) {
		return
	}
	c, err := a.kube.Create(r.Context(), principal(r), projectID, in, params.IdempotencyKey, requestID(w))
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toKubeConversion(c))
}

func (a *API) GetKubeConversion(w http.ResponseWriter, r *http.Request, id apigen.ConversionId) {
	c, err := a.kube.Get(r.Context(), principal(r), id)
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toKubeConversion(c))
}

func (a *API) ListKubeConversionRuns(w http.ResponseWriter, r *http.Request, id apigen.ConversionId) {
	runs, err := a.kube.Runs(r.Context(), principal(r), id)
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	out := struct {
		Items []kubeRunView `json:"items"`
	}{Items: []kubeRunView{}}
	for _, run := range runs {
		out.Items = append(out.Items, toKubeRun(run))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) RerunKubeConversion(w http.ResponseWriter, r *http.Request, id apigen.ConversionId, params apigen.RerunKubeConversionParams) {
	var in struct {
		Options *kubeconvert.Options `json:"options"`
		Answers map[string]string    `json:"answers"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	c, err := a.kube.Rerun(r.Context(), principal(r), id, in.Options, in.Answers, params.IdempotencyKey, requestID(w))
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toKubeConversion(c))
}

func (a *API) ListKubeConversionRevisions(w http.ResponseWriter, r *http.Request, id apigen.ConversionId) {
	revs, err := a.kube.Revisions(r.Context(), principal(r), id)
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	type item struct {
		Number    int       `json:"number"`
		RunID     string    `json:"runId,omitempty"`
		Parent    int       `json:"parent,omitempty"`
		Origin    string    `json:"origin"`
		Errors    int       `json:"errors"`
		Findings  int       `json:"findings"`
		CreatedBy string    `json:"createdBy"`
		CreatedAt time.Time `json:"createdAt"`
	}
	out := struct {
		Items []item `json:"items"`
	}{Items: []item{}}
	for _, rev := range revs {
		out.Items = append(out.Items, item{Number: rev.Number, RunID: rev.RunID, Parent: rev.Parent, Origin: rev.Origin, Errors: rev.Errors, Findings: len(rev.Findings), CreatedBy: rev.CreatedBy, CreatedAt: rev.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetKubeConversionRevision(w http.ResponseWriter, r *http.Request, id apigen.ConversionId, rev apigen.Revision) {
	_, got, err := a.kube.Revision(r.Context(), principal(r), id, rev)
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toKubeRevision(got))
}

func (a *API) GetKubeConversionFindings(w http.ResponseWriter, r *http.Request, id apigen.ConversionId, rev apigen.Revision) {
	_, got, err := a.kube.Revision(r.Context(), principal(r), id, rev)
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Items []kubeconvert.Finding `json:"items"`
	}{Items: toKubeRevision(got).Findings})
}

func (a *API) GetKubeConversionFile(w http.ResponseWriter, r *http.Request, id apigen.ConversionId, rev apigen.Revision, params apigen.GetKubeConversionFileParams) {
	_, got, err := a.kube.Revision(r.Context(), principal(r), id, rev)
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	for _, f := range got.Files {
		if f.Path == params.Path {
			writeJSON(w, http.StatusOK, f)
			return
		}
	}
	apigen.WriteProblem(w, http.StatusNotFound, "not_found", "the revision has no file "+params.Path)
}

func (a *API) EditKubeConversionFile(w http.ResponseWriter, r *http.Request, id apigen.ConversionId, rev apigen.Revision, params apigen.EditKubeConversionFileParams) {
	var in struct {
		Content string `json:"content"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	got, err := a.kube.Edit(r.Context(), principal(r), id, rev, params.Path, in.Content, requestID(w))
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toKubeRevision(got))
}

func (a *API) DownloadKubeConversionBundle(w http.ResponseWriter, r *http.Request, id apigen.ConversionId, rev apigen.Revision) {
	c, got, err := a.kube.Download(r.Context(), principal(r), id, rev, requestID(w))
	if err != nil {
		a.kubeError(w, r, err)
		return
	}
	var buf bytes.Buffer
	if err := kubeconvert.Bundle(&buf, c.Name, got); err != nil {
		a.internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-r%d.zip"`, c.Name, got.Number))
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
