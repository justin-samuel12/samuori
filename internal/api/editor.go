package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/specs"
)

func (a *API) ValidateSpecDocument(w http.ResponseWriter, r *http.Request, _ apigen.ValidateSpecDocumentParams) {
	var in struct {
		Document json.RawMessage `json:"document"`
		YAML     *string         `json:"yaml"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	var input []byte
	switch {
	case in.YAML != nil && len(in.Document) == 0:
		input = []byte(*in.YAML)
	case in.YAML == nil && len(in.Document) > 0:
		input = in.Document
	default:
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "send exactly one of document or yaml")
		return
	}
	rd, err := a.specs.Render(r.Context(), principal(r).OrgID, input)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Valid          bool            `json:"valid"`
		Document       json.RawMessage `json:"document,omitempty"`
		CanonicalYAML  string          `json:"canonicalYaml,omitempty"`
		Digest         string          `json:"digest,omitempty"`
		SchemaErrors   []issueView     `json:"schemaErrors"`
		SemanticErrors []issueView     `json:"semanticErrors"`
	}{rd.Valid, rd.Document, rd.YAML, rd.Digest, issues(rd.Schema), issues(rd.Semantics)}
	writeJSON(w, http.StatusOK, out)
}

type draftView struct {
	Version   int             `json:"version"`
	Data      json.RawMessage `json:"data"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

func (a *API) GetWizardDraft(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId) {
	d, err := a.specs.GetDraft(r.Context(), principal(r), operationRoles(r.Context()), projectID)
	if err != nil {
		a.draftError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, draftView{d.Version, d.Data, d.UpdatedAt})
}

func (a *API) SaveWizardDraft(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, _ apigen.SaveWizardDraftParams) {
	var in struct {
		Version int             `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	d, err := a.specs.SaveDraft(r.Context(), principal(r), operationRoles(r.Context()), projectID, in.Version, in.Data)
	if err != nil {
		a.draftError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, draftView{d.Version, d.Data, d.UpdatedAt})
}

func (a *API) DeleteWizardDraft(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, _ apigen.DeleteWizardDraftParams) {
	if err := a.specs.DeleteDraft(r.Context(), principal(r), operationRoles(r.Context()), projectID); err != nil {
		a.draftError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) draftError(w http.ResponseWriter, r *http.Request, err error) {
	var secret *specs.SecretInDraft
	switch {
	case errors.As(err, &secret):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "secret_literal", secret.Message+" Drafts never store secret values.")
	case errors.Is(err, specs.ErrDraftTooLarge):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "too_large", err.Error())
	case errors.Is(err, specs.ErrDraftConflict):
		apigen.WriteProblem(w, http.StatusConflict, "draft_conflict", "this draft was changed in another window; reload it")
	case errors.Is(err, specs.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "no draft")
	default:
		a.specsError(w, r, err)
	}
}
