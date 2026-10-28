// Package projects archives, restores and purges projects (ADR 0058). Archiving stops new work and revokes what the
// project was granted; purging deletes what it stored and keeps its records and audit trail.
package projects

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/approvals"
	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/tenancy"
)

var (
	ErrNotActive   = errors.New("the project is already archived")
	ErrNotArchived = errors.New("only an archived project can be restored or purged")
	ErrPurged      = errors.New("a purged project cannot be restored")
	ErrConfirm     = errors.New("type the project's name to confirm the purge")
	ErrBusy        = errors.New("work for this project is still finishing")
	ErrReason      = errors.New("the reason is at most 1024 characters")
	ErrIncomplete  = errors.New("the project is purged, but its stored files were not all removed; run the purge again")
)

const archivedReason = "project archived"

var (
	readers  = []authz.Role{authz.Developer, authz.Maintainer, authz.Publisher, authz.SecurityReviewer, authz.Administrator}
	managers = []authz.Role{authz.Maintainer, authz.Administrator}
)

type Service struct {
	db        *db.DB
	tenancy   *tenancy.Store
	approvals *approvals.Service
	builds    *builds.Service
	sources   *sources.Service
	artifacts *artifacts.Store
	now       func() time.Time
}

func NewService(d *db.DB, t *tenancy.Store, a *approvals.Service, b *builds.Service, s *sources.Service, art *artifacts.Store) *Service {
	return &Service{db: d, tenancy: t, approvals: a, builds: b, sources: s, artifacts: art, now: time.Now}
}

// Revoked counts what archiving took away or closed.
type Revoked struct {
	SecretGrants, SecretBindings, EgressAllowances, Destinations, FederationGrants, Pipelines, OutputExceptions, Requests int
}

type ArchiveResult struct {
	Project tenancy.Project
	Revoked Revoked
	// CancelledBuilds and ClosedUploads were in flight; CancelErrors lists builds that could not be cancelled.
	CancelledBuilds, ClosedUploads int
	CancelErrors                   []string
}

type PurgeResult struct {
	Project                                tenancy.Project
	Snapshots, Builds                      int
	RemovedBlobs, KeptBlobs                int
	ClearedDiscoveries, ClearedConversions int
}

func (s *Service) load(ctx context.Context, p authz.Principal, id string) (tenancy.Project, error) {
	pr, err := s.tenancy.GetProject(ctx, p, readers, id)
	if err != nil {
		return tenancy.Project{}, err
	}
	return pr, nil
}

func resource(pr tenancy.Project) authz.Resource {
	return authz.Resource{OrgID: pr.OrgID, TeamID: pr.TeamID, ProjectID: pr.ID}
}

