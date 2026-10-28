package api

import (
	"errors"
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/imagesources"
)

func (a *API) imageSourceError(w http.ResponseWriter, r *http.Request, err error) {
	var inv *imagesources.Invalid
	switch {
	case errors.Is(err, imagesources.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators manage image sources")
	case errors.Is(err, imagesources.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, imagesources.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", "that name is already used")
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", inv.Reason)
	default:
		a.internalError(w, r, err)
	}
}

type imageSourceView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Upstream      string   `json:"upstream"`
	Mirror        string   `json:"mirror"`
	Patterns      []string `json:"repositoryPatterns"`
	TeamIDs       []string `json:"teamIds"`
	PullProfileID string   `json:"pullProfileId,omitempty"`
	Status        string   `json:"status"`
}

func toImageSource(s imagesources.Source) imageSourceView {
	return imageSourceView{s.ID, s.Name, s.Upstream, s.Mirror, s.Patterns, s.TeamIDs, s.PullProfileID, s.Status}
}

func (a *API) ListImageSources(w http.ResponseWriter, r *http.Request) {
	list, err := a.imageSources.List(r.Context(), principal(r))
	if err != nil {
		a.imageSourceError(w, r, err)
		return
	}
	out := struct {
		Items []imageSourceView `json:"items"`
	}{Items: []imageSourceView{}}
	for _, s := range list {
		out.Items = append(out.Items, toImageSource(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateImageSource(w http.ResponseWriter, r *http.Request, _ apigen.CreateImageSourceParams) {
	var in apigen.ImageSourceCreate
	if !decodeBody(w, r, &in) {
		return
	}
	src := imagesources.Source{Name: in.Name, Upstream: in.Upstream, Mirror: in.Mirror, Patterns: in.RepositoryPatterns, TeamIDs: in.TeamIds}
	if in.PullProfileId != nil {
		src.PullProfileID = *in.PullProfileId
	}
	out, err := a.imageSources.Create(r.Context(), principal(r), src, requestID(w))
	if err != nil {
		a.imageSourceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toImageSource(out))
}

func (a *API) SetImageSourceState(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.SetImageSourceStateParams) {
	var in apigen.EnabledState
	if !decodeBody(w, r, &in) {
		return
	}
	if err := a.imageSources.SetStatus(r.Context(), principal(r), id, in.Enabled, requestID(w)); err != nil {
		a.imageSourceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) GetImageSourceMirrorConfig(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	cfg, err := a.imageSources.MirrorConfig(r.Context(), principal(r), id)
	if err != nil {
		a.imageSourceError(w, r, err)
		return
	}
	bundle, err := a.imageSources.TrustBundle(r.Context(), principal(r).OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"config": cfg, "trustBundle": bundle})
}

func (a *API) RotateMirrorTokenKey(w http.ResponseWriter, r *http.Request, _ apigen.RotateMirrorTokenKeyParams) {
	cert, err := a.imageSources.RotateKey(r.Context(), principal(r), requestID(w))
	if err != nil {
		a.imageSourceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"certificate": cert})
}
