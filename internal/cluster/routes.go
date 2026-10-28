package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/clusterjoin"
)

const (
	routeRecheck     = 10 * time.Minute
	routeFollowUp    = 5 * time.Second
	routeFollowUpMax = 160 * time.Second
	routeRetry       = 30 * time.Second
	routeMissingWarn = 2 * time.Minute
	restartLease     = "nats-route-restart"
	restartLeaseTTL  = 2 * time.Minute
)

type routeSync struct {
	applied    string
	nextCheck  time.Time
	followUp   time.Duration
	retryAt    time.Time
	verified   bool
	missingFor map[string]time.Time
	warnedAt   time.Time
}

// RoutePlan is the NATS routes every member should have (docs/operators/high-availability.md#nats-routes): the
// controllers that recorded their NATS name and hosts whose join was accepted, without removed controllers.
// Verification is planned only while every running controller has recorded its name.
func (r *Removals) RoutePlan(ctx context.Context) (clusterjoin.RoutePlan, error) {
	plan, _, err := r.routePlan(ctx)
	return plan, err
}

// routePlan also returns the running controllers that recorded their NATS name: one starting or stopping changes the
// mesh (a joining member routes in with verification off until its own sync) even when the plan stays the same.
func (r *Removals) routePlan(ctx context.Context) (clusterjoin.RoutePlan, []string, error) {
	plan := clusterjoin.RoutePlan{Members: []string{}, Removed: []string{}, Verify: true}
	var live []string
	removed := map[string]bool{}
	if err := r.strings(ctx, `SELECT host FROM controller_removals WHERE state = 'removed' AND host <> ''`, nil, func(h string) {
		removed[strings.ToLower(h)] = true
		plan.Removed = append(plan.Removed, strings.ToLower(h))
	}); err != nil {
		return plan, nil, err
	}
	add := func(h string) {
		h = strings.ToLower(h)
		if !removed[h] && !slices.Contains(plan.Members, h) {
			plan.Members = append(plan.Members, h)
		}
	}
	if err := r.strings(ctx, `SELECT host FROM controllers WHERE host <> ''`, nil, add); err != nil {
		return plan, nil, err
	}
	now := r.m.now().UTC()
	if err := r.strings(ctx, `SELECT host FROM controller_join_requests WHERE (state = 'accepted' AND expires_at > ?) OR (state = 'completed' AND created_at > ?)`,
		[]any{now, now.Add(-24 * time.Hour)}, add); err != nil {
		return plan, nil, err
	}
	hosts, err := r.hosts(ctx)
	if err != nil {
		return plan, nil, err
	}
	for _, h := range hosts {
		switch {
		case h.live && h.name == "":
			plan.Verify = false
		case h.live:
			live = append(live, strings.ToLower(h.name))
		}
	}
	sort.Strings(plan.Members)
	sort.Strings(plan.Removed)
	sort.Strings(live)
	return plan, live, nil
}

func (r *Removals) strings(ctx context.Context, query string, args []any, each func(string)) error {
	rows, err := r.m.db.SQL.QueryContext(ctx, r.m.db.Rebind(query), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		each(s)
	}
	return rows.Err()
}