// Archive hides the project, refuses new work for it, revokes its grants and closes its pending requests in one
// transaction, then cancels its unfinished builds and closes its open uploads. Restoring does not return the grants.
func (s *Service) Archive(ctx context.Context, p authz.Principal, id, reason, requestID string) (ArchiveResult, error) {
	pr, err := s.load(ctx, p, id)
	if err != nil {
		return ArchiveResult{}, err
	}
	if !authz.Can(p, managers, resource(pr)) {
		return ArchiveResult{}, tenancy.ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 1024 {
		return ArchiveResult{}, ErrReason
	}
	why := archivedReason
	if reason != "" {
		why += ": " + reason
	}
	now := s.now().UTC()
	var res ArchiveResult
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE projects SET state = ?, archived_by = ?, archived_at = ?, updated_at = ? WHERE id = ? AND state = ?`),
			tenancy.ProjectArchived, p.ID, now, now, id, tenancy.ProjectActive)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			if pr.State == tenancy.ProjectPurged {
				return ErrPurged
			}
			return ErrNotActive
		}
		rv, err := s.revoke(ctx, tx, p, pr, why, now, requestID)
		if err != nil {
			return err
		}
		res.Revoked = rv
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "project.archive", ResourceType: "project", ResourceID: id, Outcome: "succeeded", Reason: why, RequestID: requestID})
	})
	if err != nil {
		return ArchiveResult{}, err
	}
	res.CancelledBuilds, res.CancelErrors = s.cancelBuilds(ctx, p, id, requestID)
	if res.ClosedUploads, err = s.sources.ExpireProject(ctx, id); err != nil {
		res.CancelErrors = append(res.CancelErrors, "open uploads: "+err.Error())
	}
	res.Project, err = s.tenancy.GetProject(ctx, p, readers, id)
	return res, err
}

func (s *Service) revoke(ctx context.Context, tx *sql.Tx, p authz.Principal, pr tenancy.Project, why string, now time.Time, requestID string) (Revoked, error) {
	var rv Revoked
	each := func(count *int, query string, args []any, update string, updateArgs func(id string) []any, action, resourceType, outcome string) error {
		ids, err := column(ctx, tx, s.db.Rebind(query), args...)
		if err != nil {
			return err
		}
		for _, rid := range ids {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(update), updateArgs(rid)...); err != nil {
				return err
			}
			if err := audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: action, ResourceType: resourceType, ResourceID: rid, Outcome: outcome, Reason: why, RequestID: requestID}); err != nil {
				return err
			}
		}
		*count += len(ids)
		return nil
	}
	project := []any{pr.ID}
	steps := []func() error{
		func() error {
			return each(&rv.SecretGrants, `SELECT id FROM secret_grants WHERE project_id = ? AND status = 'active'`, project,
				`UPDATE secret_grants SET status = 'revoked', revoked_by = ?, revoked_at = ? WHERE id = ?`, func(id string) []any { return []any{p.ID, now, id} },
				"secret.grant.revoke", "secret-grant", "succeeded")
		},
		func() error {
			return each(&rv.SecretBindings, `SELECT id FROM secret_bindings WHERE project_id = ? AND status = 'active'`, project,
				`UPDATE secret_bindings SET status = 'disabled', version = version + 1, reason = ?, updated_by = ?, updated_at = ? WHERE id = ?`, func(id string) []any { return []any{why, p.ID, now, id} },
				"secret.binding.disable", "secret-binding", "succeeded")
		},
		func() error {
			return each(&rv.EgressAllowances, `SELECT id FROM project_egress_allowances WHERE project_id = ? AND status = 'active'`, project,
				`UPDATE project_egress_allowances SET status = 'revoked', revoked_by = ?, revoked_at = ?, revoke_reason = ? WHERE id = ?`, func(id string) []any { return []any{p.ID, now, why, id} },
				"egress.allowance.revoke", "project-egress-allowance", "revoked")
		},
		func() error {
			return each(&rv.FederationGrants, `SELECT id FROM federation_grants WHERE project_id = ? AND revoked_at IS NULL`, project,
				`UPDATE federation_grants SET revoked_at = ? WHERE id = ?`, func(id string) []any { return []any{now, id} },
				"federation.grant.revoke", "federation-grant", "succeeded")
		},
		func() error {
			return each(&rv.Pipelines, `SELECT id FROM pipelines WHERE project_id = ? AND enabled = ?`, []any{pr.ID, true},
				`UPDATE pipelines SET enabled = ?, updated_at = ? WHERE id = ?`, func(id string) []any { return []any{false, now, id} },
				"pipeline.disable", "pipeline", "succeeded")
		},
		func() error {
			return each(&rv.OutputExceptions, `SELECT id FROM output_exceptions WHERE project_id = ? AND state IN ('requested', 'approved')`, project,
				`UPDATE output_exceptions SET state = 'revoked', decided_by = ?, decision_reason = ?, decided_at = ? WHERE id = ?`, func(id string) []any { return []any{p.ID, why, now, id} },
				"exception.revoke", "output_exception", "succeeded")
		},
		func() error {
			n, err := s.closeDestinations(ctx, tx, p, pr, why, requestID)
			rv.Destinations = n
			return err
		},
		func() error {
			n, err := s.approvals.CloseProject(ctx, tx, p, pr.ID, why, requestID)
			rv.Requests = n
			return err
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return Revoked{}, err
		}
	}
	return rv, nil
}

// closeDestinations removes the project from every publication destination open to it.
func (s *Service) closeDestinations(ctx context.Context, tx *sql.Tx, p authz.Principal, pr tenancy.Project, why, requestID string) (int, error) {
	rows, err := tx.QueryContext(ctx, s.db.Rebind(`SELECT id, allowed_project_ids FROM destinations WHERE org_id = ?`), pr.OrgID)
	if err != nil {
		return 0, err
	}
	changed := map[string][]string{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		var allowed []string
		if json.Unmarshal([]byte(raw), &allowed) != nil || !slices.Contains(allowed, pr.ID) {
			continue
		}
		changed[id] = slices.DeleteFunc(allowed, func(x string) bool { return x == pr.ID })
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for id, allowed := range changed {
		raw, _ := json.Marshal(allowed)
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE destinations SET allowed_project_ids = ? WHERE id = ?`), string(raw), id); err != nil {
			return 0, err
		}
		if err := audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "destination.update", ResourceType: "destination", ResourceID: id, Outcome: "succeeded",
			Reason: fmt.Sprintf("closed to project %s: %s", pr.ID, why), RequestID: requestID}); err != nil {
			return 0, err
		}
	}
	return len(changed), nil
}

