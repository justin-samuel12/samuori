package builds

import (
	"bytes"
	"context"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

// TestLiveLog: only the worker holding the current lease appends live output, in order and within the cap; a build
// that uses secrets gets none, and reconciliation removes the output once the attempt ends.
func TestLiveLog(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.SQLite(t, t.TempDir()))
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
	run := func(id, attempt string, secret int) []any {
		return []any{id, digest, secret, attempt, now.Add(time.Hour), now.Add(time.Hour), id, now, now}
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`, []any{now}},
		{`INSERT INTO build_plans (digest, org_id, plan, created_at) VALUES (?, 'o1', ?, ?)`, []any{digest, string(raw), now}},
		{`INSERT INTO build_runs (id, org_id, project_id, team_id, spec_id, spec_revision, spec_digest, snapshot_id, snapshot_digest, plan_digest, state, platforms, isolation_profile, secret_bearing, untrusted, priority, generation, current_attempt_id, lease_worker_id, lease_deadline, attempts_used, max_attempts, queue_deadline, requested_by, idempotency_key, created_at, updated_at)
			VALUES (?, 'o1', 'p1', 't1', 's1', 1, 'sd', 'sn', 'snd', ?, 'building', '["linux/arm64"]', 'rootless-host', ?, 1, 0, 1, ?, 'w1', ?, 1, 3, ?, 'u1', ?, ?, ?)`, run("r1", "a1", 0)},
		{`INSERT INTO build_runs (id, org_id, project_id, team_id, spec_id, spec_revision, spec_digest, snapshot_id, snapshot_digest, plan_digest, state, platforms, isolation_profile, secret_bearing, untrusted, priority, generation, current_attempt_id, lease_worker_id, lease_deadline, attempts_used, max_attempts, queue_deadline, requested_by, idempotency_key, created_at, updated_at)
			VALUES (?, 'o1', 'p1', 't1', 's1', 1, 'sd', 'sn', 'snd', ?, 'building', '["linux/arm64"]', 'rootless-host', ?, 1, 0, 1, ?, 'w1', ?, 1, 3, ?, 'u1', ?, ?, ?)`, run("r2", "a2", 1)},
		{`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at) VALUES ('a1', 'r1', 1, 1, 'w1', 'pool', 'building', ?, 'pending', ?)`, []any{now.Add(time.Hour), now}},
		{`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at) VALUES ('a2', 'r2', 1, 1, 'w1', 'pool', 'building', ?, 'pending', ?)`, []any{now.Add(time.Hour), now}},
	} {
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(q.sql), q.args...); err != nil {
			t.Fatal(err)
		}
	}
	lease := &workerv1.LeaseRef{AttemptId: "a1", FencingGeneration: 1}
	appendLog := func(worker string, l *workerv1.LeaseRef, off uint64, data string) (bool, error) {
		return sc.AppendLog(ctx, worker, &workerv1.LogChunk{Lease: l, Offset: off, Data: []byte(data)})
	}
	if stop, err := appendLog("w1", lease, 0, "#1 FROM node\n"); stop || err != nil {
		t.Fatalf("first chunk = %v %v", stop, err)
	}
	if stop, err := appendLog("w1", lease, 0, "#1 FROM node\n"); stop || err != nil {
		t.Errorf("repeated chunk = %v %v", stop, err)
	}
	if _, err := appendLog("w1", lease, 99, "gap"); err == nil {
		t.Error("a chunk past the stored output was accepted")
	}
	if stop, err := appendLog("w1", lease, 13, "#2 RUN npm ci\n"); stop || err != nil {
		t.Fatalf("second chunk = %v %v", stop, err)
	}
	for name, c := range map[string]struct {
		worker string
		lease  *workerv1.LeaseRef
	}{
		"other worker":     {"w2", lease},
		"stale generation": {"w1", &workerv1.LeaseRef{AttemptId: "a1", FencingGeneration: 0}},
		"secret build":     {"w1", &workerv1.LeaseRef{AttemptId: "a2", FencingGeneration: 1}},
	} {
		if stop, err := appendLog(c.worker, c.lease, 0, "x"); !stop || err != nil {
			t.Errorf("%s = %v %v, want stop", name, stop, err)
		}
	}
	if _, err := appendLog("w1", lease, 27, string(bytes.Repeat([]byte("y"), MaxLiveChunk+1))); err == nil {
		t.Error("an oversized chunk was accepted")
	}
	var got []byte
	rows, err := d.SQL.QueryContext(ctx, `SELECT data FROM build_live_logs ORDER BY attempt_id, offset_bytes`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c []byte
		_ = rows.Scan(&c)
		got = append(got, c...)
	}
	rows.Close()
	if string(got) != "#1 FROM node\n#2 RUN npm ci\n" {
		t.Errorf("stored = %q", got)
	}
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`UPDATE build_attempts SET ended_at = ? WHERE id = 'a1'`), now); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = d.SQL.QueryRow(`SELECT COUNT(*) FROM build_live_logs`).Scan(&left)
	if left != 0 {
		t.Errorf("%d live chunks kept after the attempt ended", left)
	}
}
