package imagesources

import (
	"errors"
	"testing"
	"time"
)

// TestUpdateAndDelete: edits are administrator-only and validated like create; a delete is refused while an
// unfinished build of the source's team plans to pull from its upstream, and allowed once it finishes.
func TestUpdateAndDelete(t *testing.T) {
	f := setup(t)
	src, err := f.st.Create(f.ctx, f.admin, Source{Name: "corp", Upstream: "registry.corp.example", Mirror: "mirror.example:5000", Patterns: []string{"payments/*"}, TeamIDs: []string{f.teamA}}, "")
	if err != nil {
		t.Fatal(err)
	}
	change := Source{Name: "corp-base", Patterns: []string{"payments/base/*"}, TeamIDs: []string{f.teamA}}
	if _, err := f.st.Update(f.ctx, f.dev, src.ID, change, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer edited a source: %v", err)
	}
	var inv *Invalid
	for name, bad := range map[string]Source{
		"bare *":     {Name: "corp", Patterns: []string{"*"}, TeamIDs: []string{f.teamA}},
		"no teams":   {Name: "corp", Patterns: []string{"payments/*"}},
		"two teams":  {Name: "corp", Patterns: []string{"payments/*"}, TeamIDs: []string{f.teamA, f.teamB}},
		"bad name":   {Name: "Corp", Patterns: []string{"payments/*"}, TeamIDs: []string{f.teamA}},
		"no profile": {Name: "corp", Patterns: []string{"payments/*"}, TeamIDs: []string{f.teamA}, PullProfileID: "missing"},
	} {
		if _, err := f.st.Update(f.ctx, f.admin, src.ID, bad, ""); !errors.As(err, &inv) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got, err := f.st.Update(f.ctx, f.admin, src.ID, change, "")
	if err != nil || got.Name != "corp-base" || got.Upstream != "registry.corp.example" || got.Mirror != "mirror.example:5000" || got.Allows(f.teamA, "payments/other") || !got.Allows(f.teamA, "payments/base/x") {
		t.Fatalf("update = %+v %v", got, err)
	}

	if err := f.st.Delete(f.ctx, f.dev, src.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer deleted a source: %v", err)
	}
	conn, err := f.d.SQL.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`PRAGMA foreign_keys = OFF`, nil},
		{`INSERT INTO build_plans (digest, org_id, plan, created_at) VALUES ('sha256:p', ?, '{"dockerfile":{"baseImagePins":[{"ref":"registry.corp.example/payments/base/node:22"}]}}', ?)`, []any{f.org, now}},
		{`INSERT INTO build_runs (id, org_id, project_id, team_id, spec_id, spec_revision, spec_digest, snapshot_id, snapshot_digest, plan_digest, state, platforms, isolation_profile, secret_bearing, untrusted, priority, generation, attempts_used, max_attempts, queue_deadline, requested_by, idempotency_key, created_at, updated_at)
			VALUES ('run-1', ?, ?, ?, 's', 1, 'd', 'n', 'd', 'sha256:p', 'building', '[]', 'p', 0, 1, 0, 1, 0, 1, ?, 'u', 'k', ?, ?)`, []any{f.org, f.projectA, f.teamA, now, now, now}},
		{`PRAGMA foreign_keys = ON`, nil},
	} {
		if _, err := conn.ExecContext(f.ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close()
	var inUse *InUse
	if err := f.st.Delete(f.ctx, f.admin, src.ID, ""); !errors.As(err, &inUse) || len(inUse.Uses) != 1 {
		t.Fatalf("delete with an unfinished build = %v", err)
	}
	if _, err := f.d.SQL.Exec(`UPDATE build_runs SET state = 'succeeded' WHERE id = 'run-1'`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Delete(f.ctx, f.admin, src.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Get(f.ctx, f.org, src.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted source still there: %v", err)
	}
	var n int
	if err := f.d.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE resource_id = ? AND action IN ('image-source.update', 'image-source.delete')`, src.ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("audit = %d %v", n, err)
	}
}
