package audit

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

// MinRetention is the youngest age at which events may be purged; the PostgreSQL purge function enforces it too.
const MinRetention = 30 * 24 * time.Hour

// exportLag keeps scheduled exports behind transactions that may still commit events with an earlier timestamp.
const exportLag = 5 * time.Minute

// Policy is an organization's audit retention and export policy (docs/architecture/observability.md#retention).
type Policy struct {
	RetentionDays   int        `json:"retentionDays"`
	ExportEnabled   bool       `json:"exportEnabled"`
	RequireExport   bool       `json:"requireExport"`
	ExportedThrough *time.Time `json:"exportedThrough,omitempty"`
	LastExportAt    *time.Time `json:"lastExportAt,omitempty"`
	LastExportError string     `json:"lastExportError,omitempty"`
	LastPurgeAt     *time.Time `json:"lastPurgeAt,omitempty"`
	LastPurgeCount  int        `json:"lastPurgeCount"`
	UpdatedAt       *time.Time `json:"updatedAt,omitempty"`
}

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

var ErrForbidden = errors.New("forbidden")

// Retention applies policies: it writes scheduled exports to Dir and purges events past retention.
type Retention struct {
	DB  *db.DB
	Dir string
	Now func() time.Time
}

func (r *Retention) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// Policy returns the organization's policy; without one, events are kept forever and nothing is exported.
func (r *Retention) Policy(ctx context.Context, orgID string) (Policy, error) {
	p := Policy{RequireExport: true}
	var through, exportAt, purgeAt, updated sql.NullTime
	var exportErr sql.NullString
	err := r.DB.SQL.QueryRowContext(ctx, r.DB.Rebind(`SELECT retention_days, export_enabled, require_export, exported_through, last_export_at, last_export_error, last_purge_at, last_purge_count, updated_at FROM audit_policies WHERE org_id = ?`), orgID).
		Scan(&p.RetentionDays, &p.ExportEnabled, &p.RequireExport, &through, &exportAt, &exportErr, &purgeAt, &p.LastPurgeCount, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	p.ExportedThrough, p.LastExportAt, p.LastPurgeAt, p.UpdatedAt = timePtr(through), timePtr(exportAt), timePtr(purgeAt), timePtr(updated)
	p.LastExportError = exportErr.String
	return p, nil
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

// SetPolicy replaces the retention and export settings. Only administrators may change them.
func (r *Retention) SetPolicy(ctx context.Context, actor authz.Principal, p Policy, requestID string) (Policy, error) {
	if !actor.IsOrgAdmin() {
		return Policy{}, ErrForbidden
	}
	if p.RetentionDays != 0 && (p.RetentionDays < int(MinRetention/(24*time.Hour)) || p.RetentionDays > 36500) {
		return Policy{}, &Invalid{"retention is 0 (keep forever) or between 30 and 36500 days"}
	}
	if p.RetentionDays > 0 && p.RequireExport && !p.ExportEnabled {
		return Policy{}, &Invalid{"purging only exported events needs scheduled export; enable it or allow purging without export"}
	}
	now := r.now()
	err := r.DB.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, r.DB.Rebind(`UPDATE audit_policies SET retention_days = ?, export_enabled = ?, require_export = ?, updated_by = ?, updated_at = ? WHERE org_id = ?`),
			p.RetentionDays, p.ExportEnabled, p.RequireExport, actor.ID, now, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := tx.ExecContext(ctx, r.DB.Rebind(`INSERT INTO audit_policies (org_id, retention_days, export_enabled, require_export, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, ?)`),
				actor.OrgID, p.RetentionDays, p.ExportEnabled, p.RequireExport, actor.ID, now); err != nil {
				return err
			}
		}
		return Append(ctx, r.DB, tx, Event{Actor: actor, Action: "audit-policy.update", ResourceType: "audit-policy", ResourceID: actor.OrgID, Outcome: "succeeded",
			Reason: fmt.Sprintf("retention %d days, scheduled export %t, purge only exported %t", p.RetentionDays, p.ExportEnabled, p.RequireExport), RequestID: requestID})
	})
	if err != nil {
		return Policy{}, err
	}
	return r.Policy(ctx, actor.OrgID)
}

// Each streams an organization's events with from <= at < until, oldest first. Consecutive windows that share a
// boundary neither skip nor repeat an event, at any timestamp precision.
func Each(ctx context.Context, d *db.DB, orgID string, from *time.Time, until time.Time, fn func(Record) error) error {
	_, err := Export(ctx, d, Filter{OrgID: orgID, Since: from, Until: &until}, math.MaxInt, fn)
	return err
}

