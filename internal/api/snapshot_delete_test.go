package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
)

func TestDeleteOwnUpload(t *testing.T) {
	t.Run("SEC-UPL-07", func(t *testing.T) {
		l := newLiveWith(t, false, smallChunks(), nil)
		alice, bob, projA, _ := twoTeams(t, l)

		admin, csrf := l.login(t)
		h := map[string]string{auth.CSRFHeader: csrf}
		var teams apigen.TeamList
		_ = json.NewDecoder(l.call(t, admin, "GET", "/teams", "", nil).Body).Decode(&teams)
		var u apigen.User
		_ = json.NewDecoder(l.call(t, admin, "POST", "/users", `{"username":"carol","password":"a long secret phrase"}`, h).Body).Decode(&u)
		for _, tm := range teams.Items {
			if tm.Name == "team-a" {
				l.call(t, admin, "POST", "/role-bindings", fmt.Sprintf(`{"principalId":%q,"role":"developer","scopeType":"team","scopeId":%q}`, u.Id, tm.Id), h)
			}
		}
		carol := member{l.client(), map[string]string{}}
		for _, ck := range l.call(t, carol.c, "POST", "/auth/login", `{"username":"carol","password":"a long secret phrase"}`, nil).Cookies() {
			if ck.Name == auth.CSRFCookie {
				carol.csrf[auth.CSRFHeader] = ck.Value
			}
		}

		snapshot := func(files map[string]string) apigen.SourceSnapshot {
			s := l.upload(t, alice, projA, tarOf(t, files), 1024)
			_, op := l.commit(t, alice, s.Id, s.Id[len(s.Id)-8:])
			done := l.await(t, alice, op.Id)
			var snap apigen.SourceSnapshot
			_ = json.NewDecoder(l.call(t, alice.c, "GET", "/snapshots/"+*done.ResourceId, "", nil).Body).Decode(&snap)
			return snap
		}
		blob := func(content string) string {
			sum := sha256.Sum256([]byte(content))
			x := hex.EncodeToString(sum[:])
			return filepath.Join(l.dataDir, "blobs", "sha256", x[:2], x)
		}
		exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

		shared, only := `{"name":"api"}`, "console.log('only in the first upload')"
		first := snapshot(map[string]string{"package.json": shared, "index.js": only})
		second := snapshot(map[string]string{"package.json": shared, "server.js": "console.log('second')"})
		if !exists(blob(only)) || !exists(blob(shared)) {
			t.Fatal("uploaded files are not in storage")
		}

		if r := l.call(t, carol.c, "DELETE", "/snapshots/"+first.Id, "", carol.csrf); r.StatusCode != 403 {
			t.Fatalf("a teammate deleted someone else's upload: %d", r.StatusCode)
		}
		if r := l.call(t, bob.c, "DELETE", "/snapshots/"+first.Id, "", bob.csrf); r.StatusCode != 404 {
			t.Fatalf("another team deleted the upload: %d", r.StatusCode)
		}
		if r := l.call(t, alice.c, "DELETE", "/snapshots/"+first.Id, "", alice.csrf); r.StatusCode != 204 {
			t.Fatalf("the uploader could not delete her upload: %d", r.StatusCode)
		}

		if exists(blob(only)) {
			t.Error("a file only the deleted upload used is still stored")
		}
		if !exists(blob(shared)) {
			t.Error("a file another upload uses was removed")
		}
		var got apigen.SourceSnapshot
		_ = json.NewDecoder(l.call(t, alice.c, "GET", "/snapshots/"+first.Id, "", nil).Body).Decode(&got)
		if got.DeletedAt == nil {
			t.Error("the deleted upload's record does not say it was deleted")
		}
		var list apigen.SourceSnapshotList
		_ = json.NewDecoder(l.call(t, alice.c, "GET", "/projects/"+projA+"/snapshots", "", nil).Body).Decode(&list)
		if len(list.Items) != 1 || list.Items[0].Id != second.Id {
			t.Errorf("uploads listed after the delete: %+v", list.Items)
		}
		dh := map[string]string{auth.CSRFHeader: alice.csrf[auth.CSRFHeader], "Idempotency-Key": "discover-deleted-000000000"}
		if r := l.call(t, alice.c, "POST", "/snapshots/"+first.Id+"/discoveries", "", dh); r.StatusCode != 410 {
			t.Errorf("analysing a deleted upload = %d, want 410", r.StatusCode)
		}
		if r := l.call(t, alice.c, "DELETE", "/snapshots/"+first.Id, "", alice.csrf); r.StatusCode != 404 {
			t.Errorf("deleting twice = %d", r.StatusCode)
		}
		var n int
		_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'snapshot.delete'`).Scan(&n)
		if n != 1 {
			t.Errorf("snapshot.delete audit events = %d", n)
		}
	})
}
