package cluster

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"
)

func TestRemovalTakesTheHostsWorker(t *testing.T) {
	ctx := context.Background()
	d := removalDB(t)
	a, _ := member(t, d, "a"), member(t, d, "b")
	if _, err := join(ctx, d, "0.2.0", "c", Identity{Host: "c.example", CertPin: "sha256:c", LocalWorker: true}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	a.beat(ctx)
	r := NewRemovals(a)
	var calls []string
	r.LocalWorker = func(_ context.Context, revoke bool, _ string) error {
		calls = append(calls, map[bool]string{false: "drain", true: "revoke"}[revoke])
		return nil
	}
	if _, err := r.Request(ctx, admin, "b", "smaller cluster", ""); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("removing a controller without a worker touched the local worker: %v", calls)
	}
	if _, err := d.SQL.ExecContext(ctx, `DELETE FROM controller_removals`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Request(ctx, admin, "c", "converted host retired", ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"drain"}) {
		t.Fatalf("the host's worker was not drained when its removal was requested: %v", calls)
	}
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`UPDATE controllers SET heartbeat_at = ? WHERE hostname = 'c'`), time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, epoch := a.Leader()
	if err := r.Process(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"drain", "revoke"}) {
		t.Fatalf("the host's worker was not revoked with its controller: %v", calls)
	}
	var n int
	_ = d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM controllers WHERE hostname = 'c'`).Scan(&n)
	if n != 0 {
		t.Errorf("the removed controller's rows remain: %d", n)
	}
}
