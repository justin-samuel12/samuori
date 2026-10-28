package api

import (
	"errors"
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/operations"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/storage"
)

func (a *API) CreateUploadSession(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, _ apigen.CreateUploadSessionParams) {
	var body apigen.UploadSessionCreate
	if !decodeBody(w, r, &body) {
		return
	}
	var excludes []string
	if body.ExcludePatterns != nil {
		excludes = *body.ExcludePatterns
	}
	s, err := a.sources.CreateSession(r.Context(), principal(r), operationRoles(r.Context()), projectID, body.FileCount, body.TotalBytes, excludes, requestID(w))
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toUploadSession(s))
}

func (a *API) GetUploadPolicy(w http.ResponseWriter, _ *http.Request) {
	l := a.sources.Limits()
	writeJSON(w, http.StatusOK, apigen.UploadPolicy{
		MaxArchiveBytes: l.MaxArchiveBytes, MaxFiles: l.MaxFiles, MaxFileBytes: l.MaxFileBytes, ChunkBytes: a.sources.ChunkBytes(),
		DefaultExcludes: sources.DefaultExcludes(), KeptFiles: sources.KeptFiles(), SecretFilePatterns: sources.SecretFilePatterns(),
	})
}

func (a *API) GetUploadSession(w http.ResponseWriter, r *http.Request, id apigen.UploadId) {
	s, err := a.sources.GetSession(r.Context(), principal(r), operationRoles(r.Context()), id)
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toUploadSession(s))
}

func (a *API) PutUploadChunk(w http.ResponseWriter, r *http.Request, id apigen.UploadId, index int, _ apigen.PutUploadChunkParams) {
	if err := a.sources.PutChunk(r.Context(), principal(r), operationRoles(r.Context()), id, index, r.Body); err != nil {
		a.sourcesError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) CommitUpload(w http.ResponseWriter, r *http.Request, id apigen.UploadId, params apigen.CommitUploadParams) {
	op, err := a.sources.Commit(r.Context(), principal(r), operationRoles(r.Context()), id, params.IdempotencyKey, requestID(w))
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, toOperation(op, requestID(w)))
}

func (a *API) ListSnapshots(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.ListSnapshotsParams) {
	limit, cursor := 50, ""
	if params.Limit != nil {
		limit = *params.Limit
	}
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	snaps, err := a.sources.ListSnapshots(r.Context(), principal(r), operationRoles(r.Context()), projectID, cursor, limit+1)
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	out := apigen.SourceSnapshotList{Items: make([]apigen.SourceSnapshot, 0, len(snaps))}
	if len(snaps) > limit {
		snaps = snaps[:limit]
		next := snaps[limit-1].ID
		out.NextCursor = &next
	}
	for _, s := range snaps {
		out.Items = append(out.Items, toSnapshot(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetSnapshot(w http.ResponseWriter, r *http.Request, id apigen.SnapshotId) {
	s, err := a.sources.GetSnapshot(r.Context(), principal(r), operationRoles(r.Context()), id)
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSnapshot(s))
}

func (a *API) GetOperation(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	op, err := a.ops.Get(r.Context(), principal(r), id)
	if errors.Is(err, operations.ErrNotFound) {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toOperation(op, requestID(w)))
}

func (a *API) sourcesError(w http.ResponseWriter, r *http.Request, err error) {
	var rej *sources.Rejection
	switch {
	case errors.As(err, &rej):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, rej.Code, rej.Message)
	case errors.Is(err, sources.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, sources.ErrDeleted):
		apigen.WriteProblem(w, http.StatusGone, "source_deleted", "this source upload was deleted by the person who uploaded it; upload the source again")
	case errors.Is(err, sources.ErrNotUploader):
		apigen.WriteProblem(w, http.StatusForbidden, "not_uploader", err.Error())
	case errors.Is(err, sources.ErrInUse):
		apigen.WriteProblem(w, http.StatusConflict, "source_in_use", err.Error())
	case errors.Is(err, sources.ErrState):
		apigen.WriteProblem(w, http.StatusConflict, "upload_closed", "this upload is no longer accepting changes; start a new upload")
	case errors.Is(err, sources.ErrQuota):
		apigen.WriteProblem(w, http.StatusTooManyRequests, "too_many_uploads", "finish or let your other uploads expire before starting another")
	case errors.Is(err, storage.ErrCorrupt):
		a.log.Error("snapshot integrity failure", "id", requestID(w), "path", r.URL.Path)
		apigen.WriteProblem(w, http.StatusInternalServerError, "integrity", "stored source failed its integrity check; an operator has been notified")
	default:
		a.tenancyError(w, r, err)
	}
}

func toUploadSession(s sources.Session) apigen.UploadSession {
	out := apigen.UploadSession{
		Id: s.ID, ProjectId: s.ProjectID, Status: apigen.UploadSessionStatus(s.Status), TotalBytes: s.TotalBytes,
		ChunkBytes: s.ChunkBytes, MaxChunks: s.MaxChunks, ReceivedChunks: s.Received, ExpiresAt: s.ExpiresAt,
	}
	if s.OperationID != "" {
		out.OperationId = &s.OperationID
	}
	if s.SnapshotID != "" {
		out.SnapshotId = &s.SnapshotID
	}
	return out
}

func toSnapshot(s sources.Snapshot) apigen.SourceSnapshot {
	warnings := make([]apigen.Warning, 0, len(s.Warnings))
	for _, w := range s.Warnings {
		x := apigen.Warning{Code: w.Code, Message: w.Message}
		if w.Path != "" {
			x.Path = &w.Path
		}
		warnings = append(warnings, x)
	}
	rejected, excluded := s.Rejected, s.Excluded
	return apigen.SourceSnapshot{
		Id: s.ID, ProjectId: s.ProjectID, Digest: s.Digest, FileCount: s.FileCount, TotalBytes: s.TotalBytes,
		RejectedEntries: &rejected, ExcludedEntries: &excluded, Warnings: &warnings, CreatedAt: s.CreatedAt,
		CreatedBy: &apigen.Principal{Id: s.CreatedBy, Type: apigen.PrincipalTypeUser}, DeletedAt: s.DeletedAt,
	}
}

// DeleteSnapshot lets the uploader delete an upload, then removes its files that nothing else uses (ADR 0040).
func (a *API) DeleteSnapshot(w http.ResponseWriter, r *http.Request, id apigen.SnapshotId, _ apigen.DeleteSnapshotParams) {
	digests, err := a.sources.DeleteSnapshot(r.Context(), principal(r), operationRoles(r.Context()), id, requestID(w))
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	if _, _, err := a.artifacts.RemoveUnreferenced(r.Context(), digests); err != nil {
		a.log.Error("removing a deleted upload's files", "snapshot", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func toOperation(op operations.Operation, reqID string) apigen.Operation {
	out := apigen.Operation{Id: op.ID, Status: apigen.OperationStatus(op.Status)}
	if op.ResourceType != "" {
		out.ResourceType = &op.ResourceType
	}
	if op.ResourceID != "" {
		out.ResourceId = &op.ResourceID
	}
	if op.ErrorCode != "" {
		out.Error = &apigen.Problem{Code: op.ErrorCode, Message: op.ErrorMessage, RequestId: reqID}
	}
	return out
}
