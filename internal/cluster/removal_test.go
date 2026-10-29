package cluster

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

func removalDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.SQLite(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return d
}

func member(t *testing.T, d *db.DB, hostname string) *Member {
	t.Helper()
	m, err := join(context.Background(), d, "0.2.0", hostname, Identity{Host: hostname + ".example", CertPin: "sha256:" + hostname}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func refused(err error) bool {
	var r *Refused
	return errors.As(err, &r)
}

var (
	admin    = authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	operator = authz.Principal{ID: "op", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Operator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
)

func TestControllerRemovalRefusals(t *testing.T) {
	t.Run("SEC-HA-05", func(t *testing.T) {
		ctx := context.Background()
		d := removalDB(t)
		a := member(t, d, "a")
		r := NewRemovals(a)
		r.NATSOnControllers = true

		if _, err := r.Request(ctx, admin, "a", "decommission", ""); !refused(err) {
			t.Fatalf("the last controller was removable: %v", err)
		}
		b := member(t, d, "b")
		if _, err := r.Request(ctx, admin, "b", "decommission", ""); !refused(err) {
			t.Fatalf("removing one of two NATS members was accepted: %v", err)
		}
		c := member(t, d, "c")
		if _, err := r.Request(ctx, operator, "c", "decommission", ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("an operator removed a controller: %v", err)
		}
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`UPDATE controllers SET heartbeat_at = ? WHERE id = ?`), time.Now().UTC().Add(-time.Minute), b.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Request(ctx, admin, "c", "decommission", ""); !refused(err) {
			t.Fatalf("a controller was removed while another was not running: %v", err)
		}
		b.beat(ctx)
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO rollouts (id, version, state, reason, created_by, created_at) VALUES ('r1', '0.3.0', 'running', 'update', 'admin', ?)`), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Request(ctx, admin, "c", "decommission", ""); !refused(err) {
			t.Fatalf("a controller was removed during a rollout: %v", err)
		}
		if _, err := d.SQL.ExecContext(ctx, `UPDATE rollouts SET state = 'succeeded'`); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Request(ctx, admin, "c", "decommission", ""); err != nil {
			t.Fatalf("removing one of three healthy controllers: %v", err)
		}
		if _, err := r.Request(ctx, admin, "c", "decommission", ""); !refused(err) {
			t.Fatalf("a second removal of the same controller was accepted: %v", err)
		}
		_ = c
	})
}

func TestControllerRemovalHandover(t *testing.T) {
	t.Run("SEC-HA-06", func(t *testing.T) {
		ctx := context.Background()
		d := removalDB(t)
		a, b, c := member(t, d, "a"), member(t, d, "b"), member(t, d, "c")
		a.beat(ctx)
		b.beat(ctx)
		c.beat(ctx)
		leader, epoch := a.Leader()
		if !leader {
			t.Fatal("the first controller did not take the lease")
		}
		ra, rb := NewRemovals(a), NewRemovals(b)
		rb.NATSOnControllers = true
		wiped := 0
		ra.Wipe = func() error { wiped++; return nil }
		var forgot []string
		rb.Forget = func(h []string) error { forgot = append(forgot, h...); return nil }

		if _, err := rb.Request(ctx, admin, "a", "decommission", "req-1"); err != nil {
			t.Fatal(err)
		}
		a.beat(ctx)
		if ok, _ := a.Leader(); ok || !a.Removed() {
			t.Fatal("the controller being removed kept the leadership")
		}
		if err := ra.StageOwnWipe(ctx); err != nil || wiped != 0 {
			t.Fatalf("the host was wiped before another controller led: %d, %v", wiped, err)
		}
		b.beat(ctx)
		ok, newEpoch := b.Leader()
		if !ok || newEpoch <= epoch {
			t.Fatalf("leadership was not handed over: leader %v, epoch %d after %d", ok, newEpoch, epoch)
		}
		err := d.InTx(ctx, func(tx *sql.Tx) error { return a.Fenced(ctx, tx, epoch) })
		if !errors.Is(err, ErrNotLeader) {
			t.Fatalf("the removed controller could still write as leader: %v", err)
		}
		if err := a.Draining(ctx); err == nil {
			t.Error("a controller being removed still reports itself healthy to load balancers")
		}
		if err := ra.StageOwnWipe(ctx); err != nil || wiped != 0 {
			t.Fatalf("the host was wiped before it drained: %d, %v", wiped, err)
		}
		a.removedAt.Store(time.Now().Add(-RemovalDrain).UnixNano())
		if err := ra.StageOwnWipe(ctx); err != nil || wiped != 1 {
			t.Fatalf("wipe staged %d times, %v", wiped, err)
		}
		if err := rb.Process(ctx, newEpoch); err != nil {
			t.Fatal(err)
		}
		list, _ := rb.List(ctx)
		if len(list) != 1 || list[0].State != "removed" || list[0].Outcome != "wiped" {
			t.Fatalf("removal = %+v", list)
		}
		var rows int
		_ = d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM controllers WHERE hostname = 'a'`).Scan(&rows)
		if rows != 0 {
			t.Errorf("%d membership rows of the removed controller remain", rows)
		}
		var audited int
		_ = d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action IN ('controller.remove', 'controller.revoke') AND resource_id = 'a' AND org_id = 'o1'`).Scan(&audited)
		if audited != 2 {
			t.Errorf("removal audited %d times; want request and revocation", audited)
		}
		if err := rb.ForgetRemoved(ctx); err != nil || len(forgot) != 1 || forgot[0] != "a.example" {
			t.Fatalf("NATS forget = %v, %v", forgot, err)
		}
		_ = rb.ForgetRemoved(ctx)
		if len(forgot) != 1 {
			t.Errorf("the same host was forgotten again in one process: %v", forgot)
		}

		if _, err := join(ctx, d, "0.2.0", "a", Identity{Host: "a.example", CertPin: "sha256:a"}, slog.New(slog.DiscardHandler)); !errors.Is(err, ErrRemoved) {
			t.Fatalf("the removed controller's identity was accepted again: %v", err)
		}
		if _, err := join(ctx, d, "0.2.0", "a", Identity{Host: "a.example"}, slog.New(slog.DiscardHandler)); !errors.Is(err, ErrRemoved) {
			t.Fatalf("the removed host joined without a certificate: %v", err)
		}
		if _, err := join(ctx, d, "0.2.0", "a", Identity{Host: "a.example", CertPin: "sha256:rejoined"}, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatalf("a new identity from an accepted join was refused: %v", err)
		}
	})
}

func TestControllerRemovalUnreachable(t *testing.T) {
	t.Run("SEC-HA-07", func(t *testing.T) {
		ctx := context.Background()
		d := removalDB(t)
		a, b, c := member(t, d, "a"), member(t, d, "b"), member(t, d, "c")
		a.beat(ctx)
		r := NewRemovals(a)
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`UPDATE controllers SET heartbeat_at = ? WHERE id = ?`), time.Now().UTC().Add(-time.Minute), c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Request(ctx, admin, "c", "host lost", ""); err != nil {
			t.Fatalf("an unreachable controller could not be removed: %v", err)
		}
		_, epoch := a.Leader()
		if err := r.Process(ctx, epoch); err != nil {
			t.Fatal(err)
		}
		list, _ := r.List(ctx)
		if len(list) != 1 || list[0].State != "removed" || list[0].Outcome != "not-confirmed" {
			t.Fatalf("an unreachable controller's removal = %+v; want removed, not confirmed wiped", list)
		}
		_ = b
	})
}

// A removed host whose join was accepted again is not forgotten by members that restart before it starts, and does
// not hand over leadership as if still being removed.
func TestControllerRemovalRejoin(t *testing.T) {
	t.Run("SEC-HA-06", func(t *testing.T) {
		ctx := context.Background()
		d := removalDB(t)
		b := member(t, d, "b")
		b.beat(ctx)
		now := time.Now().UTC()
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO controller_removals (hostname, host, cert_pins, org_id, reason, requested_by, requested_at, state, outcome, updated_at) VALUES ('a', 'a.example', 'sha256:a', 'o1', 'gone', 'admin', ?, 'rejoined', 'wiped', ?)`), now, now); err != nil {
			t.Fatal(err)
		}
		rb := NewRemovals(b)
		var forgot []string
		rb.Forget = func(h []string) error { forgot = append(forgot, h...); return nil }
		if err := rb.ForgetRemoved(ctx); err != nil || len(forgot) != 0 {
			t.Fatalf("a rejoined host was forgotten: %v, %v", forgot, err)
		}
		if list, _ := rb.List(ctx); len(list) != 0 {
			t.Errorf("a rejoined host is listed as removed: %+v", list)
		}
		if _, err := join(ctx, d, "0.2.0", "a", Identity{Host: "a.example", CertPin: "sha256:a"}, slog.New(slog.DiscardHandler)); !errors.Is(err, ErrRemoved) {
			t.Fatalf("the old identity of a rejoined host was accepted: %v", err)
		}
		a, err := join(ctx, d, "0.2.0", "a", Identity{Host: "a.example", CertPin: "sha256:new"}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		if a.removing(ctx) || a.Removed() {
			t.Error("the rejoined controller treats itself as being removed")
		}
		if _, err := rb.Request(ctx, admin, "a", "again", "req-2"); refused(err) && err.Error() == "this controller is already being removed" {
			t.Errorf("a rejoined controller cannot be removed again: %v", err)
		}
	})
}

