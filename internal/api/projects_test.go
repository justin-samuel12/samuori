package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/storage"
)

func (l *live) memberWith(t *testing.T, admin member, username, role, teamID string) member {
	t.Helper()
	var u apigen.User
	_ = json.NewDecoder(l.call(t, admin.c, "POST", "/users", fmt.Sprintf(`{"username":%q,"password":"a long secret phrase"}`, username), admin.csrf).Body).Decode(&u)
	if r := l.call(t, admin.c, "POST", "/role-bindings", fmt.Sprintf(`{"principalId":%q,"role":%q,"scopeType":"team","scopeId":%q}`, u.Id, role, teamID), admin.csrf); r.StatusCode != 201 {
		t.Fatalf("bind %s = %d", username, r.StatusCode)
	}
	m := member{l.client(), nil}
	for _, ck := range l.call(t, m.c, "POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"a long secret phrase"}`, username), nil).Cookies() {
		if ck.Name == auth.CSRFCookie {
			m.csrf = map[string]string{auth.CSRFHeader: ck.Value}
		}
	}
	return m
}

// TestProjectArchiveRestorePurge: only the project's maintainers and administrators archive and restore it, and only
// administrators purge it. An archived project is hidden, refuses new work with 409 and loses its grants, which a
// restore does not return. A purge needs the typed project name, removes the project's stored files except those
// another project uses, keeps the row and audit trail, and leaves a project that cannot be restored (ADR 0058).
func TestProjectArchiveRestorePurge(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, projB := twoTeams(t, l)
	admin := l.adminMember(t)
	var org, teamA, teamB string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT org_id, team_id FROM projects WHERE id = ?`), projA).Scan(&org, &teamA)
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), projB).Scan(&teamB)
	carol := l.memberWith(t, admin, "carol", "maintainer", teamA)
	dave := l.memberWith(t, admin, "dave", "maintainer", teamB)

	shared := "FROM alpine:3.20\n"
	snapA := l.snapshotOf(t, alice, projA, map[string]string{"Dockerfile": shared, "only-a.txt": "project a\n"})
	l.snapshotOf(t, bob, projB, map[string]string{"Dockerfile": shared})
	spec := `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"app"},
