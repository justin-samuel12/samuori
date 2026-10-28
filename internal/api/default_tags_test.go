package api

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/storage"
)

// TestDefaultTags: an image's default tags come from its spec with {version} resolved; saving a spec whose fixed tag
// the project already published returns a warning but still saves (ADR 0054).
func TestDefaultTags(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	snap := l.snapshotOf(t, alice, projA, map[string]string{"Dockerfile": "FROM alpine:3.20\n"})
	spec := func(tags string) string {
		return `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"np-image"},
"spec":{"application":{"type":"worker"},"build":{"method":"dockerfile","dockerfile":{"context":".","path":"Dockerfile"}},"outputs":{"image":{"tags":` + tags + `}}}}`
	}
	if code, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(spec(`["samuori-x"]`)), nil); code != 422 && code != 400 {
		t.Errorf("reserved tag = %d %v", code, b)
	}
	code, _, created := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(spec(`["{version}","stable"]`)), nil)
	if code != 201 || created["warnings"] != nil {
		t.Fatalf("create = %d %v", code, created)
	}
	specID := created["specId"].(string)
	_, _, run := l.json(t, alice, "POST", "/builds", fmt.Sprintf(`{"specId":%q,"specRevision":1,"snapshotId":%q}`, specID, snap), map[string]string{"Idempotency-Key": "default-tags-build-0001"})
	runID := run["id"].(string)
	blobs, err := storage.NewBlobs(filepath.Join(l.dataDir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	digest, size, err := blobs.Put(bytes.NewReader([]byte("image")), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var org string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT org_id FROM build_runs WHERE id = ?`), runID).Scan(&org)
	now := time.Now().UTC()
	attempt, artifact, reg, prof, dest := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at, ended_at, artifact_digest) VALUES (?, ?, 1, 1, ?, ?, 'completed', ?, 'destroyed', ?, ?, ?)`, []any{attempt, runID, ids.New(), ids.New(), now, now, now, digest}},
		{`UPDATE build_runs SET state = 'succeeded', current_attempt_id = ?, artifact_digest = ?, version = '2026-10-27.1' WHERE id = ?`, []any{attempt, digest, runID}},
		{`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES (?, ?, ?, ?, 1, 'image', ?, ?, ?)`, []any{artifact, org, runID, attempt, digest, size, now}},
		{`INSERT INTO registries (id, org_id, name, kind, endpoint, plain_http, allow_private_network, created_by, created_at) VALUES (?, ?, 'local', 'oci', 'registry.example.com', ?, ?, 'u', ?)`, []any{reg, org, false, false, now}},
		{`INSERT INTO registry_profiles (id, org_id, registry_id, name, type, purpose, username, repository_scopes, status, created_by, created_at) VALUES (?, ?, ?, 'push', 'anonymous', 'push', '', '[]', 'active', 'u', ?)`, []any{prof, org, reg, now}},
		{`INSERT INTO destinations (id, org_id, name, registry_id, repository, push_profile_id, accepts_restricted, allowed_project_ids, protected_tags, immutable_tags, created_by, created_at) VALUES (?, ?, 'prod', ?, 'team/np-image', ?, ?, ?, '[]', ?, 'u', ?)`, []any{dest, org, reg, prof, false, `["` + projA + `"]`, false, now}},
		{`INSERT INTO publications (id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, gate_decisions, requested_by, idempotency_key, created_at, updated_at) VALUES (?, ?, ?, 'publish', ?, ?, ?, '["stable"]', 'published', '[]', 'u', 'k-default-tags', ?, ?)`, []any{ids.New(), org, projA, artifact, digest, dest, now, now}},
	} {
		if _, err := l.db.SQL.Exec(l.db.Rebind(q.sql), q.args...); err != nil {
			t.Fatal(err)
		}
	}
	_, _, art := l.json(t, alice, "GET", "/artifacts/"+artifact, "", nil)
	if fmt.Sprint(art["defaultTags"]) != "[2026-10-27.1 stable]" || art["version"] != "2026-10-27.1" {
		t.Errorf("artifact = %v", art)
	}
	code, _, rev := l.json(t, alice, "POST", "/specs/"+specID+"/revisions", doc(spec(`["{version}","stable","latest"]`)), map[string]string{"If-Match": `"1"`})
	if code != 201 || !strings.Contains(fmt.Sprint(rev["warnings"]), "stable already names an image (version 2026-10-27.1) published to prod") || strings.Contains(fmt.Sprint(rev["warnings"]), "latest") {
		t.Errorf("revision = %d %v", code, rev)
	}
}
