package api

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/storage"
)

// TestDownloadAndDelete: a validated image downloads as an archive for maintainers, publishers and administrators
// only, never while quarantined; deleting a build keeps its record, hides its image and logs, and is refused while a
// hold uses the image (ADR 0053).
func TestDownloadAndDelete(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	snap := l.snapshotOf(t, alice, projA, map[string]string{"Dockerfile": "FROM alpine:3.20\n"})
	spec := `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"np-image"},
"spec":{"application":{"type":"worker"},"build":{"method":"dockerfile","dockerfile":{"context":".","path":"Dockerfile"}}}}`
	_, _, created := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(spec), nil)
	_, _, run := l.json(t, alice, "POST", "/builds", fmt.Sprintf(`{"specId":%q,"specRevision":1,"snapshotId":%q}`, created["specId"], snap), map[string]string{"Idempotency-Key": "delete-build-00000001"})
	runID := run["id"].(string)
	blobs, err := storage.NewBlobs(filepath.Join(l.dataDir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	image := ociArchiveBytes(t)
	digest, size, err := blobs.Put(bytes.NewReader(image), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var org string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT org_id FROM build_runs WHERE id = ?`), runID).Scan(&org)
	now := time.Now().UTC()
	attempt, artifact, vrun := ids.New(), ids.New(), ids.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at, ended_at, artifact_digest) VALUES (?, ?, 1, 1, '01JDE1W0RKER00000000000001', '01JDE1P0010000000000000001', 'completed', ?, 'destroyed', ?, ?, ?)`, []any{attempt, runID, now, now, now, digest}},
		{`UPDATE build_runs SET state = 'succeeded', current_attempt_id = ?, artifact_digest = ? WHERE id = ?`, []any{attempt, digest, runID}},
		{`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES (?, ?, ?, ?, 1, 'image', ?, ?, ?)`, []any{artifact, org, runID, attempt, digest, size, now}},
		{`INSERT INTO validation_runs (id, org_id, project_id, artifact_id, artifact_digest, build_run_id, trigger_kind, state, verdict, plan, policy_versions, created_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, 'build', 'completed', 'passed', '{}', '{}', ?, ?)`, []any{vrun, org, projA, artifact, digest, runID, now, now}},
	} {
		if _, err := l.db.SQL.Exec(l.db.Rebind(q.sql), q.args...); err != nil {
			t.Fatal(err)
		}
	}
	download := func(m member) (int, []byte, string) {
		r := l.call(t, m.c, "GET", "/artifacts/"+artifact+"/download", "", nil)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, b, r.Header.Get("Content-Disposition")
	}
	if code, _, _ := download(alice); code != 403 {
		t.Errorf("developer download = %d", code)
	}
	code, body, disp := download(admin)
	if code != 200 || !bytes.HasPrefix(body, image[:512]) || !bytes.Contains(body, []byte(`"Config":"blobs/sha256/`)) ||
		!strings.Contains(disp, `filename="`) || !strings.Contains(disp, "np-image") {
		t.Fatalf("admin download = %d, %d bytes, %q", code, len(body), disp)
	}
	var policyID string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT id FROM policies WHERE org_id = ? AND kind = 'publication'`), org).Scan(&policyID)
	setDevDownloads := func(on bool, version int) {
		t.Helper()
		body := fmt.Sprintf(`{"kind":"publication","document":{"developerDownloads":%t}}`, on)
		if st, _, b := l.json(t, admin, "PUT", "/policies/"+policyID, body, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version)}); st != 200 {
			t.Fatalf("publication policy = %d %v", st, b)
		}
	}
	if st, _, _ := l.json(t, alice, "PUT", "/policies/"+policyID, `{"kind":"publication","document":{"developerDownloads":true}}`, map[string]string{"If-Match": `"1"`}); st != 403 {
		t.Errorf("a developer changed the publication policy: %d", st)
	}
	setDevDownloads(true, 1)
	if _, _, me := l.json(t, alice, "GET", "/me", "", nil); me["developerDownloads"] != true {
		t.Errorf("/me = %v", me)
	}
	if code, body, _ := download(alice); code != 200 || !bytes.HasPrefix(body, image[:512]) {
		t.Errorf("developer download with the setting on = %d", code)
	}
	if code, _, _ := download(bob); code == 200 {
		t.Error("a developer of another team downloaded the image")
	}
	setDevDownloads(false, 2)
	if code, _, _ := download(alice); code != 403 {
		t.Errorf("developer download after the setting was turned off = %d", code)
	}

	if _, err := l.db.SQL.Exec(l.db.Rebind(`INSERT INTO artifact_quarantines (id, org_id, digest, worker_id, reason, created_at) VALUES ('01JDE1QVARANT1NE0000000001', ?, ?, 'w', 'compromised', ?)`), org, digest, now); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := download(admin); code != 422 {
		t.Errorf("quarantined download = %d", code)
	}
	if _, err := l.db.SQL.Exec(`UPDATE artifact_quarantines SET released_at = released_at`); err != nil {
		t.Fatal(err)
	}

	if code, _, _ := l.json(t, alice, "POST", "/builds/"+runID+"/deletion", `{}`, nil); code != 403 {
		t.Errorf("developer delete = %d", code)
	}
	if _, err := l.db.SQL.Exec(l.db.Rebind(`INSERT INTO artifact_holds (id, org_id, digest, reason, actor_id, created_at) VALUES ('01JDE1H01D0000000000000001', ?, ?, 'legal', 'u', ?)`), org, digest, now); err != nil {
		t.Fatal(err)
	}
	if code, _, b := l.json(t, admin, "POST", "/builds/"+runID+"/deletion", `{"reason":"replaced by a newer build"}`, nil); code != 409 || !strings.Contains(fmt.Sprint(b), "hold") {
		t.Errorf("delete under a hold = %d %v", code, b)
	}
	if _, err := l.db.SQL.Exec(l.db.Rebind(`UPDATE artifact_holds SET released_at = ?`), now); err != nil {
		t.Fatal(err)
	}
	code, _, del := l.json(t, admin, "POST", "/builds/"+runID+"/deletion", `{"reason":"replaced by a newer build"}`, nil)
	if code != 200 || del["reason"] != "replaced by a newer build" {
		t.Fatalf("delete = %d %v", code, del)
	}
	if code, _, _ := l.json(t, admin, "POST", "/builds/"+runID+"/deletion", `{}`, nil); code != 409 {
		t.Errorf("second delete = %d", code)
	}
	_, _, got := l.json(t, alice, "GET", "/builds/"+runID, "", nil)
	if got["state"] != "succeeded" || got["deletion"] == nil {
		t.Errorf("deleted build = %v", got)
	}
	if r := l.call(t, alice.c, "GET", "/builds/"+runID+"/logs", "", nil); r.StatusCode != 410 {
		t.Errorf("logs of a deleted build = %d", r.StatusCode)
	}
	if code, _, _ := download(admin); code != 404 {
		t.Errorf("download after delete = %d", code)
	}
	if _, _, imgs := l.json(t, alice, "GET", "/projects/"+projA+"/images", "", nil); strings.Contains(fmt.Sprint(imgs), artifact) {
		t.Errorf("a deleted build's image is still listed: %v", imgs)
	}
	var audited int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('image.download', 'build.delete')`).Scan(&audited)
	// The administrator's and the developer's downloads, and the deletion.
	if audited != 3 {
		t.Errorf("audit events = %d", audited)
	}
}

// ociArchiveBytes is a minimal one-image OCI archive with an empty layer.
func ociArchiveBytes(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	put := func(name string, b []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(b)
	}
	blob := func(media string, b []byte) string {
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		put("blobs/sha256/"+h, b)
		return fmt.Sprintf(`{"mediaType":%q,"digest":"sha256:%s","size":%d}`, media, h, len(b))
	}
	var layer bytes.Buffer
	_ = tar.NewWriter(&layer).Close()
	l := blob("application/vnd.oci.image.layer.v1.tar", layer.Bytes())
	c := blob("application/vnd.oci.image.config.v1+json", []byte(`{"architecture":"amd64","os":"linux","config":{}}`))
	m := blob("application/vnd.oci.image.manifest.v1+json", []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":`+c+`,"layers":[`+l+`]}`))
	put("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	put("index.json", []byte(`{"schemaVersion":2,"manifests":[`+m+`]}`))
	_ = tw.Close()
	return out.Bytes()
}
