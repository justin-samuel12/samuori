// Package capacity collects disk, memory and activity across controllers and workers in one view
// (docs/operators/capacity.md).
package capacity

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"time"

	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/hostcap"
	"samuori.invalid/samuori/internal/updatestage"
)

// ReportEvery is how often a controller measures itself.
const ReportEvery = 30 * time.Second

type Service struct {
	db         *db.DB
	controller string
	dirs       []hostcap.Dir
	log        *slog.Logger
	now        func() time.Time
}

func New(d *db.DB, controllerID, dataDir string, log *slog.Logger) *Service {
	return &Service{db: d, controller: controllerID, log: log, now: time.Now, dirs: []hostcap.Dir{
		{Label: "Controller data", Path: dataDir},
		{Label: "Local artifacts", Path: filepath.Join(dataDir, "blobs")},
		{Label: "Update staging", Path: updatestage.Dir},
		{Label: "System", Path: "/"},
	}}
}

// Run records this controller's capacity until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(ReportEvery)
	defer t.Stop()
	for {
		if err := s.report(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("capacity report", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) report(ctx context.Context) error {
	raw, err := json.Marshal(hostcap.Measure(s.dirs))
	if err != nil {
		return err
	}
	_, err = s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE controllers SET capacity = ?, capacity_at = ? WHERE id = ?`), string(raw), s.now().UTC(), s.controller)
	return err
}

// Measured is a host's last report; Report is nil when the host never sent one.
type Measured struct {
	Report *hostcap.Report
	At     *time.Time
}

func measured(raw sql.NullString, at sql.NullTime) Measured {
	var m Measured
	if !raw.Valid || !at.Valid {
		return m
	}
	var r hostcap.Report
	if json.Unmarshal([]byte(raw.String), &r) == nil {
		m.Report, m.At = &r, &at.Time
	}
	return m
}

// Controllers returns every controller's last report by controller ID.
func (s *Service) Controllers(ctx context.Context) (map[string]Measured, error) {
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT id, capacity, capacity_at FROM controllers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Measured{}
	for rows.Next() {
		var id string
		var raw sql.NullString
		var at sql.NullTime
		if err := rows.Scan(&id, &raw, &at); err != nil {
			return nil, err
		}
		out[id] = measured(raw, at)
	}
	return out, rows.Err()
}

type Worker struct {
	ID, Name, Pool, State string
	LastSeen              *time.Time
	Measured
}

// Workers lists the organization's enrolled workers with their last reports.
func (s *Service) Workers(ctx context.Context, orgID string) ([]Worker, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT w.id, w.name, COALESCE(p.name, ''), w.operational_state, w.last_seen_at, w.capacity, w.capacity_at
		FROM workers w LEFT JOIN worker_pools p ON p.id = w.pool_id WHERE w.org_id = ? AND w.enrollment_state = 'enrolled' ORDER BY w.name`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Worker{}
	for rows.Next() {
		var w Worker
		var seen sql.NullTime
		var raw sql.NullString
		var at sql.NullTime
		if err := rows.Scan(&w.ID, &w.Name, &w.Pool, &w.State, &seen, &raw, &at); err != nil {
			return nil, err
		}
		if seen.Valid {
			w.LastSeen = &seen.Time
		}
		w.Measured = measured(raw, at)
		out = append(out, w)
	}
	return out, rows.Err()
}

// Build is a run that has not finished.
type Build struct {
	ID, ProjectID, Project, State, Worker string
	AttemptsUsed, MaxAttempts             int
	CreatedAt, UpdatedAt                  time.Time
	LeaseDeadline, QueueDeadline          *time.Time
}

// ActiveBuilds lists the organization's unfinished runs, oldest first.
func (s *Service) ActiveBuilds(ctx context.Context, orgID string) ([]Build, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT r.id, r.project_id, COALESCE(p.name, ''), r.state, COALESCE(w.name, ''), r.attempts_used, r.max_attempts, r.created_at, r.updated_at, r.lease_deadline, r.queue_deadline
		FROM build_runs r LEFT JOIN projects p ON p.id = r.project_id LEFT JOIN workers w ON w.id = r.lease_worker_id
		WHERE r.org_id = ? AND r.state IN ('queued', 'assigned', 'preparing', 'building', 'committing') ORDER BY r.created_at LIMIT 200`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Build{}
	for rows.Next() {
		var b Build
		var lease, queue sql.NullTime
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.Project, &b.State, &b.Worker, &b.AttemptsUsed, &b.MaxAttempts, &b.CreatedAt, &b.UpdatedAt, &lease, &queue); err != nil {
			return nil, err
		}
		if lease.Valid {
			b.LeaseDeadline = &lease.Time
		}
		if queue.Valid {
			b.QueueDeadline = &queue.Time
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Upload is an upload that has not been committed, failed or expired.
type Upload struct {
	ID, ProjectID, Project, Status, CreatedBy string
	ReceivedBytes, TotalBytes                 int64
	CreatedAt, ExpiresAt                      time.Time
}

// OpenUploads lists the organization's unfinished uploads, oldest first.
func (s *Service) OpenUploads(ctx context.Context, orgID string) ([]Upload, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT u.id, u.project_id, COALESCE(p.name, ''), u.status, COALESCE(us.username, u.created_by),
		COALESCE((SELECT SUM(c.size) FROM upload_chunks c WHERE c.upload_id = u.id), 0), u.total_bytes, u.created_at, u.expires_at
		FROM upload_sessions u LEFT JOIN projects p ON p.id = u.project_id LEFT JOIN users us ON us.id = u.created_by
		WHERE u.org_id = ? AND u.status IN ('open', 'committing') ORDER BY u.created_at LIMIT 200`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Upload{}
	for rows.Next() {
		var u Upload
		if err := rows.Scan(&u.ID, &u.ProjectID, &u.Project, &u.Status, &u.CreatedBy, &u.ReceivedBytes, &u.TotalBytes, &u.CreatedAt, &u.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
