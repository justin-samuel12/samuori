package notify

import (
	"context"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

// TestBuildFactsFormat: durations and sizes read naturally, and a requester's name cannot inject chat markup.
func TestBuildFactsFormat(t *testing.T) {
	for in, want := range map[time.Duration]string{42 * time.Second: "42 s", 3*time.Minute + 12*time.Second: "3 min 12 s", 2*time.Hour + 5*time.Minute: "2 h 5 min"} {
		if got := duration(in); got != want {
			t.Errorf("duration(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[int64]string{512: "512 B", 5 << 20: "5.0 MiB", 412 << 20: "412 MiB", 3 << 30: "3.0 GiB"} {
		if got := byteSize(in); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", in, got, want)
		}
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.SQLite(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, q := range []string{
		`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'Org', ?)`,
		`INSERT INTO users (id, org_id, username, display_name, disabled, created_at) VALUES ('u1', 'o1', 'dana', '<!channel> *Dana*', 0, ?)`,
		`INSERT INTO users (id, org_id, username, display_name, disabled, created_at) VALUES ('u2', 'o1', 'sam', '', 0, ?)`,
	} {
		if _, err := d.SQL.Exec(q, now); err != nil {
			t.Fatal(err)
		}
	}
	start := now.Add(-(3*time.Minute + 12*time.Second))
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`PRAGMA foreign_keys = OFF`, nil},
		{`INSERT INTO projects (id, org_id, team_id, name, publication_mode, created_at, updated_at) VALUES ('p1', 'o1', 't1', 'test', 'manual', ?, ?)`, []any{now, now}},
		{`INSERT INTO build_specs (id, org_id, project_id, name, latest_revision, created_by, created_at, updated_at) VALUES ('s1', 'o1', 'p1', 'np-image', 1, 'u2', ?, ?)`, []any{now, now}},
		{`INSERT INTO build_runs (id, org_id, project_id, team_id, spec_id, spec_revision, spec_digest, snapshot_id, snapshot_digest, state, platforms, isolation_profile, secret_bearing, untrusted, priority, generation, attempts_used, max_attempts, queue_deadline, requested_by, idempotency_key, created_at, updated_at)
			VALUES ('r1', 'o1', 'p1', 't1', 's1', 1, 'sd', 'sn', 'snd', 'failed', '[]', 'rootless-host', 0, 1, 0, 1, 1, 3, ?, 'u2', 'k1', ?, ?)`, []any{now, start, now}},
		{`INSERT INTO build_runs (id, org_id, project_id, team_id, spec_id, spec_revision, spec_digest, snapshot_id, snapshot_digest, state, platforms, isolation_profile, secret_bearing, untrusted, priority, generation, attempts_used, max_attempts, queue_deadline, requested_by, idempotency_key, created_at, updated_at)
			VALUES ('r2', 'o1', 'p1', 't1', 's1', 1, 'sd', 'sn', 'snd', 'succeeded', '[]', 'rootless-host', 0, 1, 0, 1, 1, 3, ?, 'u2', 'k2', ?, ?)`, []any{now, start, now}},
		{`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES ('ba1', 'o1', 'r2', 'a2', 1, 'image', 'sha256:x', ?, ?)`, []any{412 << 20, now}},
	} {
		if _, err := d.SQL.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.SQL.ExecContext(ctx, `UPDATE build_runs SET version = '2026-10-27.2' WHERE id = 'r2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, `UPDATE build_runs SET failure_class = 'application' WHERE id = 'r1'`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.SQL.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	disp := NewDispatcher(d, "")
	if got := disp.person(ctx, tx, "u1"); got != "!channel Dana" {
		t.Errorf("person(u1) = %q", got)
	}
	if got := disp.person(ctx, tx, "u2"); got != "sam" {
		t.Errorf("person(u2) = %q", got)
	}
	if got := disp.person(ctx, tx, "pipeline-x"); got != "" {
		t.Errorf("person(unknown) = %q", got)
	}
	for id, want := range map[string]string{
		"r1": "Project: test. App: np-image. Failure class: application. Requested by sam. Took 3 min 12 s.",
		"r2": "Project: test. App: np-image. Version: 2026-10-27.2. Requested by sam. Took 3 min 12 s. Image 412 MiB.",
	} {
		outcome := map[string]string{"r1": "failed", "r2": "succeeded"}[id]
		e, err := disp.classify(ctx, tx, audit.Record{ID: "ev-" + id, OrgID: "o1", At: now, Action: "build.finished", ResourceType: "build-run", ResourceID: id, Outcome: outcome})
		if err != nil || e == nil {
			t.Fatalf("classify %s = %v, %v", id, e, err)
		}
		if e.msg.Text != want || e.msg.App != "np-image" {
			t.Errorf("%s text = %q (app %q), want %q", id, e.msg.Text, e.msg.App, want)
		}
	}
}
