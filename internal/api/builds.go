package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
)

type runView struct {
	ID                string                   `json:"id"`
	ProjectID         string                   `json:"projectId"`
	SpecID            string                   `json:"specId"`
	SpecRevision      int                      `json:"specRevision"`
	SnapshotID        string                   `json:"snapshotId"`
	SnapshotDigest    string                   `json:"snapshotDigest"`
	SpecDigest        string                   `json:"specDigest"`
	PlanDigest        string                   `json:"planDigest,omitempty"`
	BaseImagePins     []buildplan.BaseImagePin `json:"baseImagePins,omitempty"`
	ImageLabels       map[string]string        `json:"imageLabels,omitempty"`
	ImageAnnotations  map[string]string        `json:"imageAnnotations,omitempty"`
	State             string                   `json:"state"`
	FailureClass      *string                  `json:"failureClass,omitempty"`
	BlockedReason     string                   `json:"blockedReason,omitempty"`
	IsolationProfile  string                   `json:"isolationProfile"`
	PreferredLocation string                   `json:"preferredLocation,omitempty"`
	SecretBearing     bool                     `json:"secretBearing"`
	CurrentAttemptID  string                   `json:"currentAttemptId,omitempty"`
	ArtifactID        string                   `json:"artifactId,omitempty"`
	ArtifactDigest    string                   `json:"artifactDigest,omitempty"`
	Validation        *validationBrief         `json:"validation,omitempty"`
	AttemptsUsed      int                      `json:"attemptsUsed"`
	MaxAttempts       int                      `json:"maxAttempts"`
	RequestedBy       *apigen.Principal        `json:"requestedBy"`
	CreatedAt         time.Time                `json:"createdAt"`
	UpdatedAt         time.Time                `json:"updatedAt"`
	Deletion          *deletionView            `json:"deletion,omitempty"`
	Version           string                   `json:"version,omitempty"`
}

type validationBrief struct {
	ID      string  `json:"id"`
	State   string  `json:"state"`
	Verdict *string `json:"verdict"`
}

// withValidation adds the artifact and its newest validation to a run view.
func (a *API) withValidation(r *http.Request, v runView) (runView, error) {
	if a.validations == nil || v.ArtifactDigest == "" {
		return v, nil
	}
	latest, ok, err := a.validations.Latest(r.Context(), v.ID)
	if err != nil || !ok {
		return v, err
	}
	v.ArtifactID = latest.ArtifactID
	v.Validation = &validationBrief{ID: latest.ID, State: latest.State}
	if latest.Verdict != "" {
		v.Validation.Verdict = &latest.Verdict
	}
	return v, nil
}

