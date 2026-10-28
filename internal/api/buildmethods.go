package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/providers"
)

type buildMethodOption struct {
	Method    string `json:"method"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// GetProjectBuildMethods tells the wizard which methods the project may use. It reveals availability and the
// administrator's actionable reason only; provider pins, pools and project lists stay with administrators.
func (a *API) GetProjectBuildMethods(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId) {
	p := principal(r)
	if _, err := a.tenancy.GetProject(r.Context(), p, operationRoles(r.Context()), projectID); err != nil {
		a.tenancyError(w, r, err)
		return
	}
	pol, err := a.providers.Policy(r.Context(), p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Default string              `json:"default"`
		Methods []buildMethodOption `json:"methods"`
	}{Default: pol.Default, Methods: []buildMethodOption{}}
	for _, m := range providers.Methods {
		o := buildMethodOption{Method: m, Available: true}
		if err := a.providers.MethodAllowed(r.Context(), p.OrgID, m, "", projectID); err != nil {
			var refused *providers.Invalid
			if !errors.As(err, &refused) {
				a.internalError(w, r, err)
				return
			}
			o.Available, o.Reason = false, refused.Reason
		}
		out.Methods = append(out.Methods, o)
	}
	writeJSON(w, http.StatusOK, out)
}

// GetBuildRailpack returns each attempt's stored Railpack plan and planning report.
func (a *API) GetBuildRailpack(w http.ResponseWriter, r *http.Request, id apigen.BuildId) {
	if _, err := a.builds.Get(r.Context(), principal(r), operationRoles(r.Context()), id); err != nil {
		a.buildsError(w, r, err)
		return
	}
	rows, err := a.db.SQL.QueryContext(r.Context(), a.db.Rebind(`SELECT t.number, b.kind, b.digest FROM build_artifacts b JOIN build_attempts t ON t.id = b.attempt_id
		WHERE b.run_id = ? AND b.kind IN (?, ?) ORDER BY t.number, b.created_at`), id, artifacts.KindRailpackPlan, artifacts.KindRailpackInfo)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	type item struct {
		Attempt int             `json:"attempt"`
		Plan    json.RawMessage `json:"plan,omitempty"`
		Info    json.RawMessage `json:"info,omitempty"`
	}
	type found struct {
		n            int
		kind, digest string
	}
	var list []found
	for rows.Next() {
		var f found
		if err := rows.Scan(&f.n, &f.kind, &f.digest); err != nil {
			rows.Close()
			a.internalError(w, r, err)
			return
		}
		list = append(list, f)
	}
	rows.Close()
	items := []item{}
	for _, f := range list {
		i := slices.IndexFunc(items, func(x item) bool { return x.Attempt == f.n })
		if i < 0 {
			items = append(items, item{Attempt: f.n})
			i = len(items) - 1
		}
		raw, err := a.readBlob(f.digest, artifacts.MaxRailpackBytes)
		if err != nil || !json.Valid(raw) {
			continue
		}
		if f.kind == artifacts.KindRailpackPlan {
			items[i].Plan = raw
		} else {
			items[i].Info = raw
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) readBlob(digest string, limit int64) ([]byte, error) {
	rc, err := a.artifacts.Blobs().Open(digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err == nil && int64(len(raw)) > limit {
		err = errors.New("blob too large")
	}
	return raw, err
}

type lineageRow struct {
	attempt      int
	kind, digest string
	size         int64
}

func (a *API) lineage(r *http.Request, runID string, kinds ...string) ([]lineageRow, error) {
	q := `SELECT t.number, b.kind, b.digest, b.size FROM build_artifacts b JOIN build_attempts t ON t.id = b.attempt_id
		WHERE b.run_id = ? AND b.kind IN (?` + strings.Repeat(", ?", len(kinds)-1) + `) ORDER BY t.number, b.created_at`
	args := []any{runID}
	for _, k := range kinds {
		args = append(args, k)
	}
	rows, err := a.db.SQL.QueryContext(r.Context(), a.db.Rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lineageRow
	for rows.Next() {
		var l lineageRow
		if err := rows.Scan(&l.attempt, &l.kind, &l.digest, &l.size); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetBuildBuildpacks returns each attempt's stored buildpacks metadata and SBOM reference.
func (a *API) GetBuildBuildpacks(w http.ResponseWriter, r *http.Request, id apigen.BuildId) {
	if _, err := a.builds.Get(r.Context(), principal(r), operationRoles(r.Context()), id); err != nil {
		a.buildsError(w, r, err)
		return
	}
	list, err := a.lineage(r, id, artifacts.KindBuildpacksMetadata, artifacts.KindBuildpacksSBOM)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	type sbom struct {
		Digest    string `json:"digest"`
		SizeBytes int64  `json:"sizeBytes"`
	}
	type item struct {
		Attempt  int             `json:"attempt"`
		Metadata json.RawMessage `json:"metadata,omitempty"`
		SBOM     *sbom           `json:"sbom,omitempty"`
	}
	items := []item{}
	for _, f := range list {
		i := slices.IndexFunc(items, func(x item) bool { return x.Attempt == f.attempt })
		if i < 0 {
			items = append(items, item{Attempt: f.attempt})
			i = len(items) - 1
		}
		if f.kind == artifacts.KindBuildpacksSBOM {
			items[i].SBOM = &sbom{Digest: f.digest, SizeBytes: f.size}
			continue
		}
		if raw, err := a.readBlob(f.digest, artifacts.MaxRailpackBytes); err == nil && json.Valid(raw) {
			items[i].Metadata = raw
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GetBuildBuildpacksSbom returns one attempt's lifecycle SBOM archive.
func (a *API) GetBuildBuildpacksSbom(w http.ResponseWriter, r *http.Request, id apigen.BuildId, params apigen.GetBuildBuildpacksSbomParams) {
	if _, err := a.builds.Get(r.Context(), principal(r), operationRoles(r.Context()), id); err != nil {
		a.buildsError(w, r, err)
		return
	}
	list, err := a.lineage(r, id, artifacts.KindBuildpacksSBOM)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	i := slices.IndexFunc(list, func(l lineageRow) bool { return l.attempt == params.Attempt })
	if i < 0 {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "that attempt has no buildpacks SBOM")
		return
	}
	raw, err := a.readBlob(list[i].digest, artifacts.MaxBuildpacksSBOMBytes)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-attempt-%d-buildpacks-sbom.tar"`, id, params.Attempt))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
