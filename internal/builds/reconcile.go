package builds

import (
	"context"
	"database/sql"
	"time"
)

// Reconcile repairs state the database can prove: leases past their deadline are abandoned (requeued while the
// retry budget lasts, otherwise failed as infrastructure) and queued runs past their deadline expire.
func (s *Service) Reconcile(ctx context.Context) error {
	now := s.now().UTC()
	expired, err := s.abandon(ctx, `lease_deadline < ?`, []any{now}, "lease expired: the worker stopped responding", "lease expired")
	if err != nil {
		return err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id FROM build_runs WHERE state = ? AND queue_deadline < ? LIMIT 500`), Queued, now)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		stale = append(stale, id)
	}
	rows.Close()
	n := 0
	for _, id := range stale {
		err := s.db.InTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE build_runs SET state = ?, updated_at = ? WHERE id = ? AND state = ? AND queue_deadline < ?`), Expired, now, id, Queued, now)
			if err != nil {
				return err
			}
			if c, _ := res.RowsAffected(); c != 1 {
				return nil
			}
			n++
			return s.event(ctx, tx, id, "state", map[string]string{"state": Expired, "reason": "queue deadline passed"})
		})
		if err != nil {
			return err
		}
	}
	if n > 0 || expired > 0 {
		s.wake()
	}
	// Live output is only for attempts still running; the final masked log replaces it.
	_, err = s.db.SQL.ExecContext(ctx, s.db.Rebind(`DELETE FROM build_live_logs WHERE created_at < ? OR attempt_id IN (SELECT id FROM build_attempts WHERE ended_at IS NOT NULL)`), now.Add(-24*time.Hour))
	return err
}

// FenceWorker ends every lease a revoked worker holds at once: its runs are requeued (or failed when out of
// attempts) under a new generation, so any late report from that worker is rejected as stale (SEC-ID-03).
func (s *Service) FenceWorker(ctx context.Context, workerID, reason string) error {
	n, err := s.abandon(ctx, `lease_worker_id = ?`, []any{workerID}, "worker revoked: "+reason, "worker revoked")
	if n > 0 {
		s.wake()
	}
	return err
}

// abandon requeues or fails the active runs matching where, recording detail on their attempts.
func (s *Service) abandon(ctx context.Context, where string, args []any, detail, event string) (int, error) {
	now := s.now().UTC()
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, generation, current_attempt_id, attempts_used, max_attempts FROM build_runs WHERE state IN (?, ?, ?, ?) AND `+where), append([]any{Assigned, Preparing, Building, Committing}, args...)...)
	if err != nil {
		return 0, err
	}
	type lost struct {
		id, attempt    string
		gen            int64
		used, maxTotal int
	}
	var expired []lost
	for rows.Next() {
		var l lost
		var attempt sql.NullString
		if err := rows.Scan(&l.id, &l.gen, &attempt, &l.used, &l.maxTotal); err != nil {
			rows.Close()
			return 0, err
		}
		l.attempt = attempt.String
		expired = append(expired, l)
	}
	rows.Close()
	for _, l := range expired {
		err := s.db.InTx(ctx, func(tx *sql.Tx) error {
			next, extra := Queued, `, lease_worker_id = NULL, lease_deadline = NULL`
			if l.used >= l.maxTotal {
				next, extra = Failed, `, failure_class = 'infrastructure', lease_deadline = NULL`
			}
			res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE build_runs SET state = ?, updated_at = ?`+extra+` WHERE id = ? AND generation = ? AND `+where), append([]any{next, now, l.id, l.gen}, args...)...)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE build_attempts SET state = 'abandoned', failure_class = 'infrastructure', detail = ?, ended_at = ? WHERE id = ?`), detail, now, l.attempt); err != nil {
				return err
			}
			if err := s.event(ctx, tx, l.id, "state", map[string]string{"state": next, "reason": event}); err != nil {
				return err
			}
			if next == Queued {
				return s.enqueue(ctx, tx, SubjectQueued, l.id, l.gen)
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return len(expired), nil
}

// Rewake enqueues fresh wake-ups for queued and assigned runs. It runs at start-up so a lost message store never
// strands work; fresh message IDs keep the duplicate window from suppressing them.
func (s *Service) Rewake(ctx context.Context) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, s.db.Rebind(`SELECT id, state, generation FROM build_runs WHERE state IN (?, ?)`), Queued, Assigned)
		if err != nil {
			return err
		}
		type item struct {
			id, state string
			gen       int64
		}
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.state, &it.gen); err != nil {
				rows.Close()
				return err
			}
			items = append(items, it)
		}
		rows.Close()
		for _, it := range items {
			subject := SubjectQueued
			if it.state == Assigned {
				subject = SubjectAssign
			}
			if err := s.enqueue(ctx, tx, subject, it.id, it.gen); err != nil {
				return err
			}
		}
		return nil
	})
}

// RunReconciler repeats Reconcile until ctx ends.
func (s *Service) RunReconciler(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
				onErr(err)
			}
		}
	}
}
