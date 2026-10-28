package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/selfupdate"
)

type rolloutStep struct {
	Seq       int       `json:"seq"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	Wave      int       `json:"wave"`
	Pool      string    `json:"pool,omitempty"`
	Location  string    `json:"location,omitempty"`
	Arch      string    `json:"arch,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type rolloutOut struct {
	ID          string        `json:"id"`
	Version     string        `json:"version"`
	State       string        `json:"state"`
	Reason      string        `json:"reason"`
	Detail      string        `json:"detail,omitempty"`
	CreatedBy   string        `json:"createdBy"`
	CreatedAt   time.Time     `json:"createdAt"`
	FinishedAt  *time.Time    `json:"finishedAt,omitempty"`
	Paused      bool          `json:"paused"`
	PauseReason string        `json:"pauseReason,omitempty"`
	WavePercent int           `json:"wavePercent"`
	Steps       []rolloutStep `json:"steps"`
}

type checkOut struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

type preflightOut struct {
	Version string        `json:"version"`
	Ready   bool          `json:"ready"`
	Checks  []checkOut    `json:"checks"`
	Steps   []rolloutStep `json:"steps"`
}

func toSteps(steps []selfupdate.Step) []rolloutStep {
	out := []rolloutStep{}
	for _, s := range steps {
		out = append(out, rolloutStep{Seq: s.Seq, Kind: s.Kind, Name: s.Name, State: s.State, Detail: s.Detail, Wave: s.Wave, Pool: s.Pool, Location: s.Location, Arch: s.Arch, UpdatedAt: s.UpdatedAt})
	}
	return out
}

func (a *API) toRollout(req *http.Request, r selfupdate.Rollout) rolloutOut {
	return rolloutOut{ID: r.ID, Version: r.Version, State: r.State, Reason: r.Reason, Detail: r.Detail, CreatedBy: a.username(req, r.CreatedBy), CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt,
		Paused: r.Paused, PauseReason: r.PauseReason, WavePercent: r.WavePercent, Steps: toSteps(r.Steps)}
}

func toRelease(r selfupdate.Release) apigen.Release {
	out := apigen.Release{Version: r.Version, Architectures: r.Architectures, Size: r.Size, Bundled: r.Bundled, UploadedBy: r.UploadedBy, UploadedAt: r.UploadedAt}
	if p := r.WorkerProtocol; p != nil {
		v := fmt.Sprintf("%d.%d", p.Major, p.Minor)
		out.WorkerProtocol = &v
	}
	if len(r.Changes) > 0 {
		changes := make([]struct {
			Commit  string `json:"commit"`
			Date    string `json:"date"`
			Summary string `json:"summary"`
		}, len(r.Changes))
		for i, c := range r.Changes {
			changes[i].Commit, changes[i].Date, changes[i].Summary = c.Commit, c.Date, c.Summary
		}
		out.Changes = &changes
	}
	return out
}

func (a *API) selfUpdateProblem(w http.ResponseWriter, r *http.Request, err error) {
	var inv *selfupdate.Invalid
	switch {
	case errors.Is(err, selfupdate.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, selfupdate.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, selfupdate.ErrExists), errors.Is(err, selfupdate.ErrRunning), errors.Is(err, selfupdate.ErrNoSigners):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", err.Error())
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", inv.Reason)
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) ListReleases(w http.ResponseWriter, r *http.Request) {
	list, err := a.selfUpdate.Releases(r.Context(), principal(r))
	if err != nil {
		a.selfUpdateProblem(w, r, err)
		return
	}
	out := apigen.ReleaseList{SignersConfigured: a.selfUpdate.SignersConfigured(), Items: []apigen.Release{}}
	for _, rel := range list {
		out.Items = append(out.Items, toRelease(rel))
	}
	writeJSON(w, http.StatusOK, out)
}

// UploadRelease spools the bundle to a temporary file, because verifying it reads it more than once.
func (a *API) UploadRelease(w http.ResponseWriter, r *http.Request, _ apigen.UploadReleaseParams) {
	if !principal(r).IsOrgAdmin() {
		a.selfUpdateProblem(w, r, selfupdate.ErrForbidden)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "send multipart/form-data with the bundle")
		return
	}
	var bundle *os.File
	defer func() {
		if bundle != nil {
			bundle.Close()
			os.Remove(bundle.Name())
		}
	}()
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "the upload ended early")
			return
		}
		if part.FormName() == "bundle" && bundle == nil {
			if bundle, err = os.CreateTemp("", "samuori-update-*.samuoriupdate"); err == nil {
				_, err = io.Copy(bundle, part)
			}
		}
		part.Close()
		if err != nil {
			apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "the bundle could not be read")
			return
		}
	}
	if bundle == nil {
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", "send the samuori-<version>.samuoriupdate bundle from make update")
		return
	}
	rel, err := a.selfUpdate.Upload(r.Context(), principal(r), bundle, requestID(w))
	if err != nil {
		a.selfUpdateProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRelease(rel))
}

