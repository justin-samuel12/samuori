package workers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/pki"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

func TestWorkerRemoval(t *testing.T) {
	t.Run("SEC-WRK-09", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		d, err := db.Open(ctx, dbtest.SQLite(t, dir))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if _, err := d.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		ca, err := pki.Open(filepath.Join(dir, "pki"))
		if err != nil {
			t.Fatal(err)
		}
		srv := NewServer(NewStore(d, ca), slog.New(slog.NewTextHandler(io.Discard, nil)))
		now := time.Now().UTC()
		for _, q := range []string{
			`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`,
			`INSERT INTO worker_pools (id, org_id, name, location, isolation_profile, trust_domain, trusted_workloads_only, profile_approved, created_at) VALUES ('p1', 'o1', 'pool', 'here', 'rootless-dedicated-vm', 'default', 0, 1, ?)`,
		} {
			if _, err := d.SQL.ExecContext(ctx, d.Rebind(q), now); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO workers (id, org_id, pool_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, created_at) VALUES ('w1', 'o1', 'p1', 'build-1', 'enrolled', 'active', 's1', ?, '{}', '0.2.0', 2, ?)`), now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		admin := authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
		operator := authz.Principal{ID: "op", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Operator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}

		if _, err := srv.ApplyAction(ctx, operator, "w1", ActionRemove, "decommission", nil, ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("an operator removed a worker: %v", err)
		}
		w, err := srv.ApplyAction(ctx, admin, "w1", ActionRemove, "decommission", nil, "")
		if err != nil || w.OperationalState != Draining {
			t.Fatalf("remove = %s, %v; want the worker draining first", w.OperationalState, err)
		}
		if r, ok := srv.store.RemovalOf(ctx, "w1"); !ok || r.State != "draining" {
			t.Fatalf("removal = %+v, %v", r, ok)
		}

		ctxW := context.WithValue(ctx, workerKey{}, Worker{ID: "w1", OrgID: "o1"})
		if _, err := srv.ReportRemoval(ctxW, &workerv1.RemovalReport{Staged: true}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("a removal was confirmed before the wipe was requested: %v", err)
		}

		if err := srv.ProcessRemovals(ctx); err != nil {
			t.Fatal(err)
		}
		r, _ := srv.store.RemovalOf(ctx, "w1")
		if r.State != "removed" || r.Outcome != "not-confirmed" {
			t.Fatalf("an unconnected worker's removal = %+v; want removed, not confirmed wiped", r)
		}
		got, err := srv.store.Get(ctx, "o1", "w1")
		if err != nil || got.EnrollmentState != Revoked {
			t.Fatalf("a removed worker is still %s (%v)", got.EnrollmentState, err)
		}
		var n int
		_ = d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action = 'worker.remove'`).Scan(&n)
		if n != 1 {
			t.Errorf("removal audited %d times", n)
		}
	})
}
