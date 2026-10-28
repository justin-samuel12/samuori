package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestBuildAttemptEgressDenied: the attempts API returns the stored proxy denials with advice computed from the
// current egress policy, and only to members who may read the build.
func TestBuildAttemptEgressDenied(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, _, projB := twoTeams(t, l)
	snap := l.snapshotOf(t, bob, projB, map[string]string{"package.json": `{"name":"b"}`})
	_, _, rev := l.json(t, bob, "POST", "/projects/"+projB+"/specs", doc(nodeSpec), nil)
	st, _, run := l.json(t, bob, "POST", "/builds", fmt.Sprintf(`{"specId":%q,"specRevision":1,"snapshotId":%q}`, rev["specId"], snap), map[string]string{"Idempotency-Key": "egress-build-00000001"})
	if st != 201 {
		t.Fatalf("build = %d %v", st, run)
	}
	id := run["id"].(string)
	report := `{"denied":[{"host":"registry.npmjs.org","port":443,"count":999,"reason":"not on the build's egress allowlist"},{"host":"127.0.0.1","port":80,"count":1,"reason":"not on the build's egress allowlist"}],"unrecorded":2}`
	if _, err := l.db.SQL.Exec(`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at, ended_at, failure_class, egress_report) VALUES ('01JEGRESSATTEMPT0000000001', ?, 1, 1, '01JEGRESSW0RKER00000000001', '01JEGRESSP0000000000000001', 'failed', ?, 'destroyed', ?, ?, 'application', ?)`,
		id, time.Now().UTC(), time.Now().UTC(), time.Now().UTC(), report); err != nil {
		t.Fatal(err)
	}
	st, _, b := l.json(t, bob, "GET", "/builds/"+id+"/attempts", "", nil)
	raw, _ := json.Marshal(b)
	var out struct {
		Items []struct {
			EgressDenied []struct {
				Host      string   `json:"host"`
				Port      int      `json:"port"`
				Count     int      `json:"count"`
				Hint      string   `json:"hint"`
				Presets   []string `json:"presets"`
				Allowable bool     `json:"allowable"`
			} `json:"egressDenied"`
			EgressUnrecorded int `json:"egressUnrecorded"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || st != 200 || len(out.Items) != 1 {
		t.Fatalf("attempts = %d %s", st, raw)
	}
	a := out.Items[0]
	if a.EgressUnrecorded != 2 || len(a.EgressDenied) != 2 {
		t.Fatalf("attempt = %s", raw)
	}
	npm, lo := a.EgressDenied[0], a.EgressDenied[1]
	if npm.Host != "registry.npmjs.org" || npm.Count != 999 || !npm.Allowable || len(npm.Presets) != 1 || npm.Presets[0] != "npm" || !strings.Contains(npm.Hint, "npm preset") {
		t.Errorf("npm denial = %+v", npm)
	}
	if lo.Allowable || !strings.Contains(lo.Hint, "cannot be allowed") {
		t.Errorf("loopback denial = %+v", lo)
	}
	if st, _, _ := l.json(t, alice, "GET", "/builds/"+id+"/attempts", "", nil); st != 404 {
		t.Errorf("another team's developer reads the denials: %d", st)
	}
}