func (a *API) DeleteRelease(w http.ResponseWriter, r *http.Request, version string, _ apigen.DeleteReleaseParams) {
	if err := a.selfUpdate.DeleteRelease(r.Context(), principal(r), version, requestID(w)); err != nil {
		a.selfUpdateProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) PreflightRelease(w http.ResponseWriter, r *http.Request, version string, params apigen.PreflightReleaseParams) {
	percent := 0
	if params.WavePercent != nil {
		percent = *params.WavePercent
	}
	p, err := a.selfUpdate.Preflight(r.Context(), principal(r), version, params.AllowDowngrade != nil && *params.AllowDowngrade, percent)
	if err != nil {
		a.selfUpdateProblem(w, r, err)
		return
	}
	out := preflightOut{Version: p.Version, Ready: p.Ready, Checks: []checkOut{}, Steps: toSteps(p.Steps)}
	for _, c := range p.Checks {
		out.Checks = append(out.Checks, checkOut{ID: c.ID, Name: c.Name, State: c.State, Detail: c.Detail})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) StartRollout(w http.ResponseWriter, r *http.Request, _ apigen.StartRolloutParams) {
	var body apigen.RolloutRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "invalid body")
		return
	}
	req := selfupdate.StartRequest{Version: body.Version,
		PauseAfterFirstWave: body.PauseAfterFirstWave != nil && *body.PauseAfterFirstWave,
		AllowDowngrade:      body.AllowDowngrade != nil && *body.AllowDowngrade}
	if body.Reason != nil {
		req.Reason = *body.Reason
	}
	if body.WavePercent != nil {
		req.WavePercent = *body.WavePercent
	}
	ro, err := a.selfUpdate.Start(r.Context(), principal(r), req, requestID(w))
	if err != nil {
		a.selfUpdateProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.toRollout(r, ro))
}

func (a *API) ListRollouts(w http.ResponseWriter, r *http.Request, params apigen.ListRolloutsParams) {
	limit := 20
	if params.Limit != nil && *params.Limit >= 1 && *params.Limit <= 100 {
		limit = *params.Limit
	}
	list, err := a.selfUpdate.List(r.Context(), principal(r), limit)
	if err != nil {
		a.selfUpdateProblem(w, r, err)
		return
	}
	out := struct {
		Items []rolloutOut `json:"items"`
	}{Items: []rolloutOut{}}
	for _, ro := range list {
		out.Items = append(out.Items, a.toRollout(r, ro))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetRollout(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	ro, err := a.selfUpdate.Get(r.Context(), principal(r), id)
	if err != nil {
		a.selfUpdateProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.toRollout(r, ro))
}

func (a *API) CancelRollout(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.CancelRolloutParams) {
	a.controlRollout(w, r, a.selfUpdate.Cancel, id)
}

func (a *API) PauseRollout(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.PauseRolloutParams) {
	a.controlRollout(w, r, a.selfUpdate.Pause, id)
}

func (a *API) ResumeRollout(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.ResumeRolloutParams) {
	a.controlRollout(w, r, a.selfUpdate.Resume, id)
}

func (a *API) controlRollout(w http.ResponseWriter, r *http.Request, fn func(context.Context, authz.Principal, string, string) (selfupdate.Rollout, error), id string) {
	ro, err := fn(r.Context(), principal(r), id, requestID(w))
	if err != nil {
		a.selfUpdateProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.toRollout(r, ro))
}

func (a *API) PurgeRollouts(w http.ResponseWriter, r *http.Request, _ apigen.PurgeRolloutsParams) {
	n, err := a.selfUpdate.PurgeHistory(r.Context(), principal(r), requestID(w))
	if err != nil {
		a.selfUpdateProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"removed": n})
}
