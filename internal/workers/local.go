package workers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

const (
	LocalPoolName   = "local"
	LocalWorkerName = "local"
	localTokenTTL   = time.Hour
)

// LocalWorker enrolls the all-in-one host worker without manual steps (ADR 0032). Once an organization exists it
// keeps a pool named "local" and, while no local worker is enrolled, writes a single-use enrollment token and the CA
// pin into Dir, a directory only the controller and root can read. The host's worker unit takes them from there.
// A revoked local worker is never re-enrolled automatically, and the pool is never enabled automatically: the system
// creates it, not an administrator.
type LocalWorker struct {
	Store   *Store
	Dir     string
	Profile string
	Log     *slog.Logger
}

// CheckDir refuses a handoff directory that anyone but its owner can read or enter.
func (l *LocalWorker) CheckDir() error {
	info, err := os.Stat(l.Dir)
	if err != nil {
		return fmt.Errorf("localWorker.dir: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("localWorker.dir %s must be a directory with mode 0700", l.Dir)
	}
	return nil
}

func (l *LocalWorker) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := l.Reconcile(ctx); err != nil {
			l.Log.Warn("local worker enrollment", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Reconcile performs one step: nothing before setup, then the pool, then a token while none is pending.
func (l *LocalWorker) Reconcile(ctx context.Context) error {
	s := l.Store
	var org string
	err := s.db.SQL.QueryRowContext(ctx, `SELECT id FROM organizations ORDER BY created_at LIMIT 1`).Scan(&org)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	actor := authz.Principal{ID: "system:local-worker", OrgID: org, Kind: authz.System}
	pool, err := l.pool(ctx, actor)
	if err != nil {
		return err
	}
	list, err := s.List(ctx, org, "", 10_000)
	if err != nil {
		return err
	}
	now := s.now()
	for _, w := range list {
		if w.PoolID != pool.ID || w.Name != LocalWorkerName {
			continue
		}
		if w.EnrollmentState == Revoked && w.ReplacedBy == "" {
			return nil
		}
		if w.EnrollmentState == Enrolled && now.Before(w.CertExpiresAt) {
			_ = os.Remove(filepath.Join(l.Dir, "enrollment-token"))
			return nil
		}
	}
	if pending, _ := os.ReadFile(filepath.Join(l.Dir, "enrollment-token.expires")); len(pending) > 0 {
		if exp, err := time.Parse(time.RFC3339, string(pending)); err == nil && now.Before(exp) {
			if _, err := os.Stat(filepath.Join(l.Dir, "enrollment-token")); err == nil {
				return nil
			}
		}
	}
	tok, err := s.issueToken(ctx, actor, pool.ID, localTokenTTL, "local worker")
	if err != nil {
		return err
	}
	for name, body := range map[string]string{"ca-pin": tok.CAPin, "enrollment-token.expires": tok.ExpiresAt.Format(time.RFC3339), "enrollment-token": tok.Value} {
		if err := writePrivate(filepath.Join(l.Dir, name), body); err != nil {
			return err
		}
	}
	l.Log.Info("local worker enrollment token issued", "pool", pool.ID, "expires", tok.ExpiresAt)
	return nil
}

func (l *LocalWorker) pool(ctx context.Context, actor authz.Principal) (Pool, error) {
	pools, err := l.Store.ListPools(ctx, actor.OrgID)
	if err != nil {
		return Pool{}, err
	}
	for _, p := range pools {
		if p.Name == LocalPoolName {
			return p, nil
		}
	}
	return l.Store.createPool(ctx, actor, Pool{Name: LocalPoolName, IsolationProfile: l.Profile, TrustDomain: "default"}, "")
}

// writePrivate replaces name atomically with a file only its owner can read.
func writePrivate(name, body string) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), name); err != nil {
		return fmt.Errorf("local worker: %w", err)
	}
	return nil
}

// createPool records a pool in its location (the default one when none is given). A pool an administrator creates
// is approved by that administrator (ADR 0045); one the system creates is not.
func (s *Store) createPool(ctx context.Context, actor authz.Principal, p Pool, requestID string) (Pool, error) {
	p.ID, p.CreatedAt, p.ProfileApproved = ids.New(), s.now().UTC(), actor.IsOrgAdmin()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		if p.Location, err = s.resolveLocation(ctx, tx, actor.OrgID, p.Location); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO worker_pools (id, org_id, name, location, isolation_profile, trust_domain, trusted_workloads_only, profile_approved, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			p.ID, actor.OrgID, p.Name, p.Location, p.IsolationProfile, p.TrustDomain, p.TrustedWorkloadsOnly, p.ProfileApproved, p.CreatedAt); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		reason := p.IsolationProfile
		if p.ProfileApproved {
			if err := s.recordApproval(ctx, tx, actor, p.ID, true, "approved by creating the pool"); err != nil {
				return err
			}
			reason += ", approved by its creator"
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pool.create", ResourceType: "worker_pool", ResourceID: p.ID, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	return p, err
}

// EnrolledLocal maps each enrolled worker named local to its organization.
func (s *Store) EnrolledLocal(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, org_id FROM workers WHERE name = ? AND enrollment_state = ?`), LocalWorkerName, Enrolled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, org string
		if err := rows.Scan(&id, &org); err != nil {
			return nil, err
		}
		out[id] = org
	}
	return out, rows.Err()
}