"spec":{"application":{"type":"worker"},"build":{"method":"dockerfile","dockerfile":{"context":".","path":"Dockerfile"}}}}`
	_, _, created := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(spec), nil)
	specID := created["specId"].(string)
	build := func(key string) (int, body) {
		st, _, b := l.json(t, alice, "POST", "/builds", fmt.Sprintf(`{"specId":%q,"specRevision":1,"snapshotId":%q}`, specID, snapA), map[string]string{"Idempotency-Key": key})
		return st, b
	}
	_, done := build("archive-build-00000001")
	_, waiting := build("archive-build-00000002")
	doneID, waitingID := done["id"].(string), waiting["id"].(string)

	blobs, err := storage.NewBlobs(filepath.Join(l.dataDir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	image, size, err := blobs.Put(bytes.NewReader(ociArchiveBytes(t)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	attempt, artifact, egressReq := ids.New(), ids.New(), ids.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at, ended_at, artifact_digest) VALUES (?, ?, 1, 1, '01JDE1W0RKER00000000000001', '01JDE1P0010000000000000001', 'completed', ?, 'destroyed', ?, ?, ?)`, []any{attempt, doneID, now, now, now, image}},
		{`UPDATE build_runs SET state = 'succeeded', current_attempt_id = ?, artifact_digest = ? WHERE id = ?`, []any{attempt, image, doneID}},
		{`UPDATE build_runs SET state = 'queued' WHERE id = ?`, []any{waitingID}},
		{`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES (?, ?, ?, ?, 1, 'image', ?, ?, ?)`, []any{artifact, org, doneID, attempt, image, size, now}},
		{`INSERT INTO egress_access_requests (id, org_id, project_id, method, entries, justification, evidence, requested_by, created_at) VALUES (?, ?, ?, 'dockerfile', '{}', 'npm registry for installs', '{}', 'u', ?)`, []any{egressReq, org, projA, now}},
		{`INSERT INTO project_egress_allowances (id, org_id, project_id, kind, preset, methods, reason, request_id, status, requested_by, approved_by, created_at) VALUES (?, ?, ?, 'preset', 'npm', '["dockerfile"]', 'installs', ?, 'active', 'u', 'a', ?)`, []any{ids.New(), org, projA, egressReq, now}},
	} {
		if _, err := l.db.SQL.Exec(l.db.Rebind(q.sql), q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/repository-requests", `{"repository":"team-a/app","justification":"somewhere to publish the app"}`, nil); st >= 300 {
		t.Fatalf("repository request = %d %v", st, b)
	}

	for name, m := range map[string]member{"developer": alice, "another team's maintainer": dave} {
		for _, op := range []string{"archive", "restore", "purge"} {
			if st, _, _ := l.json(t, m, "POST", "/projects/"+projA+"/"+op, "", map[string]string{"X-Samuori-Confirm": "app"}); st != 403 && st != 404 {
				t.Errorf("%s %s = %d", name, op, st)
			}
		}
	}

	st, _, arch := l.json(t, carol, "POST", "/projects/"+projA+"/archive", `{"reason":"replaced by a new service"}`, nil)
	if st != 200 {
		t.Fatalf("archive = %d %v", st, arch)
	}
	revoked := arch["revoked"].(map[string]any)
	if arch["project"].(map[string]any)["state"] != "archived" || revoked["egressAllowances"] != 1.0 || revoked["requests"] != 1.0 || arch["cancelledBuilds"] != 1.0 {
		t.Errorf("archive = %v", arch)
	}
	if st, _, _ := l.json(t, carol, "POST", "/projects/"+projA+"/archive", "", nil); st != 409 {
		t.Errorf("second archive = %d", st)
	}
	var allowance, approval, waitingState string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT status FROM project_egress_allowances WHERE project_id = ?`), projA).Scan(&allowance)
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT state FROM approvals WHERE project_id = ?`), projA).Scan(&approval)
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT state FROM build_runs WHERE id = ?`), waitingID).Scan(&waitingState)
	if allowance != "revoked" || approval != "expired" || waitingState != "cancelled" {
		t.Errorf("after archive: allowance %s, approval %s, queued build %s", allowance, approval, waitingState)
	}

	listed := func(m member, query string) string {
		_, _, b := l.json(t, m, "GET", "/projects"+query, "", nil)
		return fmt.Sprint(b["items"])
	}
	if strings.Contains(listed(carol, ""), projA) || !strings.Contains(listed(carol, "?include=archived"), projA) {
		t.Error("an archived project is listed by default, or not listed with include=archived")
	}
	if strings.Contains(listed(dave, "?include=archived"), projA) {
		t.Error("another team sees the archived project")
	}

	refused := map[string]func() (int, body){
		"upload": func() (int, body) {
			st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/uploads", `{"fileCount":1,"totalBytes":10}`, nil)
			return st, b
		},
		"spec": func() (int, body) {
			st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(strings.Replace(spec, `"app"`, `"other"`, 1)), nil)
			return st, b
		},
		"build": func() (int, body) { return build("archive-build-00000003") },
		"publication": func() (int, body) {
			st, _, b := l.json(t, carol, "POST", "/publications", fmt.Sprintf(`{"kind":"publish","artifactId":%q,"destinationId":%q}`, artifact, ids.New()), map[string]string{"Idempotency-Key": "archive-publish-0000001"})
			return st, b
		},
		"repository request": func() (int, body) {
			st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/repository-requests", `{"repository":"team-a/again","justification":"somewhere to publish the app"}`, nil)
			return st, b
		},
	}
	for name, call := range refused {
		if st, b := call(); st != 409 || b["code"] != "project_archived" {
			t.Errorf("%s on an archived project = %d %v", name, st, b)
		}
	}

	if st, _, _ := l.json(t, carol, "POST", "/projects/"+projA+"/purge", "", map[string]string{"X-Samuori-Confirm": "app"}); st != 403 {
		t.Errorf("maintainer purge = %d", st)
	}
	if st, _, _ := l.json(t, admin, "POST", "/projects/"+projB+"/purge", "", map[string]string{"X-Samuori-Confirm": "app"}); st != 409 {
		t.Errorf("purge of an active project = %d", st)
	}
	for _, confirm := range []string{"", "wrong"} {
		if st, _, _ := l.json(t, admin, "POST", "/projects/"+projA+"/purge", "", map[string]string{"X-Samuori-Confirm": confirm}); st != 428 {
			t.Errorf("purge confirmed with %q = %d", confirm, st)
		}
	}

	if st, _, b := l.json(t, carol, "POST", "/projects/"+projA+"/restore", "", nil); st != 200 || b["state"] != "active" {
		t.Fatalf("restore = %d %v", st, b)
	}
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT status FROM project_egress_allowances WHERE project_id = ?`), projA).Scan(&allowance)
	if allowance != "revoked" {
		t.Error("restore brought a revoked grant back")
	}
	if st, _, _ := l.json(t, admin, "POST", "/projects/"+projA+"/purge", "", map[string]string{"X-Samuori-Confirm": "app"}); st != 409 {
		t.Errorf("purge of a restored project = %d", st)
	}
	if st, _, _ := l.json(t, admin, "POST", "/projects/"+projA+"/archive", "", nil); st != 200 {
		t.Fatalf("archive again = %d", st)
	}

	var manifest string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT digest FROM source_snapshots WHERE id = ?`), snapA).Scan(&manifest)
	digestOf := func(content string) string {
		d, _, err := blobs.Put(strings.NewReader(content), 1<<10)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	sharedBlob := digestOf(shared)
	st, _, purged := l.json(t, admin, "POST", "/projects/"+projA+"/purge", "", map[string]string{"X-Samuori-Confirm": "app"})
	if st != 200 || purged["project"].(map[string]any)["state"] != "purged" || !strings.Contains(fmt.Sprint(purged["externalCopies"]), "external registries were not deleted") ||
		purged["removedBlobs"].(float64) < 3 || purged["keptBlobs"].(float64) < 1 {
		t.Fatalf("purge = %d %v", st, purged)
	}
	for name, d := range map[string]string{"image": image, "manifest": manifest} {
		if _, err := blobs.ReadAll(d, 1<<20); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("the purged project's %s is still stored: %v", name, err)
		}
	}
	if _, err := blobs.ReadAll(sharedBlob, 1<<10); err != nil {
		t.Errorf("a file another project uses was removed: %v", err)
	}

	if st, _, b := l.json(t, admin, "POST", "/projects/"+projA+"/purge", "", map[string]string{"X-Samuori-Confirm": "app"}); st != 200 || b["removedBlobs"] != 0.0 {
		t.Errorf("second purge = %d %v", st, b)
	}
	if st, _, b := l.json(t, carol, "POST", "/projects/"+projA+"/restore", "", nil); st != 409 {
		t.Errorf("restore after purge = %d %v", st, b)
	}
	if st, _, b := l.json(t, admin, "GET", "/projects/"+projA, "", nil); st != 200 || b["state"] != "purged" || b["name"] != "app" {
		t.Errorf("purged project = %d %v", st, b)
	}
	var audited int
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT COUNT(*) FROM audit_events WHERE resource_id = ? AND action IN ('project.archive', 'project.restore', 'project.purge', 'project.purge.storage')`), projA).Scan(&audited)
	if audited != 7 {
		t.Errorf("project lifecycle audit events = %d, want 7", audited)
	}
}
