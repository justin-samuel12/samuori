package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/publication"
)

type deletionView struct {
	DeletedBy string    `json:"deletedBy"`
	DeletedAt time.Time `json:"deletedAt"`
	Reason    string    `json:"reason"`
}

func (a *API) deletionOf(r *http.Request, runID string) (*deletionView, error) {
	d, err := a.builds.Deleted(r.Context(), runID)
	if err != nil || d == nil {
		return nil, err
	}
	return &deletionView{DeletedBy: a.displayName(r.Context(), d.By), DeletedAt: d.At, Reason: d.Reason}, nil
}

// DeleteBuild removes a finished build's image, logs and cache and keeps its record (ADR 0053).
func (a *API) DeleteBuild(w http.ResponseWriter, r *http.Request, id apigen.BuildId, _ apigen.DeleteBuildParams) {
	var in struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &in) {
		return
	}
	d, err := a.builds.Delete(r.Context(), principal(r), operationRoles(r.Context()), id, in.Reason, requestID(w))
	switch {
	case errors.Is(err, builds.ErrDeleteRoles):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, builds.ErrDeleted):
		apigen.WriteProblem(w, http.StatusConflict, "deleted", err.Error())
	case errors.Is(err, builds.ErrInUse):
		apigen.WriteProblem(w, http.StatusConflict, "in_use", "The build's image is still in use: "+err.Error()+".")
	case errors.Is(err, builds.ErrNotFinished), errors.Is(err, builds.ErrReason):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	case err != nil:
		a.buildsError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, deletionView{DeletedBy: a.displayName(r.Context(), d.By), DeletedAt: d.At, Reason: d.Reason})
	}
}

// displayName is a user's display name or username; the ID when no user has it.
func (a *API) displayName(ctx context.Context, id string) string {
	var name string
	if err := a.db.SQL.QueryRowContext(ctx, a.db.Rebind(`SELECT CASE WHEN display_name <> '' THEN display_name ELSE username END FROM users WHERE id = ?`), id).Scan(&name); err != nil {
		return id
	}
	return name
}

// DownloadArtifact streams a validated image as an OCI archive (ADR 0053).
func (a *API) DownloadArtifact(w http.ResponseWriter, r *http.Request, id apigen.ArtifactId) {
	d, err := a.publications.Download(r.Context(), principal(r), operationRoles(r.Context()), id, requestID(w))
	if err != nil {
		if errors.Is(err, publication.ErrForbidden) {
			apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only project maintainers, publishers and administrators download images")
			return
		}
		a.publicationError(w, r, err)
		return
	}
	stream, closeStream, err := a.publications.OpenDownload(d)
	if err != nil {
		a.log.Error("image download", "artifact", id, "error", err)
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "unreadable_image", "the stored image archive could not be read")
		return
	}
	defer closeStream()
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, d.Filename))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := stream.Write(w); err != nil {
		a.log.Error("image download interrupted", "artifact", id, "error", err)
	}
}