// Run exports and purges every organization with a policy. Errors are recorded on the policy and returned.
func (r *Retention) Run(ctx context.Context) error {
	rows, err := r.DB.SQL.QueryContext(ctx, `SELECT org_id FROM audit_policies`)
	if err != nil {
		return err
	}
	var orgs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		orgs = append(orgs, id)
	}
	rows.Close()
	var errs []error
	for _, org := range orgs {
		p, err := r.Policy(ctx, org)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if p.ExportEnabled {
			if err := r.Export(ctx, org); err != nil {
				errs = append(errs, fmt.Errorf("audit export for %s: %w", org, err))
				continue
			}
		}
		if p.RetentionDays > 0 {
			if _, err := r.Purge(ctx, org); err != nil {
				errs = append(errs, fmt.Errorf("audit purge for %s: %w", org, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Export writes the events since the last scheduled export to a JSON Lines file with a SHA-256 sidecar, then
// advances the export checkpoint.
func (r *Retention) Export(ctx context.Context, orgID string) error {
	p, err := r.Policy(ctx, orgID)
	if err != nil {
		return err
	}
	until := r.now().Add(-exportLag).Truncate(time.Microsecond)
	if p.ExportedThrough != nil && !until.After(*p.ExportedThrough) {
		return nil
	}
	err = r.writeExport(ctx, orgID, p.ExportedThrough, until)
	msg := any(nil)
	if err != nil {
		msg = clip(err.Error(), 500)
	}
	q, args := `UPDATE audit_policies SET last_export_at = ?, last_export_error = ?`, []any{r.now(), msg}
	if err == nil {
		q, args = q+`, exported_through = ?`, append(args, until)
	}
	if _, uerr := r.DB.SQL.ExecContext(ctx, r.DB.Rebind(q+` WHERE org_id = ?`), append(args, orgID)...); uerr != nil {
		return errors.Join(err, uerr)
	}
	return err
}

func (r *Retention) writeExport(ctx context.Context, orgID string, from *time.Time, until time.Time) error {
	if r.Dir == "" {
		return errors.New("no audit export directory is configured")
	}
	dir := filepath.Join(r.Dir, orgID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	start := "beginning"
	if from != nil {
		start = from.UTC().Format("20060102T150405Z")
	}
	name := fmt.Sprintf("audit-%s-%s.jsonl", start, until.UTC().Format("20060102T150405Z"))
	tmp, err := os.CreateTemp(dir, ".export-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	sum := sha256.New()
	w := bufio.NewWriter(io.MultiWriter(tmp, sum))
	enc := json.NewEncoder(w)
	if err := Each(ctx, r.DB, orgID, from, until, func(rec Record) error { return enc.Encode(rec) }); err != nil {
		tmp.Close()
		return err
	}
	if err := errors.Join(w.Flush(), tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".sha256"), []byte(hex.EncodeToString(sum.Sum(nil))+"  "+name+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// Purge deletes events older than the retention period, and, when the policy requires it, only those already
// exported. Events younger than MinRetention are never purged. The purge itself is audited.
func (r *Retention) Purge(ctx context.Context, orgID string) (int64, error) {
	p, err := r.Policy(ctx, orgID)
	if err != nil || p.RetentionDays <= 0 {
		return 0, err
	}
	now := r.now()
	cutoff := now.Add(-time.Duration(p.RetentionDays) * 24 * time.Hour)
	if p.RequireExport {
		if p.ExportedThrough == nil {
			return 0, nil
		}
		if p.ExportedThrough.Before(cutoff) {
			cutoff = *p.ExportedThrough
		}
	}
	if cutoff.After(now.Add(-MinRetention)) {
		return 0, fmt.Errorf("refusing to purge events younger than %s", MinRetention)
	}
	var n int64
	err = r.DB.InTx(ctx, func(tx *sql.Tx) error {
		id := ids.New()
		if _, err := tx.ExecContext(ctx, r.DB.Rebind(`INSERT INTO audit_purges (id, org_id, cutoff, started_at) VALUES (?, ?, ?, ?)`), id, orgID, cutoff, now); err != nil {
			return err
		}
		if r.DB.Dialect == db.Postgres {
			if err := tx.QueryRowContext(ctx, `SELECT samuori_audit_purge($1, $2)`, orgID, cutoff).Scan(&n); err != nil {
				return err
			}
		} else {
			res, err := tx.ExecContext(ctx, `DELETE FROM audit_events WHERE org_id = ? AND at < ?`, orgID, cutoff)
			if err != nil {
				return err
			}
			n, _ = res.RowsAffected()
		}
		if _, err := tx.ExecContext(ctx, r.DB.Rebind(`UPDATE audit_purges SET deleted = ?, finished_at = ? WHERE id = ?`), n, now, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, r.DB.Rebind(`UPDATE audit_policies SET last_purge_at = ?, last_purge_count = ? WHERE org_id = ?`), now, n, orgID); err != nil {
			return err
		}
		return Append(ctx, r.DB, tx, Event{Actor: authz.Principal{OrgID: orgID, Kind: authz.System}, Action: "audit.purge", ResourceType: "audit-policy", ResourceID: orgID, Outcome: "succeeded",
			Reason: fmt.Sprintf("deleted %d events before %s", n, cutoff.Format(time.RFC3339))})
	})
	return n, err
}

// RunLoop applies policies every interval until ctx ends.
func (r *Retention) RunLoop(ctx context.Context, every time.Duration, onError func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := r.Run(ctx); err != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