// SyncNATSRoutes runs on every controller with a NATS member and asks the host's root helper to bring the member's
// routes to the plan. It checks when the plan or the running controllers change and after a stray-route restart, again
// 5 s later doubling to 160 s, so a stray route gossip opens during a join closes within seconds, then every ten
// minutes (docs/operators/high-availability.md#nats-routes). A failed attempt is retried after 30 seconds.
func (r *Removals) SyncNATSRoutes(ctx context.Context) error {
	if r.SyncRoutes == nil || r.m.Removed() {
		return nil
	}
	plan, live, err := r.routePlan(ctx)
	if err != nil {
		return err
	}
	key, _ := json.Marshal(struct {
		Plan clusterjoin.RoutePlan `json:"plan"`
		Live []string              `json:"live"`
	}{plan, live})
	now := r.m.now()
	r.mu.Lock()
	s := &r.routes
	changed := string(key) != s.applied
	due := now.After(s.retryAt) && (changed || !now.Before(s.nextCheck))
	r.mu.Unlock()
	if !due {
		return nil
	}
	out, err := r.SyncRoutes(plan)
	if err == nil && out.Outcome == "stray" {
		var held bool
		if held, err = r.lease(ctx, restartLease, restartLeaseTTL); err == nil && held {
			plan.Restart = true
			r.m.log.Warn("NATS routes: a route to a removed member survived the reload; restarting this host's member", "stray", out.Stray)
			out, err = r.SyncRoutes(plan)
		} else if err == nil {
			err = fmt.Errorf("routes to %s survived the reload; another member is restarting, trying again", strings.Join(out.Stray, ", "))
		}
	}
	if err == nil && out.Outcome != "succeeded" && out.Outcome != "restarted" {
		err = errors.New(out.Detail)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		s.retryAt = now.Add(routeRetry)
		return err
	}
	if string(key) != s.applied || out.Verify != s.verified {
		r.m.log.Info("NATS routes brought up to date", "routes", out.Hosts, "verified", out.Verify, "waiting", out.Waiting, "detail", out.Detail)
	}
	switch {
	case changed || out.Outcome == "restarted":
		s.followUp = routeFollowUp
	case s.followUp > 0 && s.followUp < routeFollowUpMax:
		s.followUp *= 2
	default:
		s.followUp = 0
	}
	next := routeRecheck
	if s.followUp > 0 {
		next = s.followUp
	}
	if (len(out.Missing) > 0 || (!out.Verify && out.Waiting != "")) && next > time.Minute {
		next = time.Minute
	}
	s.applied, s.nextCheck, s.verified = string(key), now.Add(next), out.Verify
	if s.missingFor == nil {
		s.missingFor = map[string]time.Time{}
	}
	for h := range s.missingFor {
		if !slices.Contains(out.Missing, h) {
			delete(s.missingFor, h)
		}
	}
	var late []string
	for _, h := range out.Missing {
		if _, ok := s.missingFor[h]; !ok {
			s.missingFor[h] = now
		} else if now.Sub(s.missingFor[h]) > routeMissingWarn {
			late = append(late, h)
		}
	}
	if len(late) > 0 && now.Sub(s.warnedAt) > 5*time.Minute {
		s.warnedAt = now
		r.m.log.Warn("NATS routes: listed members are not connected; a joining host retries until every member lists it, and a stopped controller stays listed until it is removed", "members", late)
	}
	return nil
}

// lease takes or renews a short cluster-wide lease, so only one controller at a time restarts its NATS member.
func (r *Removals) lease(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	now := r.m.now().UTC()
	held := false
	err := r.m.db.InTx(ctx, func(tx *sql.Tx) error {
		var holder string
		var epoch int64
		var expires time.Time
		err := tx.QueryRowContext(ctx, r.m.db.Rebind(`SELECT holder, epoch, expires_at FROM cluster_leases WHERE name = ?`), name).Scan(&holder, &epoch, &expires)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, r.m.db.Rebind(`INSERT INTO cluster_leases (name, holder, epoch, expires_at) VALUES (?, ?, 1, ?)`), name, r.m.ID, now.Add(ttl))
			held = err == nil
			return err
		case err != nil:
			return err
		case holder != r.m.ID && now.Before(expires):
			return nil
		}
		res, err := tx.ExecContext(ctx, r.m.db.Rebind(`UPDATE cluster_leases SET holder = ?, epoch = ?, expires_at = ? WHERE name = ? AND holder = ? AND epoch = ?`), r.m.ID, epoch+1, now.Add(ttl), name, holder, epoch)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		held = n == 1
		return nil
	})
	return held, err
}