func toRun(r builds.Run) runView {
	v := runView{ID: r.ID, ProjectID: r.ProjectID, SpecID: r.SpecID, SpecRevision: r.SpecRevision, SnapshotID: r.SnapshotID, SnapshotDigest: r.SnapshotDigest,
		SpecDigest: r.SpecDigest, PlanDigest: r.PlanDigest, State: r.State, BlockedReason: r.BlockedReason, IsolationProfile: r.IsolationProfile, PreferredLocation: r.PreferredLocation,
		SecretBearing: r.SecretBearing, Version: r.Version, CurrentAttemptID: r.CurrentAttemptID, ArtifactDigest: r.ArtifactDigest, AttemptsUsed: r.AttemptsUsed, MaxAttempts: r.MaxAttempts, RequestedBy: &apigen.Principal{Id: r.RequestedBy, Type: apigen.PrincipalTypeUser},
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if r.FailureClass != "" {
		v.FailureClass = &r.FailureClass
	}
	return v
}

func (a *API) CreateBuild(w http.ResponseWriter, r *http.Request, params apigen.CreateBuildParams) {
	var in apigen.BuildRunCreate
	if !decodeBody(w, r, &in) {
		return
	}
	opts := builds.CreateOptions{CleanBuild: in.CleanBuild != nil && *in.CleanBuild}
	if in.Location != nil {
		opts.Location = *in.Location
	}
	run, err := a.builds.Create(r.Context(), principal(r), operationRoles(r.Context()), in.SpecId, in.SpecRevision, in.SnapshotId, opts, params.IdempotencyKey, requestID(w))
	if err != nil {
		a.buildsError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/builds/"+run.ID)
	writeJSON(w, http.StatusCreated, toRun(run))
}

func (a *API) ListBuilds(w http.ResponseWriter, r *http.Request, params apigen.ListBuildsParams) {
	limit, cursor := pageParams(params.Limit, params.Cursor)
	project, state := "", ""
	if params.ProjectId != nil {
		project = *params.ProjectId
	}
	if params.State != nil {
		state = string(*params.State)
	}
	list, err := a.builds.List(r.Context(), principal(r), operationRoles(r.Context()), project, state, cursor, limit+1)
	if err != nil {
		a.buildsError(w, r, err)
		return
	}
	out := struct {
		Items      []runView `json:"items"`
		NextCursor *string   `json:"nextCursor,omitempty"`
	}{Items: make([]runView, 0, len(list))}
	if len(list) > limit {
		list = list[:limit]
		next := list[limit-1].ID
		out.NextCursor = &next
	}
	for _, run := range list {
		out.Items = append(out.Items, toRun(run))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetBuild(w http.ResponseWriter, r *http.Request, id apigen.BuildId) {
	run, err := a.builds.Get(r.Context(), principal(r), operationRoles(r.Context()), id)
	if err != nil {
		a.buildsError(w, r, err)
		return
	}
	v, err := a.withValidation(r, toRun(run))
	if err == nil {
		v.Deletion, err = a.deletionOf(r, run.ID)
	}
	var plan builds.Plan
	if err == nil {
		plan, err = a.builds.RunPlan(r.Context(), run)
	}
	if plan.Dockerfile != nil {
		v.BaseImagePins = plan.Dockerfile.BaseImagePins
	}
	if plan.Image != nil {
		v.ImageLabels, v.ImageAnnotations = plan.Image.Labels, plan.Image.Annotations
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) CancelBuild(w http.ResponseWriter, r *http.Request, id apigen.BuildId, _ apigen.CancelBuildParams) {
	run, err := a.builds.Cancel(r.Context(), principal(r), operationRoles(r.Context()), id, requestID(w))
	if err != nil {
		a.buildsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toRun(run))
}

type attemptView struct {
	ID           string     `json:"id"`
	BuildRunID   string     `json:"buildRunId"`
	Number       int        `json:"number"`
	State        string     `json:"state"`
	WorkerID     string     `json:"workerId"`
	PoolID       string     `json:"poolId"`
	FailureClass string     `json:"failureClass,omitempty"`
	Detail       string     `json:"detail,omitempty"`
	Cleanup      cleanupV   `json:"cleanup"`
	StartedAt    time.Time  `json:"startedAt"`
	EndedAt      *time.Time `json:"endedAt,omitempty"`
	// EgressDenied comes from the worker's egress proxy report, never from the build log.
	EgressDenied     []egress.Advice `json:"egressDenied,omitempty"`
	EgressUnrecorded int             `json:"egressUnrecorded,omitempty"`
}

type cleanupV struct {
	Outcome string `json:"outcome"`
}

func (a *API) ListBuildAttempts(w http.ResponseWriter, r *http.Request, id apigen.BuildId) {
	list, err := a.builds.Attempts(r.Context(), principal(r), operationRoles(r.Context()), id)
	if err != nil {
		a.buildsError(w, r, err)
		return
	}
	out := struct {
		Items []attemptView `json:"items"`
	}{Items: make([]attemptView, 0, len(list))}
	for _, at := range list {
		out.Items = append(out.Items, attemptView{ID: at.ID, BuildRunID: at.RunID, Number: at.Number, State: at.State, WorkerID: at.WorkerID, PoolID: at.PoolID, FailureClass: at.FailureClass, Detail: at.Detail, Cleanup: cleanupV{at.Cleanup}, StartedAt: at.StartedAt, EndedAt: at.EndedAt,
			EgressDenied: at.EgressDenied, EgressUnrecorded: at.EgressUnrecorded})
	}
	writeJSON(w, http.StatusOK, out)
}

// StreamBuildEvents sends stored events as server-sent events, resuming after Last-Event-ID, until the run ends.
func (a *API) StreamBuildEvents(w http.ResponseWriter, r *http.Request, id apigen.BuildId, params apigen.StreamBuildEventsParams) {
	p, roles := principal(r), operationRoles(r.Context())
	var after int64
	if params.LastEventID != nil {
		after, _ = strconv.ParseInt(*params.LastEventID, 10, 64)
	}
	if _, err := a.builds.Get(r.Context(), p, roles, id); err != nil {
		a.buildsError(w, r, err)
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		events, err := a.builds.Events(r.Context(), p, roles, id, after)
		if err != nil {
			return
		}
		for _, e := range events {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, e.Data)
			after = e.Seq
		}
		if flusher != nil {
			flusher.Flush()
		}
		run, err := a.builds.Get(r.Context(), p, roles, id)
		if err != nil || (builds.Terminal(run.State) && len(events) == 0) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

// GetBuildLogs returns each attempt's stored log in order. Workers mask delivered secret values before upload.
func (a *API) GetBuildLogs(w http.ResponseWriter, r *http.Request, id apigen.BuildId) {
	if _, err := a.builds.Get(r.Context(), principal(r), operationRoles(r.Context()), id); err != nil {
		a.buildsError(w, r, err)
		return
	}
	if d, err := a.builds.Deleted(r.Context(), id); err != nil || d != nil {
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		apigen.WriteProblem(w, http.StatusGone, "deleted", "this build was deleted, with its image and logs")
		return
	}
	rows, err := a.db.SQL.QueryContext(r.Context(), a.db.Rebind(`SELECT t.number, b.digest FROM build_artifacts b JOIN build_attempts t ON t.id = b.attempt_id WHERE b.run_id = ? AND b.kind = 'log' ORDER BY t.number, b.created_at`), id)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	type entry struct {
		number int
		digest string
	}
	var logs []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.number, &e.digest); err != nil {
			rows.Close()
			a.internalError(w, r, err)
			return
		}
		logs = append(logs, e)
	}
	rows.Close()
	live, err := a.liveLog(r.Context(), id)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if live != nil {
		w.Header().Set("X-Samuori-Log-Live", "true")
	}
	w.WriteHeader(http.StatusOK)
	for _, e := range logs {
		fmt.Fprintf(w, "=== attempt %d ===\n", e.number)
		rc, err := a.artifacts.Blobs().Open(e.digest)
		if err != nil {
			fmt.Fprintln(w, "[log unavailable]")
			continue
		}
		_, _ = io.Copy(w, rc)
		rc.Close()
		fmt.Fprintln(w)
	}
	if live != nil {
		fmt.Fprintf(w, "=== attempt %d (running) ===\n", live.number)
		if live.secret {
			fmt.Fprintln(w, "This build uses secrets, so its output is shown only when the attempt ends, after secret values are masked.")
			return
		}
		_, _ = w.Write(live.data)
	}
}

type liveOutput struct {
	number int
	secret bool
	data   []byte
}

// liveLog is the output of the run's current attempt while it builds and before its final log is stored
// (docs/architecture/build-lifecycle.md#live-logs).
func (a *API) liveLog(ctx context.Context, runID string) (*liveOutput, error) {
	var out liveOutput
	var attempt string
	err := a.db.SQL.QueryRowContext(ctx, a.db.Rebind(`SELECT t.id, t.number, r.secret_bearing FROM build_runs r JOIN build_attempts t ON t.id = r.current_attempt_id
		WHERE r.id = ? AND t.ended_at IS NULL AND r.state IN ('assigned', 'preparing', 'building', 'committing')
		AND NOT EXISTS (SELECT 1 FROM build_artifacts b WHERE b.attempt_id = t.id AND b.kind = 'log')`), runID).Scan(&attempt, &out.number, &out.secret)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || out.secret {
		return &out, err
	}
	rows, err := a.db.SQL.QueryContext(ctx, a.db.Rebind(`SELECT data FROM build_live_logs WHERE attempt_id = ? ORDER BY offset_bytes`), attempt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var chunk []byte
		if err := rows.Scan(&chunk); err != nil {
			return nil, err
		}
		out.data = append(out.data, chunk...)
	}
	return &out, rows.Err()
}

func (a *API) buildsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, builds.ErrNotFound), errors.Is(err, specs.ErrNotFound), errors.Is(err, sources.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, sources.ErrDeleted):
		apigen.WriteProblem(w, http.StatusGone, "source_deleted", "this build's source upload was deleted; upload the source again and build from the new upload")
	case errors.Is(err, builds.ErrTerminal):
		apigen.WriteProblem(w, http.StatusConflict, "finished", err.Error())
	case errors.Is(err, builds.ErrLocation):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "location_not_allowed", err.Error())
	default:
		a.tenancyError(w, r, err)
	}
}
