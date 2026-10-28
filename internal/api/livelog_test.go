package api

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// TestLiveBuildLog: while an attempt builds, the logs endpoint returns its live output, marked live; a build that
// uses secrets shows a notice instead of output.
func TestLiveBuildLog(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	files := map[string]string{"Dockerfile": "FROM alpine:3.20\nRUN echo hi\n"}
	snap := l.snapshotOf(t, alice, projA, files)
	spec := `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"live"},
"spec":{"application":{"type":"worker"},"build":{"method":"dockerfile","dockerfile":{"context":".","path":"Dockerfile"}}}}`
	_, _, created := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(spec), nil)
	_, _, run := l.json(t, alice, "POST", "/builds", fmt.Sprintf(`{"specId":%q,"specRevision":1,"snapshotId":%q}`, created["specId"], snap), map[string]string{"Idempotency-Key": "live-log-build-0001"})
	runID, ok := run["id"].(string)
	if !ok {
		t.Fatalf("build = %v", run)
	}
	now := time.Now().UTC()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at) VALUES ('01JLIVEATTEMPT000000000001', ?, 1, 1, '01JLIVEW0RKER0000000000001', '01JLIVEP00L000000000000001', 'building', ?, 'pending', ?)`, []any{runID, now.Add(time.Hour), now}},
		{`UPDATE build_runs SET state = 'building', current_attempt_id = '01JLIVEATTEMPT000000000001', generation = 1 WHERE id = ?`, []any{runID}},
		{`INSERT INTO build_live_logs (attempt_id, generation, offset_bytes, data, created_at) VALUES ('01JLIVEATTEMPT000000000001', 1, 0, ?, ?)`, []any{[]byte("#1 FROM alpine\n"), now}},
		{`INSERT INTO build_live_logs (attempt_id, generation, offset_bytes, data, created_at) VALUES ('01JLIVEATTEMPT000000000001', 1, 15, ?, ?)`, []any{[]byte("#2 RUN echo hi\n"), now}},
	} {
		if _, err := l.db.SQL.Exec(l.db.Rebind(q.sql), q.args...); err != nil {
			t.Fatal(err)
		}
	}
	get := func() (string, string) {
		r := l.call(t, alice.c, "GET", "/builds/"+runID+"/logs", "", nil)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b), r.Header.Get("X-Samuori-Log-Live")
	}
	if body, live := get(); live != "true" || !strings.Contains(body, "=== attempt 1 (running) ===\n#1 FROM alpine\n#2 RUN echo hi\n") {
		t.Errorf("live log = %q (live %q)", body, live)
	}
	if _, err := l.db.SQL.Exec(l.db.Rebind(`UPDATE build_runs SET secret_bearing = ? WHERE id = ?`), true, runID); err != nil {
		t.Fatal(err)
	}
	if body, _ := get(); strings.Contains(body, "FROM alpine") || !strings.Contains(body, "uses secrets") {
		t.Errorf("secret build log = %q", body)
	}
	if _, err := l.db.SQL.Exec(l.db.Rebind(`UPDATE build_attempts SET ended_at = ? WHERE run_id = ?`), now, runID); err != nil {
		t.Fatal(err)
	}
	if body, live := get(); live != "" || strings.Contains(body, "running") {
		t.Errorf("after the attempt ended = %q (live %q)", body, live)
	}
}
