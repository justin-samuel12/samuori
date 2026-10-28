package api

import (
	"errors"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/validation"
)

func (a *API) validationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, validation.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, validation.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "you may not start validations for this project")
	default:
		a.internalError(w, r, err)
	}
}

type findingView struct {
	Rule            string `json:"rule"`
	Class           string `json:"class,omitempty"`
	Path            string `json:"path"`
	ExpectedReason  string `json:"expectedReason,omitempty"`
	LayerDigest     string `json:"layerDigest,omitempty"`
	SizeBytes       int64  `json:"sizeBytes"`
	LogicalSecretID string `json:"logicalSecretId,omitempty"`
	Outcome         string `json:"outcome"`
	ExceptionID     string `json:"exceptionId,omitempty"`
}

type checkView struct {
	CheckID           string                     `json:"checkId"`
	Category          string                     `json:"category"`
	Origin            string                     `json:"origin"`
	Required          bool                       `json:"required"`
	Outcome           string                     `json:"outcome"`
	Reason            string                     `json:"reason,omitempty"`
	Tool              *tool                      `json:"tool,omitempty"`
	OutputDigest      string                     `json:"outputDigest,omitempty"`
	SensitiveFindings []findingView              `json:"sensitiveFindings"`
	Vulnerabilities   []validation.Vulnerability `json:"vulnerabilities,omitempty"`
}

type tool struct {
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
	Database string `json:"database,omitempty"`
}

type validationView struct {
	ID             string      `json:"id"`
	ArtifactID     string      `json:"artifactId"`
	ArtifactDigest string      `json:"artifactDigest"`
	BuildRunID     string      `json:"buildRunId,omitempty"`
	Trigger        string      `json:"trigger"`
	State          string      `json:"state"`
	Verdict        *string     `json:"verdict"`
	Classification string      `json:"classification,omitempty"`
	Reason         string      `json:"reason,omitempty"`
	Checks         []checkView `json:"checks"`
	CreatedAt      time.Time   `json:"createdAt"`
	StartedAt      *time.Time  `json:"startedAt,omitempty"`
	FinishedAt     *time.Time  `json:"finishedAt,omitempty"`
}

func layer(d string) string {
	if len(d) == 71 && d[:7] == "sha256:" {
		return d
	}
	return ""
}

func toValidation(v validation.Run) validationView {
	out := validationView{ID: v.ID, ArtifactID: v.ArtifactID, ArtifactDigest: v.ArtifactDigest, BuildRunID: v.BuildRunID, Trigger: v.Trigger, State: v.State, Reason: v.Reason, Classification: v.Classification,
		Checks: []checkView{}, CreatedAt: v.CreatedAt, StartedAt: v.StartedAt, FinishedAt: v.FinishedAt}
	if v.Verdict != "" {
		out.Verdict = &v.Verdict
	}
	for _, c := range v.Checks {
		cv := checkView{CheckID: c.ID, Category: c.Category, Origin: c.Origin, Required: c.Required, Outcome: c.Outcome, Reason: c.Reason, OutputDigest: c.OutputDigest, SensitiveFindings: []findingView{}, Vulnerabilities: c.Vulnerabilities}
		if c.Tool.Name != "" {
			cv.Tool = &tool{Name: c.Tool.Name, Version: c.Tool.Version, Database: c.Tool.Database}
		}
		for _, f := range c.Findings {
			o := f.Outcome
			if o == "" {
				o = "warn"
			}
			cv.SensitiveFindings = append(cv.SensitiveFindings, findingView{Rule: f.Rule, Class: f.Class, Path: f.Path, ExpectedReason: f.Expected, LayerDigest: layer(f.Layer), SizeBytes: f.Size, LogicalSecretID: f.SecretID, Outcome: o, ExceptionID: f.ExceptionID})
		}
		out.Checks = append(out.Checks, cv)
	}
	return out
}

func (a *API) GetArtifact(w http.ResponseWriter, r *http.Request, id apigen.ArtifactId) {
	art, err := a.validations.Artifact(r.Context(), principal(r), operationRoles(r.Context()), id)
	if err != nil {
		a.validationError(w, r, err)
		return
	}
	size := int(art.Size)
	out := apigen.ImageArtifact{Id: art.ID, Digest: art.Digest, BuildRunId: art.BuildRunID, Sensitivity: apigen.ImageArtifactSensitivityNormal, SizeBytes: &size, CreatedAt: &art.CreatedAt}
	if v, ok, err := a.validations.Latest(r.Context(), art.BuildRunID); err == nil && ok && v.Classification == "restricted" {
		out.Sensitivity = apigen.ImageArtifactSensitivityRestricted
	}
	media := "application/vnd.oci.image.layout.v1+tar"
	out.MediaType = &media
	if tags, err := a.publications.DefaultTags(r.Context(), art.BuildRunID); err == nil {
		out.DefaultTags = &tags
	}
	var version string
	if err := a.db.SQL.QueryRowContext(r.Context(), a.db.Rebind(`SELECT COALESCE(version, '') FROM build_runs WHERE id = ?`), art.BuildRunID).Scan(&version); err == nil && version != "" {
		out.Version = &version
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) ListValidations(w http.ResponseWriter, r *http.Request, id apigen.ArtifactId) {
	list, err := a.validations.List(r.Context(), principal(r), operationRoles(r.Context()), id)
	if err != nil {
		a.validationError(w, r, err)
		return
	}
	out := struct {
		Items []validationView `json:"items"`
	}{Items: []validationView{}}
	for _, v := range list {
		out.Items = append(out.Items, toValidation(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetValidation(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	v, err := a.validations.Get(r.Context(), principal(r), operationRoles(r.Context()), id)
	if err != nil {
		a.validationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toValidation(v))
}

func (a *API) StartRevalidation(w http.ResponseWriter, r *http.Request, id apigen.ArtifactId, params apigen.StartRevalidationParams) {
	v, err := a.validations.Rescan(r.Context(), principal(r), operationRoles(r.Context()), id, params.IdempotencyKey)
	if err != nil {
		a.validationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toValidation(v))
}