// The stream's own replicas decide: a host NATS has not taken back yet does not count toward the majority.
func TestControllerRemovalStreamMajority(t *testing.T) {
	t.Run("SEC-HA-05", func(t *testing.T) {
		ctx := context.Background()
		d := removalDB(t)
		a, b, c := member(t, d, "a"), member(t, d, "b"), member(t, d, "c")
		a.beat(ctx)
		b.beat(ctx)
		c.beat(ctx)
		r := NewRemovals(b)
		r.NATSOnControllers = true
		peers := []StreamPeer{{Name: "a.example", Current: true}, {Name: "b.example", Current: true}}
		r.StreamPeers = func(context.Context) ([]StreamPeer, int, error) { return peers, 3, nil }
		if _, err := r.Request(ctx, admin, "a", "decommission", ""); !refused(err) {
			t.Fatalf("removal left the stream 1 of 3 replicas while a rejoined member was not back: %v", err)
		}
		peers = append(peers, StreamPeer{Name: "c.example", Current: false})
		_, err := r.Request(ctx, admin, "a", "decommission", "")
		if !refused(err) || !strings.Contains(err.Error(), "c.example") {
			t.Fatalf("removal did not wait for the member that is catching up: %v", err)
		}
		r2 := NewRemovals(c)
		r2.NATSOnControllers = true
		r2.StreamPeers = func(context.Context) ([]StreamPeer, int, error) { return nil, 3, errors.New("timeout") }
		if _, err := r2.Request(ctx, admin, "b", "decommission", ""); !refused(err) || !strings.Contains(err.Error(), "could not be read") {
			t.Fatalf("removal went ahead without reading the stream: %v", err)
		}
		peers[2].Current = true
		if _, err := r.Request(ctx, admin, "a", "decommission", ""); err != nil {
			t.Fatalf("removal refused with two of three replicas current afterwards: %v", err)
		}
	})
}

