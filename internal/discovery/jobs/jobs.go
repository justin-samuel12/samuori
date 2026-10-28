// Package jobs runs discovery for a snapshot in the sandbox and records validated results.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/discovery"
	"samuori.invalid/samuori/internal/discovery/sandbox"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/operations"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/tenancy"
)

const (
	Running   = "running"
	Completed = "completed"
	Failed    = "failed"

	maxTotalContent = 32 << 20
	maxListedFiles  = 50_000
)

var ErrNotFound = errors.New("not found")

type Discovery struct {
	ID, ProjectID, SnapshotID, Status string
	Result                            discovery.Result
	Sandbox                           sandbox.Sandbox
	ErrorCode, ErrorMessage           string
	CreatedAt                         time.Time
}

type Service struct {
	db       *db.DB
	sources  *sources.Service
	ops      *operations.Store
	runner   sandbox.Runner
	policies *policy.Store
	log      *slog.Logger
	wg       sync.WaitGroup
	sem      chan struct{}
	owner    cluster.Ownership
}

func NewService(d *db.DB, src *sources.Service, ops *operations.Store, runner sandbox.Runner, policies *policy.Store, log *slog.Logger) *Service {
	return &Service{db: d, sources: src, ops: ops, runner: runner, policies: policies, log: log, sem: make(chan struct{}, 2), owner: &cluster.Solo{}}
}

// Analyse runs discovery over in, which admission assembles, in the sandbox.
func (s *Service) Analyse(ctx context.Context, in discovery.Input) (discovery.Result, error) {
	res, _, err := s.runner.Run(ctx, in)
	return res, err
}

func (s *Service) Wait() { s.wg.Wait() }

// Start records a discovery for a snapshot the caller can read and runs it in the background.
func (s *Service) Start(ctx context.Context, p authz.Principal, roles []authz.Role, snapshotID, requestID string) (operations.Operation, error) {
	snap, err := s.sources.GetSnapshot(ctx, p, roles, snapshotID)
	if err != nil {
		return operations.Operation{}, err
	}
	if snap.DeletedAt != nil {
		return operations.Operation{}, sources.ErrDeleted
	}
	var op operations.Operation
	id := ids.New()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tenancy.RequireActive(ctx, s.db, tx, snap.ProjectID); err != nil {
			return err
		}
		var created bool
		op, created, err = s.ops.Begin(ctx, tx, p, "discovery", id)
		if err != nil || !created {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO discoveries (id, org_id, project_id, snapshot_id, status, created_by, created_at, owner_controller) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			id, p.OrgID, snap.ProjectID, snap.ID, Running, p.ID, time.Now().UTC(), s.owner.ControllerID()); err != nil {
			return err
		}
		if err := s.ops.Finish(ctx, tx, op.ID, operations.Running, "discovery", id, "", ""); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "discovery.start", ResourceType: "discovery", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
	if err != nil {
		return operations.Operation{}, err
	}
	op.Status, op.ResourceType, op.ResourceID = operations.Running, "discovery", id
	s.wg.Add(1)
	go s.run(p, snap, id, op.ID)
	return op, nil
}