// cancelBuilds cancels the project's unfinished builds with the ordinary cancel path, so workers are told to stop
// and the attempts are abandoned.
func (s *Service) cancelBuilds(ctx context.Context, p authz.Principal, projectID, requestID string) (int, []string) {
	ids, err := column(ctx, s.db.SQL, s.db.Rebind(`SELECT id FROM build_runs WHERE project_id = ? AND state NOT IN (?, ?, ?, ?, ?, ?)`),
		projectID, builds.Blocked, builds.Succeeded, builds.Failed, builds.TimedOut, builds.Cancelled, builds.Expired)
	if err != nil {
		return 0, []string{"listing builds: " + err.Error()}
	}
	n := 0
	var failed []string
	for _, id := range ids {
		_, err := s.builds.Cancel(ctx, p, managers, id, requestID)
		switch {
		case err == nil:
			n++
		case errors.Is(err, builds.ErrTerminal):
		default:
			failed = append(failed, id+": "+err.Error())
		}
	}
	return n, failed
}

// Restore makes an archived project usable again, with no grants.
func (s *Service) Restore(ctx context.Context, p authz.Principal, id, requestID string) (tenancy.Project, error) {
	pr, err := s.load(ctx, p, id)
	if err != nil {
		return tenancy.Project{}, err
	}
	if !authz.Can(p, managers, resource(pr)) {
		return tenancy.Project{}, tenancy.ErrForbidden
	}
	now := s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE projects SET state = ?, archived_by = NULL, archived_at = NULL, updated_at = ? WHERE id = ? AND state = ?`),
			tenancy.ProjectActive, now, id, tenancy.ProjectArchived)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			var state string
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT state FROM projects WHERE id = ?`), id).Scan(&state); err != nil {
				return err
			}
			if state == tenancy.ProjectPurged {
				return ErrPurged
			}
			return ErrNotArchived
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "project.restore", ResourceType: "project", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
	if err != nil {
		return tenancy.Project{}, err
	}
	return s.tenancy.GetProject(ctx, p, readers, id)
}

var unfinished = []struct{ what, query string }{
	{"a build", `SELECT COUNT(*) FROM build_runs WHERE project_id = ? AND state NOT IN ('blocked', 'succeeded', 'failed', 'timed_out', 'cancelled', 'expired')`},
	{"a validation", `SELECT COUNT(*) FROM validation_runs WHERE project_id = ? AND state IN ('pending', 'running')`},
	{"a publication", `SELECT COUNT(*) FROM publications WHERE project_id = ? AND state IN ('requested', 'approved', 'gate_check', 'publishing', 'verifying')`},
	{"a discovery", `SELECT COUNT(*) FROM discoveries WHERE project_id = ? AND status = 'running'`},
	{"a conversion", `SELECT COUNT(*) FROM kube_conversion_runs WHERE state IN ('queued', 'assigned', 'running') AND conversion_id IN (SELECT id FROM kube_conversions WHERE project_id = ?)`},
	{"an upload", `SELECT COUNT(*) FROM upload_sessions WHERE project_id = ? AND status = 'committing'`},
}