// A first controller with no other member serves as degraded, also after a join whose host never started; once
// another controller has started, a lost NATS majority counts again.
func TestNeverJoined(t *testing.T) {
	ctx := context.Background()
	d := removalDB(t)
	a := member(t, d, "a")
	a.beat(ctx)
	if !a.NeverJoined(ctx) {
		t.Fatal("a lone first controller is not reported as never joined")
	}
	b := member(t, d, "b")
	b.beat(ctx)
	if a.NeverJoined(ctx) {
		t.Error("a controller with another member is reported as never joined")
	}
	d2 := removalDB(t)
	c := member(t, d2, "c")
	c.beat(ctx)
	now := time.Now().UTC()
	if _, err := d2.SQL.ExecContext(ctx, d2.Rebind(`INSERT INTO controller_join_tokens (id, token_hash, created_by, created_at, expires_at) VALUES ('t1', 'h', 'admin', ?, ?)`), now, now.Add(time.Hour)); err != nil {
		t.Fatalf("join token fixture: %v", err)
	}
	if _, err := d2.SQL.ExecContext(ctx, d2.Rebind(`INSERT INTO controller_join_requests (id, token_id, host, remote_addr, code, secret_hash, state, created_at, expires_at) VALUES ('r1', 't1', 'd.example', '192.0.2.1', 'AAAA-BBBB', 'x', 'completed', ?, ?)`), now, now.Add(time.Hour)); err != nil {
		t.Fatalf("join request fixture: %v", err)
	}
	if !c.NeverJoined(ctx) {
		t.Error("an accepted join whose host never started made the first controller unavailable")
	}
}

// A restarted host is listed once, as its new process, not also as the stopped one.
func TestControllersOnePerHost(t *testing.T) {
	ctx := context.Background()
	d := removalDB(t)
	old := member(t, d, "a")
	old.beat(ctx)
	old.leave(ctx)
	time.Sleep(10 * time.Millisecond)
	restarted := member(t, d, "a")
	restarted.beat(ctx)
	other := member(t, d, "b")
	other.beat(ctx)
	list, err := other.Controllers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d controllers for two hosts: %+v", len(list), list)
	}
	for _, c := range list {
		if c.Hostname == "a" && (c.ID != restarted.ID || !c.Live) {
			t.Errorf("host a is listed as %+v; want its restarted, live process %s", c, restarted.ID)
		}
	}
}
