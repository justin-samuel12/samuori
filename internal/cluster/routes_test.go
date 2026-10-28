package cluster

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/clusterjoin"
)

func TestNATSRoutePlan(t *testing.T) {
	t.Run("SEC-HA-10", func(t *testing.T) {
		ctx := context.Background()
		d := removalDB(t)
		a, b := member(t, d, "a"), member(t, d, "b")
		c := member(t, d, "c")
		now := time.Now().UTC()
		for _, q := range []struct {
			args []any
			sql  string
		}{
			{[]any{now, now}, `INSERT INTO controller_removals (hostname, host, cert_pins, org_id, reason, requested_by, requested_at, state, outcome, updated_at) VALUES ('c', 'c.example', '', 'o1', 'gone', 'admin', ?, 'removed', 'not-confirmed', ?)`},
			{[]any{c.ID}, `DELETE FROM controllers WHERE id = ?`},
			{[]any{now, now.Add(time.Hour)}, `INSERT INTO controller_join_tokens (id, token_hash, created_by, created_at, expires_at) VALUES ('t1', 'h1', 'admin', ?, ?)`},
			{[]any{now, now.Add(time.Hour)}, `INSERT INTO controller_join_requests (id, token_id, host, remote_addr, code, secret_hash, state, created_at, expires_at) VALUES ('q1', 't1', 'D.example', '', 'X', 'h', 'accepted', ?, ?)`},
			{[]any{now, now.Add(time.Hour)}, `INSERT INTO controller_join_tokens (id, token_hash, created_by, created_at, expires_at) VALUES ('t2', 'h2', 'admin', ?, ?)`},
			{[]any{now, now.Add(time.Hour)}, `INSERT INTO controller_join_requests (id, token_id, host, remote_addr, code, secret_hash, state, created_at, expires_at) VALUES ('q2', 't2', 'e.example', '', 'Y', 'h', 'rejected', ?, ?)`},
		} {
			if _, err := d.SQL.ExecContext(ctx, d.Rebind(q.sql), q.args...); err != nil {
				t.Fatal(err)
			}
		}
		r := NewRemovals(a)
		plan, err := r.RoutePlan(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(plan.Members, []string{"a.example", "b.example", "d.example"}) || !slices.Equal(plan.Removed, []string{"c.example"}) || !plan.Verify {
			t.Fatalf("plan = %+v", plan)
		}
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`UPDATE controllers SET host = '' WHERE id = ?`), b.ID); err != nil {
			t.Fatal(err)
		}
		if plan, _ := r.RoutePlan(ctx); plan.Verify {
			t.Fatal("verification planned while a running controller has not recorded its NATS name")
		}
	})
}

func TestNATSRouteSync(t *testing.T) {
	t.Run("SEC-HA-10", func(t *testing.T) {
		ctx := context.Background()
		d := removalDB(t)
		a, b := member(t, d, "a"), member(t, d, "b")
		ra, rb := NewRemovals(a), NewRemovals(b)
		var calls []clusterjoin.RoutePlan
		fail := true
		ra.SyncRoutes = func(p clusterjoin.RoutePlan) (clusterjoin.RouteOutcome, error) {
			calls = append(calls, p)
			if fail {
				return clusterjoin.RouteOutcome{}, errors.New("samuori-nats-routes did not answer")
			}
			if !p.Restart {
				return clusterjoin.RouteOutcome{Outcome: "stray", Stray: []string{"c.example"}}, nil
			}
			return clusterjoin.RouteOutcome{Outcome: "restarted", Verify: true}, nil
		}
		if err := ra.SyncNATSRoutes(ctx); err == nil {
			t.Fatal("a helper that did not answer counted as synced")
		}
		if err := ra.SyncNATSRoutes(ctx); err != nil || len(calls) != 1 {
			t.Fatalf("the retry did not wait: %d calls, %v", len(calls), err)
		}
		ra.routes.retryAt = time.Time{}
		fail = false
		if held, err := rb.lease(ctx, restartLease, restartLeaseTTL); err != nil || !held {
			t.Fatalf("b could not take the restart lease: %v %v", held, err)
		}
		if err := ra.SyncNATSRoutes(ctx); err == nil || len(calls) != 2 {
			t.Fatalf("a restarted its member while another controller held the restart lease: %d calls, %v", len(calls), err)
		}
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`UPDATE cluster_leases SET expires_at = ? WHERE name = ?`), time.Now().UTC().Add(-time.Second), restartLease); err != nil {
			t.Fatal(err)
		}
		ra.routes.retryAt = time.Time{}
		if err := ra.SyncNATSRoutes(ctx); err != nil || len(calls) != 4 || !calls[3].Restart {
			t.Fatalf("restart under the lease: %d calls, %v", len(calls), err)
		}
		if err := ra.SyncNATSRoutes(ctx); err != nil || len(calls) != 4 {
			t.Fatalf("an unchanged plan was sent again at once: %d calls, %v", len(calls), err)
		}
		a.removed.Store(true)
		ra.routes.applied = ""
		if err := ra.SyncNATSRoutes(ctx); err != nil || len(calls) != 4 {
			t.Fatal("a controller being removed rewrote its routes")
		}
	})
}

func TestNATSRouteFollowUp(t *testing.T) {
	t.Run("SEC-HA-10", func(t *testing.T) {
		ctx := context.Background()
		d := removalDB(t)
		a, b := member(t, d, "a"), member(t, d, "b")
		clk := time.Now()
		a.now = func() time.Time { return clk }
		ra := NewRemovals(a)
		calls := 0
		ra.SyncRoutes = func(clusterjoin.RoutePlan) (clusterjoin.RouteOutcome, error) {
			calls++
			return clusterjoin.RouteOutcome{Outcome: "succeeded", Verify: true}, nil
		}
		at := func(after time.Duration, want int) {
			t.Helper()
			clk = clk.Add(after)
			if _, err := d.SQL.ExecContext(ctx, d.Rebind(`UPDATE controllers SET heartbeat_at = ? WHERE stopped_at IS NULL`), clk.UTC()); err != nil {
				t.Fatal(err)
			}
			if err := ra.SyncNATSRoutes(ctx); err != nil || calls != want {
				t.Fatalf("%v later: %d helper calls, want %d (%v)", after, calls, want, err)
			}
		}
		at(0, 1)
		n := 1
		for gap := 5 * time.Second; gap <= 160*time.Second; gap *= 2 {
			at(gap-time.Second, n)
			n++
			at(time.Second, n)
		}
		at(9*time.Minute, n)
		at(time.Minute, n+1)
		at(4*time.Second, n+1)
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`UPDATE controllers SET stopped_at = ? WHERE id = ?`), clk.UTC(), b.ID); err != nil {
			t.Fatal(err)
		}
		at(0, n+2)
		at(5*time.Second, n+3)
	})
}
