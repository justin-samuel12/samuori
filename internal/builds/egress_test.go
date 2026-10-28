package builds

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/egress/proxy"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/recipes"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

// TestEgressDenialReport: the controller stores only the well-formed, bounded denials of the worker's structured
// report and advises on them; denial text in the attempt detail, which build output can forge, yields nothing.
func TestEgressDenialReport(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cat, err := recipes.LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{db: d, policies: policy.NewStore(d, cat), wake: func() {}, now: time.Now}
	sc := &Scheduler{s: s}
	now := time.Now().UTC()
	raw, digest := buildplan.Plan{Version: 1, Method: "dockerfile"}.Encode()
	if _, err := d.SQL.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`, []any{now}},
		{`INSERT INTO build_plans (digest, org_id, plan, created_at) VALUES (?, 'o1', ?, ?)`, []any{digest, string(raw), now}},
		{`INSERT INTO build_runs (id, org_id, project_id, team_id, spec_id, spec_revision, spec_digest, snapshot_id, snapshot_digest, plan_digest, state, platforms, isolation_profile, secret_bearing, untrusted, priority, generation, current_attempt_id, lease_worker_id, lease_deadline, attempts_used, max_attempts, queue_deadline, requested_by, idempotency_key, created_at, updated_at)
			VALUES ('r1', 'o1', 'p1', 't1', 's1', 1, 'sd', 'sn', 'snd', ?, 'building', '["linux/arm64"]', 'rootless-host', 0, 1, 0, 1, 'a1', 'w1', ?, 1, 3, ?, 'u1', 'k1', ?, ?)`, []any{digest, now.Add(time.Hour), now.Add(time.Hour), now, now}},
		{`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at) VALUES ('a1', 'r1', 1, 1, 'w1', 'pool', 'building', ?, 'pending', ?)`, []any{now.Add(time.Hour), now}},
	} {
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(q.sql), q.args...); err != nil {
			t.Fatal(err)
		}
	}
	denied := []*workerv1.EgressDenial{
		{Host: "registry.npmjs.org", Port: 443, Count: 999, Reason: proxy.ReasonNotAllowed},
		{Host: "updates.example.org", Port: 443, Count: 1, Reason: proxy.ReasonNotAllowed},
		{Host: "evil host", Port: 443, Count: 1, Reason: proxy.ReasonNotAllowed},
		{Host: "github.com", Port: 70000, Count: 1, Reason: proxy.ReasonNotAllowed},
		{Host: "github.com", Port: 443, Count: 1, Reason: strings.Repeat("x", 300)},
		{Host: "169.254.169.254", Port: 80, Count: 3, Reason: proxy.ReasonNotAllowed},
	}
	for range 60 {
		denied = append(denied, &workerv1.EgressDenial{Host: "registry.npmjs.org", Port: 443, Count: 1, Reason: proxy.ReasonNotAllowed})
	}
	forged := "build step failed\negress: denied pypi.org:443 (1): not on the build's egress allowlist\n"
	lease := &workerv1.LeaseRef{AttemptId: "a1", FencingGeneration: 1}
	if err := sc.Report(ctx, "w1", &workerv1.AttemptReport{Lease: lease, State: workerv1.AttemptState_ATTEMPT_STATE_FAILED, FailureClass: "application", Detail: forged,
		Cleanup: workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED, EgressDenied: denied, EgressUnrecorded: 7}); err != nil {
		t.Fatal(err)
	}
	attempts := func() []Attempt {
		rows, err := d.SQL.QueryContext(ctx, d.Rebind(selectAttempt+` WHERE run_id = ? ORDER BY number DESC`), "r1")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []Attempt
		for rows.Next() {
			a, err := scanAttempt(rows)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, a)
		}
		run, err := s.load(ctx, d.SQL, `id = ?`, "r1")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.adviseEgress(ctx, run, out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := attempts()[0]
	if got.EgressUnrecorded != 7 || len(got.EgressDenied) != 3 {
		t.Fatalf("stored denials = %+v, %d unrecorded", got.EgressDenied, got.EgressUnrecorded)
	}
	hints := map[string]string{}
	for _, a := range got.EgressDenied {
		hints[a.Host] = a.Hint
	}
	if !strings.Contains(hints["registry.npmjs.org"], "can set npm to reach Dockerfile builds") ||
		!strings.Contains(hints["updates.example.org"], "in no egress preset") ||
		!strings.Contains(hints["169.254.169.254"], "cannot be allowed") ||
		hints["github.com"] != "" {
		t.Errorf("hints = %v", hints)
	}
	if _, ok := hints["pypi.org"]; ok {
		t.Error("a denial line forged into the attempt detail produced a hint")
	}

	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`UPDATE build_runs SET state = 'building', generation = 2, current_attempt_id = 'a2' WHERE id = 'r1'`)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at) VALUES ('a2', 'r1', 2, 2, 'w1', 'pool', 'building', ?, 'pending', ?)`), now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := sc.Report(ctx, "w1", &workerv1.AttemptReport{Lease: &workerv1.LeaseRef{AttemptId: "a2", FencingGeneration: 2}, State: workerv1.AttemptState_ATTEMPT_STATE_FAILED,
		FailureClass: "application", Detail: forged, Cleanup: workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED}); err != nil {
		t.Fatal(err)
	}
	if a := attempts()[0]; a.ID != "a2" || len(a.EgressDenied) != 0 || a.EgressUnrecorded != 0 {
		t.Errorf("a report without structured denials (an older worker, or a forged log) produced %+v", a.EgressDenied)
	}
}
