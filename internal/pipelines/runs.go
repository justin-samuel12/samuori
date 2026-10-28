package pipelines

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/tenancy"
)

// Run states. A run ends when its build starts; the build, its checks and publication have their own lifecycles.
const (
	Queued   = "queued"
	Fetching = "fetching"
	Started  = "started"
	Failed   = "failed"
	Skipped  = "skipped"
)

type Run struct {
	ID, OrgID, PipelineID, ProjectID string
	Trigger, Ref                     string
	RequestedCommit, Commit          string
	State                            string
	FailureCode, FailureMessage      string
	SkippedEntries                   int
	SnapshotID, BuildRunID           string
	RequestedBy                      string
	CreatedAt, UpdatedAt             time.Time
}

const runCols = `id, org_id, pipeline_id, project_id, trigger, ref, COALESCE(requested_commit, ''), COALESCE(commit_sha, ''), state, COALESCE(failure_code, ''), COALESCE(failure_message, ''), skipped_entries, COALESCE(snapshot_id, ''), COALESCE(build_run_id, ''), requested_by, created_at, updated_at`

func scanRun(sc interface{ Scan(...any) error }) (Run, error) {
	var r Run
	err := sc.Scan(&r.ID, &r.OrgID, &r.PipelineID, &r.ProjectID, &r.Trigger, &r.Ref, &r.RequestedCommit, &r.Commit, &r.State, &r.FailureCode, &r.FailureMessage, &r.SkippedEntries, &r.SnapshotID, &r.BuildRunID, &r.RequestedBy, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// HookResult is the answer to a verified webhook delivery.
type HookResult struct {
	Status string // queued, skipped, duplicate or ignored
	RunID  string
	Reason string
}

// ErrUnverified answers every webhook that cannot be verified, so callers learn nothing about which pipelines exist.
var ErrUnverified = errors.New("the delivery could not be verified")

const MaxHookBody = 1 << 20

type pushPayload struct {
	Ref         string  `json:"ref"`
	After       string  `json:"after"`
	CheckoutSHA *string `json:"checkout_sha"`
	Deleted     bool    `json:"deleted"`
}

func header(h http.Header, names ...string) string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// verify checks the delivery against the pipeline's secret before anything in the body is read (SEC-HOOK-01).
func verify(scheme string, secret []byte, h http.Header, body []byte) bool {
	switch scheme {
	case SchemeHMAC:
		sig, ok := strings.CutPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
		got, err := hex.DecodeString(sig)
		if !ok || err != nil {
			return false
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		return hmac.Equal(got, mac.Sum(nil))
	case SchemeGitLab:
		tok := h.Get("X-Gitlab-Token")
		return tok != "" && subtle.ConstantTimeCompare([]byte(tok), secret) == 1
	}
	return false
}

// Hook handles a webhook delivery. The payload only selects a ref and names a commit; the pipeline's own URL and spec
// decide what is fetched and built (SEC-HOOK-02).
func (s *Service) Hook(ctx context.Context, pipelineID string, h http.Header, body []byte) (HookResult, error) {
	if len(body) > MaxHookBody {
		return HookResult{}, ErrUnverified
	}
	p, err := s.load(ctx, pipelineID)
	if errors.Is(err, ErrNotFound) || (err == nil && !p.Enabled) {
		return HookResult{}, ErrUnverified
	}
	if err != nil {
		return HookResult{}, err
	}
	secret, err := s.secrets.ControllerValue(ctx, p.OrgID, p.hookSecretID)
	if err != nil {
		return HookResult{}, err
	}
	system := authz.Principal{OrgID: p.OrgID, Kind: authz.System}
	if !verify(p.HookScheme, secret, h, body) {
		_ = s.db.InTx(ctx, func(tx *sql.Tx) error {
			return audit.Append(ctx, s.db, tx, audit.Event{Actor: system, Action: "pipeline.hook", ResourceType: "pipeline", ResourceID: p.ID, Outcome: "denied", Reason: "signature"})
		})
		return HookResult{}, ErrUnverified
	}
	event := header(h, "X-GitHub-Event", "X-Gitea-Event", "X-Forgejo-Event", "X-Gitlab-Event")
	switch event {
	case "ping":
		return HookResult{Status: "ignored", Reason: "ping"}, nil
	case "push", "Push Hook", "Tag Push Hook":
	default:
		return HookResult{Status: "ignored", Reason: "only push events start runs"}, nil
	}
	var push pushPayload
	if err := json.Unmarshal(body, &push); err != nil {
		return HookResult{Status: "ignored", Reason: "the payload is not a push event"}, nil
	}
	commit := push.After
	if push.CheckoutSHA != nil {
		commit = *push.CheckoutSHA
	}
	switch {
	case push.Deleted || strings.Trim(commit, "0") == "":
		return HookResult{Status: "ignored", Reason: "the ref was deleted"}, nil
	case !ValidRef(push.Ref) || !ValidCommit(commit):
		return HookResult{Status: "ignored", Reason: "the ref or commit is not valid"}, nil
	case !p.Matches(push.Ref):
		return HookResult{Status: "ignored", Reason: "the pipeline does not build " + push.Ref}, nil
	}
	// Delivery ID headers are not signed, and no Git host signs a timestamp, so only the signed body identifies a
	// delivery: a replay under a new delivery ID is still the same delivery.
	sum := sha256.Sum256(body)
	return s.enqueue(ctx, p, "webhook", push.Ref, commit, "body:"+hex.EncodeToString(sum[:]), "webhook")
}

func (s *Service) enqueue(ctx context.Context, p Pipeline, trigger, ref, commit, delivery, requestedBy string) (HookResult, error) {
	var out HookResult
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if delivery != "" {
			var existing string
			err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM pipeline_runs WHERE pipeline_id = ? AND delivery_id = ?`), p.ID, delivery).Scan(&existing)
			if err == nil {
				out = HookResult{Status: "duplicate", RunID: existing}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		state, reason := Queued, ""
		if commit != "" {
			var n int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM pipeline_runs WHERE pipeline_id = ? AND state IN (?, ?, ?) AND (requested_commit = ? OR commit_sha = ?)`), p.ID, Queued, Fetching, Started, commit, commit).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				state, reason = Skipped, "the commit already has a run"
			}
		}
		if state == Queued && trigger == "webhook" {
			var n int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM pipeline_runs WHERE pipeline_id = ? AND state IN (?, ?)`), p.ID, Queued, Fetching).Scan(&n); err != nil {
				return err
			}
			if n >= s.MaxQueued {
				return ErrBusy
			}
		}
		if err := tenancy.RequireActive(ctx, s.db, tx, p.ProjectID); err != nil {
			return err
		}
		now := s.now().UTC()
		id := ids.New()
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO pipeline_runs (id, org_id, pipeline_id, project_id, trigger, ref, requested_commit, delivery_id, state, failure_message, requested_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), id, p.OrgID, p.ID, p.ProjectID, trigger, ref, nullable(commit), nullable(delivery), state, nullable(reason), requestedBy, now, now); err != nil {
			return err
		}
		out = HookResult{Status: state, RunID: id, Reason: reason}
		actor := authz.Principal{OrgID: p.OrgID, Kind: authz.System}
		if requestedBy != "webhook" {
			actor = authz.Principal{ID: requestedBy, OrgID: p.OrgID, Kind: authz.User}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pipeline.trigger", ResourceType: "pipeline", ResourceID: p.ID, Outcome: state, Reason: trigger + " " + ref})
	})
	return out, err
}

// Start queues a run of ref by hand or from external CI. The caller needs a build role on the project, as does the
// pipeline's owner when the run starts. Repeating a key returns the first run.
func (s *Service) Start(ctx context.Context, actor authz.Principal, id, ref, trigger, key string) (Run, error) {
	p, err := s.Get(ctx, actor, id)
	if err != nil {
		return Run{}, err
	}
	if _, err := s.projects.GetProject(ctx, actor, BuildRoles, p.ProjectID); err != nil {
		return Run{}, ErrForbidden
	}
	if !p.Enabled {
		return Run{}, &Invalid{"the pipeline is disabled"}
	}
	if !p.Matches(ref) {
		return Run{}, &Invalid{"the pipeline does not build " + ref}
	}
	delivery := ""
	if key != "" {
		delivery = "start:" + actor.ID + ":" + key
	}
	res, err := s.enqueue(ctx, p, trigger, ref, "", delivery, actor.ID)
	if err != nil {
		return Run{}, err
	}
	return s.run(ctx, res.RunID)
}

func (s *Service) run(ctx context.Context, id string) (Run, error) {
	r, err := scanRun(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+runCols+` FROM pipeline_runs WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// Runs lists a pipeline's runs, newest first.
func (s *Service) Runs(ctx context.Context, actor authz.Principal, id, before string, limit int) ([]Run, error) {
	if _, err := s.Get(ctx, actor, id); err != nil {
		return nil, err
	}
	q, args := `SELECT `+runCols+` FROM pipeline_runs WHERE pipeline_id = ?`, []any{id}
	if before != "" {
		q, args = q+` AND `+db.NewestBefore("pipeline_runs", "pipeline_runs"), append(args, before, before, before)
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(q+` ORDER BY created_at DESC, id DESC LIMIT ?`), append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// claimFor bounds one fetch and snapshot; a run still fetching after it is claimed again.
const claimFor = 20 * time.Minute

// ProcessNext claims one queued run and carries it to a started build or a failure. It reports whether it found one.
func (s *Service) ProcessNext(ctx context.Context) (bool, error) {
	now := s.now().UTC()
	orphaned, oargs := s.owner.Orphaned("owner_controller")
	claimable := `(state = ? OR (state = ? AND (claimed_until < ? OR ` + orphaned + `)))`
	cargs := append([]any{Queued, Fetching, now}, oargs...)
	var id string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM pipeline_runs WHERE `+claimable+` ORDER BY created_at LIMIT 1`), cargs...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	res, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE pipeline_runs SET state = ?, claimed_until = ?, owner_controller = ?, updated_at = ? WHERE id = ? AND `+claimable),
		append([]any{Fetching, now.Add(claimFor), s.owner.ControllerID(), now, id}, cargs...)...)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return true, nil
	}
	return true, s.execute(ctx, id)
}

func (s *Service) execute(ctx context.Context, id string) error {
	r, err := s.run(ctx, id)
	if err != nil {
		return err
	}
	p, err := s.load(ctx, r.PipelineID)
	if errors.Is(err, ErrNotFound) {
		return s.finish(ctx, r, Failed, "deleted", "the pipeline was deleted before the run started", nil)
	}
	if err != nil {
		return err
	}
	if !p.Enabled {
		return s.finish(ctx, r, Failed, "disabled", "the pipeline was disabled before the run started", nil)
	}
	owner, _, err := s.projects.LoadPrincipal(ctx, p.OwnerID)
	if err != nil || owner.OrgID != p.OrgID {
		return s.finish(ctx, r, Failed, "owner_unavailable", "the pipeline's owner no longer exists or is disabled", nil)
	}
	if _, err := s.projects.GetProject(ctx, owner, BuildRoles, p.ProjectID); err != nil {
		return s.finish(ctx, r, Failed, "owner_forbidden", "the pipeline's owner can no longer build in this project", nil)
	}
	req := FetchRequest{URL: p.RepoURL, Ref: r.Ref, Username: p.Username, AllowPrivateNetwork: p.AllowPrivateNetwork, MaxBytes: s.snapshots.Limits().MaxArchiveBytes, MaxFiles: s.snapshots.Limits().MaxFiles}
	if p.tokenSecretID != "" {
		tok, err := s.secrets.ControllerValue(ctx, p.OrgID, p.tokenSecretID)
		if err != nil {
			return s.finish(ctx, r, Failed, "token_unavailable", "the repository token could not be read", nil)
		}
		req.Token = string(tok)
	}
	dir, err := os.MkdirTemp(s.snapshots.Staging(), "pipeline-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	archive := filepath.Join(dir, "commit.tar")
	got, err := s.fetcher.Fetch(ctx, req, archive)
	var fe *FetchError
	if errors.As(err, &fe) {
		return s.finish(ctx, r, Failed, fe.Code, fe.Message, nil)
	}
	if err != nil {
		return s.finish(ctx, r, Failed, "fetch_failed", "fetching the repository failed", err)
	}
	r.Commit, r.SkippedEntries = got.Commit, got.Skipped
	var dup int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM pipeline_runs WHERE pipeline_id = ? AND id <> ? AND state = ? AND commit_sha = ?`), p.ID, r.ID, Started, got.Commit).Scan(&dup); err != nil {
		return err
	}
	if dup > 0 {
		return s.finish(ctx, r, Skipped, "", "the commit already has a build", nil)
	}
	snap, err := s.snapshots.Ingest(ctx, owner, BuildRoles, p.ProjectID, archive, "")
	var rej *sources.Rejection
	if errors.As(err, &rej) {
		return s.finish(ctx, r, Failed, rej.Code, rej.Message, nil)
	}
	if err != nil {
		return s.finish(ctx, r, Failed, "snapshot_failed", "the commit could not be stored as a snapshot", err)
	}
	r.SnapshotID = snap.ID
	revision := p.SpecRevision
	if revision == 0 {
		sp, err := s.specs.Get(ctx, owner, BuildRoles, p.SpecID)
		if err != nil {
			return s.finish(ctx, r, Failed, "spec_unavailable", "the pipeline's spec no longer exists", nil)
		}
		revision = sp.LatestRevision
	}
	b, err := s.builds.Create(ctx, owner, BuildRoles, p.SpecID, revision, snap.ID, builds.CreateOptions{SourceRevision: got.Commit}, "pipeline-"+r.ID, "")
	if err != nil {
		msg, cause := buildRefusal(err)
		return s.finish(ctx, r, Failed, "build_refused", "the build could not be started: "+msg, cause)
	}
	r.BuildRunID = b.ID
	return s.finish(ctx, r, Started, "", "", nil)
}

// buildRefusal names typed refusals; anything else is logged, not shown.
func buildRefusal(err error) (string, error) {
	switch {
	case errors.Is(err, builds.ErrLocation):
		return err.Error(), nil
	case errors.Is(err, builds.ErrNotFound), errors.Is(err, tenancy.ErrNotFound), errors.Is(err, tenancy.ErrForbidden):
		return "the owner cannot build this spec or snapshot", nil
	}
	return "see the server log", err
}

func (s *Service) finish(ctx context.Context, r Run, state, code, msg string, cause error) error {
	now := s.now().UTC()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE pipeline_runs SET state = ?, failure_code = ?, failure_message = ?, commit_sha = ?, skipped_entries = ?, snapshot_id = ?, build_run_id = ?, claimed_until = NULL, updated_at = ? WHERE id = ? AND state = ?`),
			state, nullable(code), nullable(msg), nullable(r.Commit), r.SkippedEntries, nullable(r.SnapshotID), nullable(r.BuildRunID), now, r.ID, Fetching); err != nil {
			return err
		}
		reason := code
		if state == Started {
			reason = "build " + r.BuildRunID
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{OrgID: r.OrgID, Kind: authz.System}, Action: "pipeline.run", ResourceType: "pipeline", ResourceID: r.PipelineID, Outcome: state, Reason: reason})
	})
	if err == nil && cause != nil {
		return fmt.Errorf("pipeline run %s: %w", r.ID, cause)
	}
	return err
}

// RunLoop processes queued runs one at a time until ctx ends.
func (s *Service) RunLoop(ctx context.Context, every time.Duration, onError func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for {
			found, err := s.ProcessNext(ctx)
			if err != nil {
				onError(err)
			}
			if !found || ctx.Err() != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
