// Package observability summarizes the controller's operational state from the database for the diagnostics API and
// the metrics endpoint (docs/architecture/observability.md). It reads only; it never changes state.
package observability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/health"
)

// StaleAfter is how long an enrolled worker may go without a heartbeat before it is reported stale.
const StaleAfter = 2 * time.Minute

type Count struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

type Snapshot struct {
	At           time.Time     `json:"at"`
	Health       health.Report `json:"health"`
	Builds       Builds        `json:"builds"`
	Validations  States        `json:"validations"`
	Publications States        `json:"publications"`
	Workers      Workers       `json:"workers"`
	Registries   []Registry    `json:"registries"`
	Outbox       Outbox        `json:"outbox"`
}

// States counts records by current state, and those that reached a terminal state in the last 24 hours.
type States struct {
	ByState map[string]int `json:"byState"`
	Last24h map[string]int `json:"last24h"`
}

type Builds struct {
	States
	QueuedOldestSeconds float64 `json:"queuedOldestSeconds"`
	FailureClasses24h   []Count `json:"failureClasses24h"`
}

type Workers struct {
	ByEnrollment map[string]int `json:"byEnrollment"`
	Connected    int            `json:"connected"`
	Stale        int            `json:"stale"`
	Pools        []Pool         `json:"pools"`
}

type Pool struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Location         string `json:"location"`
	IsolationProfile string `json:"isolationProfile"`
	ProfileApproved  bool   `json:"profileApproved"`
	Enrolled         int    `json:"enrolled"`
	Connected        int    `json:"connected"`
	Stale            int    `json:"stale"`
}

type Registry struct {
	Registry string     `json:"registry"`
	Profile  string     `json:"profile"`
	Status   string     `json:"status"`
	TestedAt *time.Time `json:"testedAt,omitempty"`
}

type Outbox struct {
	Pending       int     `json:"pending"`
	OldestSeconds float64 `json:"oldestSeconds"`
}

// Source builds snapshots. Connected reports whether a worker holds a live channel; Health may be nil.
type Source struct {
	DB        *db.DB
	Health    *health.Registry
	Connected func(workerID string) bool
	Now       func() time.Time
}

var terminalBuilds = []string{"succeeded", "failed", "timed_out", "cancelled", "expired", "blocked"}

