package builds

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/db"
)

// TestBuildVersions: successful builds are numbered per project and UTC day, in the order they succeed, and a
// version is never handed out twice (ADR 0054).
func TestBuildVersions(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		if _, err := d.SQL.ExecContext(ctx, `INSERT INTO build_runs (id, org_id, project_id, team_id, spec_id, spec_revision, spec_digest, snapshot_id, snapshot_digest, state, platforms, isolation_profile, secret_bearing, untrusted, priority, generation, attempts_used, max_attempts, queue_deadline, requested_by, idempotency_key, created_at, updated_at)
			VALUES (?, 'o1', 'p1', 't1', 's1', 1, 'sd', 'sn', 'snd', 'succeeded', '[]', 'rootless-host', 0, 1, 0, 1, 1, 3, ?, 'u', ?, ?, ?)`, id, now, id, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.SQL.ExecContext(ctx, `UPDATE build_runs SET project_id = 'p2' WHERE id = 'r3'`); err != nil {
		t.Fatal(err)
	}
	s := &Service{db: d, now: time.Now}
	day := time.Date(2026, 10, 27, 23, 59, 0, 0, time.UTC)
	cases := []struct {
		id, project string
		at          time.Time
		want        string
	}{
		{"r1", "p1", day, "2026-10-27.1"},
		{"r2", "p1", day, "2026-10-27.2"},
		{"r3", "p2", day, "2026-10-27.1"},
		{"r4", "p1", day.Add(2 * time.Minute), "2026-10-28.1"},
		{"r5", "p1", day.In(time.FixedZone("CDT", -5*3600)), "2026-10-27.3"},
	}
	for _, c := range cases {
		tx, err := d.SQL.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		v, err := s.assignVersion(ctx, tx, c.id, c.project, c.at)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if v != c.want {
			t.Errorf("%s = %s, want %s", c.id, v, c.want)
		}
	}
	if _, err := d.SQL.ExecContext(ctx, `UPDATE build_runs SET version = '2026-10-27.1' WHERE id = 'r2'`); err == nil {
		t.Error("two builds of one project got the same version")
	}
}
