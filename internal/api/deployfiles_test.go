package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/storage"
)

// TestDeploymentFiles: deployment files name the newest successful publication by tag and digest, or the chosen one,
// else the docker load name; they carry no secret IDs or build argument values; other teams get 404 (ADR 0059).
func TestDeploymentFiles(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, _ := twoTeams(t, l)
	snap := l.snapshotOf(t, alice, projA, map[string]string{"Dockerfile": "FROM alpine:3.20\n"})
	spec := `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"orders-api"},
"spec":{"application":{"type":"http-service","port":8080,"healthPath":"/healthz"},
"build":{"method":"dockerfile","dockerfile":{"context":".","path":"Dockerfile"},"args":[{"name":"FLAVOUR","value":"flavour-value-7f3a","confirmedNotSecret":true}]},
"secrets":[{"id":"npm-token-7f3a","required":false}]}}`
	code, _, created := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(spec), nil)
	if code != 201 {
		t.Fatalf("create spec = %d %v", code, created)
	}
	_, _, run := l.json(t, alice, "POST", "/builds", fmt.Sprintf(`{"specId":%q,"specRevision":1,"snapshotId":%q}`, created["specId"], snap), map[string]string{"Idempotency-Key": "deploy-files-build-0001"})
	runID := run["id"].(string)
	blobs, err := storage.NewBlobs(filepath.Join(l.dataDir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	digest, size, err := blobs.Put(bytes.NewReader(ociArchiveBytes(t)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var org string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT org_id FROM build_runs WHERE id = ?`), runID).Scan(&org)
	now := time.Now().UTC()
	attempt, artifact, reg, prof, destA, destB := ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := l.db.SQL.Exec(l.db.Rebind(sql), args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at, ended_at, artifact_digest) VALUES (?, ?, 1, 1, ?, ?, 'completed', ?, 'destroyed', ?, ?, ?)`, attempt, runID, ids.New(), ids.New(), now, now, now, digest)
	exec(`UPDATE build_runs SET state = 'succeeded', current_attempt_id = ?, artifact_digest = ?, version = '2026-10-27.1' WHERE id = ?`, attempt, digest, runID)
	exec(`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES (?, ?, ?, ?, 1, 'image', ?, ?, ?)`, artifact, org, runID, attempt, digest, size, now)

	get := func(m member, path string) (int, []byte) {
		t.Helper()
		r := l.call(t, m.c, "GET", "/artifacts/"+artifact+"/deploy"+path, "", nil)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, b
	}
	noLeak := func(what string, b []byte) {
		t.Helper()
		if bytes.Contains(b, []byte("7f3a")) {
			t.Errorf("%s holds a secret ID or build argument value:\n%s", what, b)
		}
	}
	for _, p := range []string{"", "/compose", "/helm"} {
		if code, _ := get(bob, p); code != 404 {
			t.Errorf("another team's developer GET deploy%s = %d", p, code)
		}
	}
	_, _, info := l.json(t, alice, "GET", "/artifacts/"+artifact+"/deploy", "", nil)
	ref, _ := info["reference"].(string)
	if info["published"] != false || !strings.HasSuffix(ref, "/orders-api:2026-10-27.1") || strings.Contains(ref, "@") || info["chartFilename"] != "orders-api-2026.1027.1.tgz" {
		t.Errorf("unpublished deploy = %v", info)
	}
	code, compose := get(alice, "/compose")
	if code != 200 || !bytes.Contains(compose, []byte(`image: "`+ref+`"`)) || !bytes.Contains(compose, []byte("pull_policy: never")) || !bytes.Contains(compose, []byte(`"8080:8080"`)) {
		t.Errorf("unpublished compose = %d\n%s", code, compose)
	}
	noLeak("compose.yml", compose)

	image := "sha256:" + strings.Repeat("ab", 32)
	older, newer, failed := ids.New(), ids.New(), ids.New()
	exec(`INSERT INTO registries (id, org_id, name, kind, endpoint, plain_http, allow_private_network, created_by, created_at) VALUES (?, ?, 'example', 'oci', 'registry.example.com', ?, ?, 'u', ?)`, reg, org, false, false, now)
	exec(`INSERT INTO registry_profiles (id, org_id, registry_id, name, type, purpose, username, repository_scopes, status, created_by, created_at) VALUES (?, ?, ?, 'push', 'anonymous', 'push', '', '[]', 'active', 'u', ?)`, prof, org, reg, now)
	for _, d := range []struct{ id, name string }{{destA, "staging"}, {destB, "prod"}} {
		exec(`INSERT INTO destinations (id, org_id, name, registry_id, repository, push_profile_id, accepts_restricted, allowed_project_ids, protected_tags, immutable_tags, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '[]', ?, 'u', ?)`,
			d.id, org, d.name, reg, "team/"+d.name, prof, false, `["`+projA+`"]`, false, now)
	}
	insertPub := func(id, dest, repo, tags, state string, at time.Time) {
		exec(`INSERT INTO publications (id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, gate_decisions, image_digest, destination_ref, requested_by, idempotency_key, created_at, updated_at) VALUES (?, ?, ?, 'publish', ?, ?, ?, ?, ?, '[]', ?, ?, 'u', ?, ?, ?)`,
			id, org, projA, artifact, digest, dest, tags, state, image, "registry.example.com/"+repo+"@"+image, "k-"+id, at, at)
	}
	insertPub(older, destA, "team/staging", `[]`, "published", now.Add(-time.Hour))
	insertPub(newer, destB, "team/prod", `["stable","latest"]`, "published", now)
	insertPub(failed, destB, "team/prod", `["broken"]`, "failed", now.Add(time.Minute))

	_, _, info = l.json(t, alice, "GET", "/artifacts/"+artifact+"/deploy", "", nil)
	if info["published"] != true || info["publicationId"] != newer || info["reference"] != "registry.example.com/team/prod:stable@"+image || len(info["publications"].([]any)) != 2 {
		t.Errorf("published deploy = %v", info)
	}
	_, _, info = l.json(t, alice, "GET", "/artifacts/"+artifact+"/deploy?publicationId="+older, "", nil)
	if info["reference"] != "registry.example.com/team/staging:samuori-"+strings.Repeat("ab", 6)+"@"+image {
		t.Errorf("chosen publication deploy = %v", info)
	}
	if code, _ := get(alice, "?publicationId="+failed); code != 422 {
		t.Errorf("a failed publication was accepted: %d", code)
	}
	code, compose = get(alice, "/compose?publicationId="+older)
	if code != 200 || !bytes.Contains(compose, []byte(`image: "registry.example.com/team/staging:samuori-`)) || bytes.Contains(compose, []byte("pull_policy")) {
		t.Errorf("published compose = %d\n%s", code, compose)
	}

	code, chart := get(alice, "/helm")
	if code != 200 {
		t.Fatalf("helm = %d %s", code, chart)
	}
	zr, err := gzip.NewReader(bytes.NewReader(chart))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name], _ = io.ReadAll(tr)
		noLeak(h.Name, files[h.Name])
	}
	values := string(files["orders-api/values.yaml"])
	if files["orders-api/Chart.yaml"] == nil || files["orders-api/templates/service.yml"] == nil ||
		!strings.Contains(values, `repository: "registry.example.com/team/prod"`) || !strings.Contains(values, `digest: "`+image+`"`) || !strings.Contains(values, `path: "/healthz"`) {
		t.Errorf("chart files = %v\nvalues:\n%s", len(files), values)
	}
	var audited int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'image.deployment_files'`).Scan(&audited)
	if audited != 3 {
		t.Errorf("audit events = %d", audited)
	}
}