// Take returns the state of one organization, or of the whole installation when orgID is empty.
func (s Source) Take(ctx context.Context, orgID string) (Snapshot, error) {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	day := now.Add(-24 * time.Hour)
	snap := Snapshot{At: now}
	if s.Health != nil {
		snap.Health = s.Health.Evaluate(ctx)
	}
	scope, args := "", []any{}
	if orgID != "" {
		scope, args = " AND org_id = ?", []any{orgID}
	}
	var err error
	if snap.Builds.ByState, err = s.counts(ctx, `SELECT state, COUNT(*) FROM build_runs WHERE 1 = 1`+scope+` GROUP BY state`, args...); err != nil {
		return snap, err
	}
	if snap.Builds.Last24h, err = s.counts(ctx, `SELECT state, COUNT(*) FROM build_runs WHERE updated_at >= ? AND state IN (?, ?, ?, ?, ?, ?)`+scope+` GROUP BY state`,
		append([]any{day, terminalBuilds[0], terminalBuilds[1], terminalBuilds[2], terminalBuilds[3], terminalBuilds[4], terminalBuilds[5]}, args...)...); err != nil {
		return snap, err
	}
	if snap.Builds.QueuedOldestSeconds, err = s.oldest(ctx, `SELECT created_at FROM build_runs WHERE state = 'queued'`+scope+` ORDER BY created_at LIMIT 1`, now, args...); err != nil {
		return snap, err
	}
	failures, err := s.counts(ctx, `SELECT COALESCE(failure_class, 'unknown'), COUNT(*) FROM build_runs WHERE updated_at >= ? AND state IN ('failed', 'timed_out')`+scope+` GROUP BY COALESCE(failure_class, 'unknown')`, append([]any{day}, args...)...)
	if err != nil {
		return snap, err
	}
	snap.Builds.FailureClasses24h = sorted(failures, 5)

	if snap.Validations.ByState, err = s.counts(ctx, `SELECT state, COUNT(*) FROM validation_runs WHERE 1 = 1`+scope+` GROUP BY state`, args...); err != nil {
		return snap, err
	}
	if snap.Validations.Last24h, err = s.counts(ctx, `SELECT COALESCE(verdict, state), COUNT(*) FROM validation_runs WHERE finished_at >= ?`+scope+` GROUP BY COALESCE(verdict, state)`, append([]any{day}, args...)...); err != nil {
		return snap, err
	}
	if snap.Publications.ByState, err = s.counts(ctx, `SELECT state, COUNT(*) FROM publications WHERE 1 = 1`+scope+` GROUP BY state`, args...); err != nil {
		return snap, err
	}
	if snap.Publications.Last24h, err = s.counts(ctx, `SELECT state, COUNT(*) FROM publications WHERE updated_at >= ? AND state IN ('published', 'blocked', 'failed', 'rejected', 'expired', 'cancelled')`+scope+` GROUP BY state`, append([]any{day}, args...)...); err != nil {
		return snap, err
	}
	if snap.Workers, err = s.workers(ctx, now, scope, args); err != nil {
		return snap, err
	}
	if snap.Registries, err = s.registries(ctx, scope, args); err != nil {
		return snap, err
	}
	if err := s.DB.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE published_at IS NULL`).Scan(&snap.Outbox.Pending); err != nil {
		return snap, err
	}
	if snap.Outbox.OldestSeconds, err = s.oldest(ctx, `SELECT created_at FROM outbox WHERE published_at IS NULL ORDER BY seq LIMIT 1`, now); err != nil {
		return snap, err
	}
	return snap, nil
}

func (s Source) counts(ctx context.Context, q string, args ...any) (map[string]int, error) {
	rows, err := s.DB.SQL.QueryContext(ctx, s.DB.Rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

func (s Source) oldest(ctx context.Context, q string, now time.Time, args ...any) (float64, error) {
	var t time.Time
	err := s.DB.SQL.QueryRowContext(ctx, s.DB.Rebind(q), args...).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return max(now.Sub(t).Seconds(), 0), nil
}

func (s Source) workers(ctx context.Context, now time.Time, scope string, args []any) (Workers, error) {
	w := Workers{ByEnrollment: map[string]int{}, Pools: []Pool{}}
	pools := map[string]int{}
	rows, err := s.DB.SQL.QueryContext(ctx, s.DB.Rebind(`SELECT id, name, location, isolation_profile, profile_approved FROM worker_pools WHERE 1 = 1`+scope+` ORDER BY name`), args...)
	if err != nil {
		return w, err
	}
	for rows.Next() {
		var p Pool
		if err := rows.Scan(&p.ID, &p.Name, &p.Location, &p.IsolationProfile, &p.ProfileApproved); err != nil {
			rows.Close()
			return w, err
		}
		pools[p.ID] = len(w.Pools)
		w.Pools = append(w.Pools, p)
	}
	rows.Close()
	rows, err = s.DB.SQL.QueryContext(ctx, s.DB.Rebind(`SELECT id, COALESCE(pool_id, ''), enrollment_state, last_seen_at FROM workers WHERE 1 = 1`+scope), args...)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, pool, state string
		var seen sql.NullTime
		if err := rows.Scan(&id, &pool, &state, &seen); err != nil {
			return w, err
		}
		w.ByEnrollment[state]++
		if state != "enrolled" {
			continue
		}
		i, ok := pools[pool]
		connected := s.Connected != nil && s.Connected(id)
		stale := !seen.Valid || now.Sub(seen.Time) > StaleAfter
		if connected {
			w.Connected++
		}
		if stale {
			w.Stale++
		}
		if ok {
			w.Pools[i].Enrolled++
			if connected {
				w.Pools[i].Connected++
			}
			if stale {
				w.Pools[i].Stale++
			}
		}
	}
	return w, rows.Err()
}

func (s Source) registries(ctx context.Context, scope string, args []any) ([]Registry, error) {
	q := `SELECT r.name, p.name, p.last_test FROM registry_profiles p JOIN registries r ON r.id = p.registry_id WHERE 1 = 1`
	if scope != "" {
		q += ` AND p.org_id = ?`
	}
	rows, err := s.DB.SQL.QueryContext(ctx, s.DB.Rebind(q+` ORDER BY r.name, p.name`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Registry{}
	for rows.Next() {
		var r Registry
		var last sql.NullString
		if err := rows.Scan(&r.Registry, &r.Profile, &last); err != nil {
			return nil, err
		}
		r.Status = "untested"
		var t struct {
			Status   string    `json:"status"`
			TestedAt time.Time `json:"testedAt"`
		}
		if last.Valid && json.Unmarshal([]byte(last.String), &t) == nil && t.Status != "" {
			r.Status, r.TestedAt = t.Status, &t.TestedAt
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// sorted returns the n largest counts, ties by key.
func sorted(m map[string]int, n int) []Count {
	out := []Count{}
	for k, v := range m {
		out = append(out, Count{k, v})
	}
	slices.SortFunc(out, func(a, b Count) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out[:min(n, len(out))]
}