func (s *Service) run(p authz.Principal, snap sources.Snapshot, id, opID string) {
	defer s.wg.Done()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx := context.Background()
	in, err := s.input(ctx, p.OrgID, snap)
	var res discovery.Result
	var sb sandbox.Sandbox
	if err == nil {
		res, sb, err = s.runner.Run(ctx, in)
	}
	status, code, msg := Completed, "", ""
	if err != nil {
		status, code, msg = Failed, "discovery_failed", "Discovery could not finish. The source was not changed; you can describe the build yourself or try again."
		s.log.Error("discovery failed", "discovery", id, "snapshot", snap.ID, "error", err)
	}
	resultJSON, _ := json.Marshal(res)
	sandboxJSON, _ := json.Marshal(sb)
	opStatus := operations.Succeeded
	if status == Failed {
		opStatus = operations.Failed
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE discoveries SET status = ?, result = ?, sandbox = ?, error_code = ?, error_message = ?, completed_at = ? WHERE id = ? AND status = ? AND owner_controller = ?`),
			status, string(resultJSON), string(sandboxJSON), nullable(code), nullable(msg), time.Now().UTC(), id, Running, s.owner.ControllerID())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errSuperseded
		}
		if err := s.ops.Finish(ctx, tx, opID, opStatus, "discovery", id, code, msg); err != nil {
			return err
		}
		outcome := "succeeded"
		if status == Failed {
			outcome = "failed"
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "discovery.complete", ResourceType: "discovery", ResourceID: id, Outcome: outcome})
	})
	if err != nil && !errors.Is(err, errSuperseded) {
		s.log.Error("recording discovery", "discovery", id, "error", err)
	}
}

// errSuperseded means the discovery was already closed as interrupted because its controller was presumed gone.
var errSuperseded = errors.New("the discovery was closed by another controller")

// UseOwnership records which controller runs each discovery, so that only discoveries of controllers that are gone
// are closed as interrupted.
func (s *Service) UseOwnership(o cluster.Ownership) { s.owner = o }

// input sends the full path list and the content of files detectors read, within fixed budgets.
func (s *Service) input(ctx context.Context, orgID string, snap sources.Snapshot) (discovery.Input, error) {
	m, err := s.sources.Manifest(snap)
	if err != nil {
		return discovery.Input{}, err
	}
	eff, err := s.policies.Effective(ctx, orgID)
	if err != nil {
		return discovery.Input{}, err
	}
	in := discovery.Input{ApprovedFrontends: eff.Frontends.Approved}
	var budget int64 = maxTotalContent
	for i, f := range m.Files {
		if i >= maxListedFiles {
			break
		}
		file := discovery.InputFile{Path: f.Path, Size: f.Size}
		if f.Size <= discovery.MaxContent(f.Path) && f.Size <= budget {
			data, err := s.sources.ReadFile(f)
			if err != nil {
				return discovery.Input{}, err
			}
			file.Content = data
			if file.Content == nil {
				file.Content = []byte{}
			}
			budget -= f.Size
		}
		in.Files = append(in.Files, file)
	}
	return in, nil
}

// List returns a snapshot's discoveries, newest first, without results.
func (s *Service) List(ctx context.Context, p authz.Principal, roles []authz.Role, snapshotID string) ([]Discovery, error) {
	snap, err := s.sources.GetSnapshot(ctx, p, roles, snapshotID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, status, error_code, error_message, created_at FROM discoveries WHERE snapshot_id = ? AND org_id = ? ORDER BY id DESC LIMIT 50`), snap.ID, p.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Discovery
	for rows.Next() {
		d := Discovery{ProjectID: snap.ProjectID, SnapshotID: snap.ID}
		var code, msg sql.NullString
		if err := rows.Scan(&d.ID, &d.Status, &code, &msg, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.ErrorCode, d.ErrorMessage = code.String, msg.String
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Service) Get(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (Discovery, error) {
	var d Discovery
	var result, sb, code, msg sql.NullString
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT project_id, snapshot_id, status, result, sandbox, error_code, error_message, created_at FROM discoveries WHERE id = ? AND org_id = ?`), id, p.OrgID).
		Scan(&d.ProjectID, &d.SnapshotID, &d.Status, &result, &sb, &code, &msg, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Discovery{}, ErrNotFound
	}
	if err != nil {
		return Discovery{}, err
	}
	if _, err := s.sources.GetSnapshot(ctx, p, roles, d.SnapshotID); err != nil {
		return Discovery{}, ErrNotFound
	}
	d.ID, d.ErrorCode, d.ErrorMessage = id, code.String, msg.String
	if result.Valid {
		_ = json.Unmarshal([]byte(result.String), &d.Result)
	}
	if sb.Valid {
		_ = json.Unmarshal([]byte(sb.String), &d.Sandbox)
	}
	return d, nil
}

// Reconcile fails discoveries whose controller is gone; starting another is cheap. Discoveries running on live
// controllers are left alone.
func (s *Service) Reconcile(ctx context.Context) error {
	const msg = "The server running this discovery stopped; start it again."
	orphaned, args := s.owner.Orphaned("owner_controller")
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id FROM discoveries WHERE status = ? AND `+orphaned), append([]any{Running}, args...)...)
	if err != nil {
		return err
	}
	var stuck []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		stuck = append(stuck, id)
	}
	rows.Close()
	for _, id := range stuck {
		err := s.db.InTx(ctx, func(tx *sql.Tx) error {
			now := time.Now().UTC()
			res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE discoveries SET status = ?, error_code = 'interrupted', error_message = ?, completed_at = ? WHERE id = ? AND status = ?`), Failed, msg, now, id, Running)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil
			}
			_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE operations SET status = ?, error_code = 'interrupted', error_message = ?, updated_at = ? WHERE kind = 'discovery' AND resource_id = ? AND status IN (?, ?)`),
				operations.Failed, msg, now, id, operations.Pending, operations.Running)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// RunReconciler closes discoveries of departed controllers until ctx ends.
func (s *Service) RunReconciler(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("reconciling discoveries", "error", err)
			}
		}
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