// Purge deletes from internal storage everything an archived project stored: uploaded source, build images, logs,
// reports and SBOMs, cache, discovery results and conversion output. It keeps the project row, every record with its
// IDs, digests and times, and the audit trail. Content another project or a hold still uses is kept. Images already
// pushed to external registries are not touched. A purge that fails part-way can be run again.
func (s *Service) Purge(ctx context.Context, p authz.Principal, id, confirm, requestID string) (PurgeResult, error) {
	pr, err := s.load(ctx, p, id)
	if err != nil {
		return PurgeResult{}, err
	}
	if !p.IsOrgAdmin() {
		return PurgeResult{}, tenancy.ErrForbidden
	}
	if pr.State == tenancy.ProjectActive {
		return PurgeResult{}, ErrNotArchived
	}
	if confirm != pr.Name {
		return PurgeResult{}, ErrConfirm
	}
	for _, u := range unfinished {
		var n int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(u.query), id).Scan(&n); err != nil {
			return PurgeResult{}, err
		}
		if n > 0 {
			return PurgeResult{}, fmt.Errorf("%w: %s is still running; wait for it to finish and purge again", ErrBusy, u.what)
		}
	}
	var res PurgeResult
	now := s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO source_snapshot_deletions (snapshot_id, deleted_by, deleted_at)
			SELECT id, ?, ? FROM source_snapshots WHERE project_id = ? AND id NOT IN (SELECT snapshot_id FROM source_snapshot_deletions)`), p.ID, now, id)
		if err != nil {
			return err
		}
		res.Snapshots = affected(r)
		if r, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO build_run_deletions (run_id, org_id, deleted_by, reason, deleted_at)
			SELECT id, org_id, ?, 'project purged', ? FROM build_runs WHERE project_id = ? AND id NOT IN (SELECT run_id FROM build_run_deletions)`), p.ID, now, id); err != nil {
			return err
		}
		res.Builds = affected(r)
		if r, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE discoveries SET result = NULL WHERE project_id = ? AND result IS NOT NULL`), id); err != nil {
			return err
		}
		res.ClearedDiscoveries = affected(r)
		if r, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE kube_conversion_files SET content = '' WHERE content <> '' AND conversion_id IN (SELECT id FROM kube_conversions WHERE project_id = ?)`), id); err != nil {
			return err
		}
		res.ClearedConversions = affected(r)
		for _, q := range []string{
			`DELETE FROM cache_entries WHERE project_id = ? OR run_id IN (SELECT id FROM build_runs WHERE project_id = ?)`,
			`DELETE FROM build_live_logs WHERE attempt_id IN (SELECT a.id FROM build_attempts a JOIN build_runs r ON r.id = a.run_id WHERE r.project_id = ?)`,
			`DELETE FROM upload_chunks WHERE upload_id IN (SELECT id FROM upload_sessions WHERE project_id = ?)`,
			`DELETE FROM wizard_drafts WHERE project_id = ?`,
		} {
			args := []any{id}
			if strings.Count(q, "?") == 2 {
				args = append(args, id)
			}
			if _, err := tx.ExecContext(ctx, s.db.Rebind(q), args...); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE projects SET state = ?, purged_by = COALESCE(purged_by, ?), purged_at = COALESCE(purged_at, ?), updated_at = ? WHERE id = ?`),
			tenancy.ProjectPurged, p.ID, now, now, id); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "project.purge", ResourceType: "project", ResourceID: id, Outcome: "succeeded",
			Reason: fmt.Sprintf("%d uploads, %d builds marked deleted", res.Snapshots, res.Builds), RequestID: requestID})
	})
	if err != nil {
		return PurgeResult{}, err
	}
	digests, err := s.ownedBlobs(ctx, id)
	if err == nil {
		res.RemovedBlobs, res.KeptBlobs, err = s.artifacts.RemoveUnreferenced(ctx, digests)
	}
	outcome, detail := "succeeded", fmt.Sprintf("%d stored files removed, %d kept because something else uses them", res.RemovedBlobs, res.KeptBlobs)
	if err != nil {
		outcome, detail = "failed", err.Error()
	}
	if aerr := s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "project.purge.storage", ResourceType: "project", ResourceID: id, Outcome: outcome, Reason: detail, RequestID: requestID})
	}); aerr != nil && err == nil {
		err = aerr
	}
	if err != nil {
		return res, fmt.Errorf("%w: %v", ErrIncomplete, err)
	}
	res.Project, err = s.tenancy.GetProject(ctx, p, readers, id)
	return res, err
}

// ownedBlobs lists every blob the project's records name: snapshot manifests and their files, build outputs and
// validation outputs.
func (s *Service) ownedBlobs(ctx context.Context, projectID string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, q := range []string{
		`SELECT a.digest FROM build_artifacts a JOIN build_runs r ON r.id = a.run_id WHERE r.project_id = ?`,
		`SELECT artifact_digest FROM build_runs WHERE project_id = ? AND artifact_digest IS NOT NULL`,
		`SELECT c.output_digest FROM check_results c JOIN validation_runs v ON v.id = c.validation_run_id WHERE v.project_id = ? AND c.output_digest IS NOT NULL`,
	} {
		list, err := column(ctx, s.db.SQL, s.db.Rebind(q), projectID)
		if err != nil {
			return nil, err
		}
		for _, d := range list {
			add(d)
		}
	}
	manifests, err := column(ctx, s.db.SQL, s.db.Rebind(`SELECT digest FROM source_snapshots WHERE project_id = ?`), projectID)
	if err != nil {
		return nil, err
	}
	blobs := s.artifacts.Blobs()
	for _, d := range manifests {
		raw, err := blobs.ReadAll(d, 256<<20)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var m struct {
			Files []struct {
				Digest string `json:"digest"`
			} `json:"files"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		for _, f := range m.Files {
			add(f.Digest)
		}
	}
	// Manifests go last so a purge that stops early can still find the files they list.
	for _, d := range manifests {
		add(d)
	}
	return out, nil
}

type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func column(ctx context.Context, q queryer, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func affected(r sql.Result) int {
	n, _ := r.RowsAffected()
	return int(n)
}
