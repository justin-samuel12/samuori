package api

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/sources"
)

type member struct {
	c    *http.Client
	csrf map[string]string
}

// twoTeams creates two teams with one project each and a developer on each team.
func twoTeams(t *testing.T, l *live) (alice, bob member, projA, projB string) {
	t.Helper()
	l.setup(t)
	admin, csrf := l.login(t)
	h := map[string]string{auth.CSRFHeader: csrf}
	mk := func(team, user string) (member, string) {
		var tm apigen.Team
		_ = json.NewDecoder(l.call(t, admin, "POST", "/teams", fmt.Sprintf(`{"name":%q}`, team), h).Body).Decode(&tm)
		var pr apigen.Project
		_ = json.NewDecoder(l.call(t, admin, "POST", "/projects", fmt.Sprintf(`{"name":"app","teamId":%q}`, tm.Id), h).Body).Decode(&pr)
		var u apigen.User
		_ = json.NewDecoder(l.call(t, admin, "POST", "/users", fmt.Sprintf(`{"username":%q,"password":"a long secret phrase"}`, user), h).Body).Decode(&u)
		if r := l.call(t, admin, "POST", "/role-bindings", fmt.Sprintf(`{"principalId":%q,"role":"developer","scopeType":"team","scopeId":%q}`, u.Id, tm.Id), h); r.StatusCode != 201 {
			t.Fatalf("bind = %d", r.StatusCode)
		}
		c := l.client()
		resp := l.call(t, c, "POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"a long secret phrase"}`, user), nil)
		for _, ck := range resp.Cookies() {
			if ck.Name == auth.CSRFCookie {
				return member{c, map[string]string{auth.CSRFHeader: ck.Value}}, pr.Id
			}
		}
		t.Fatal("login failed")
		return member{}, ""
	}
	alice, projA = mk("team-a", "alice")
	bob, projB = mk("team-b", "bob")
	return
}

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return buf.Bytes()
}

func (l *live) put(t *testing.T, m member, upload string, index int, data []byte) int {
	t.Helper()
	req, _ := http.NewRequest("PUT", fmt.Sprintf("%s/api/v1/uploads/%s/chunks/%d", l.srv.URL, upload, index), bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(auth.CSRFHeader, m.csrf[auth.CSRFHeader])
	resp, err := m.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (l *live) upload(t *testing.T, m member, project string, archive []byte, chunk int) apigen.UploadSession {
	t.Helper()
	var s apigen.UploadSession
	r := l.call(t, m.c, "POST", "/projects/"+project+"/uploads", fmt.Sprintf(`{"fileCount":10,"totalBytes":%d}`, len(archive)), m.csrf)
	if r.StatusCode != 201 {
		t.Fatalf("create upload = %d", r.StatusCode)
	}
	_ = json.NewDecoder(r.Body).Decode(&s)
	for i := 0; i*chunk < len(archive); i++ {
		if st := l.put(t, m, s.Id, i, archive[i*chunk:min((i+1)*chunk, len(archive))]); st != 204 {
			t.Fatalf("chunk %d = %d", i, st)
		}
	}
	return s
}

func (l *live) commit(t *testing.T, m member, upload, key string) (int, apigen.Operation) {
	t.Helper()
	h := map[string]string{auth.CSRFHeader: m.csrf[auth.CSRFHeader], "Idempotency-Key": "commit-key-" + key + "-0000000"}
	r := l.call(t, m.c, "POST", "/uploads/"+upload+"/commit", "", h)
	var op apigen.Operation
	_ = json.NewDecoder(r.Body).Decode(&op)
	return r.StatusCode, op
}

func (l *live) await(t *testing.T, m member, opID string) apigen.Operation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var op apigen.Operation
		_ = json.NewDecoder(l.call(t, m.c, "GET", "/operations/"+opID, "", nil).Body).Decode(&op)
		if op.Status == "succeeded" || op.Status == "failed" {
			return op
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return apigen.Operation{}
}

func smallChunks() sources.Config {
	cfg := sources.DefaultConfig()
	cfg.ChunkBytes = 1024
	return cfg
}

func TestUploadToSnapshot(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, _ := twoTeams(t, l)
	archive := tarOf(t, map[string]string{"package.json": `{"name":"api"}`, "src/index.js": strings.Repeat("x", 3000), ".env": "SECRET=1"})
	s := l.upload(t, alice, projA, archive, 1024)
	if s.MaxChunks != (len(archive)+1023)/1024 || s.ChunkBytes != 1024 {
		t.Fatalf("session = %+v", s)
	}

	var got apigen.UploadSession
	_ = json.NewDecoder(l.call(t, alice.c, "GET", "/uploads/"+s.Id, "", nil).Body).Decode(&got)
	if len(got.ReceivedChunks) != s.MaxChunks || got.Status != "open" {
		t.Fatalf("progress = %+v", got)
	}
	if r := l.call(t, bob.c, "GET", "/uploads/"+s.Id, "", nil); r.StatusCode != 404 {
		t.Fatalf("other team reads upload = %d", r.StatusCode)
	}
	if st := l.put(t, bob, s.Id, 0, archive[:1024]); st != 404 {
		t.Fatalf("other team writes chunk = %d", st)
	}

	st, op := l.commit(t, alice, s.Id, "k1")
	if st != 202 {
		t.Fatalf("commit = %d", st)
	}
	if st, again := l.commit(t, alice, s.Id, "k1"); st != 202 || again.Id != op.Id {
		t.Fatalf("idempotent commit = %d %s vs %s", st, again.Id, op.Id)
	}
	if st, _ := l.commit(t, alice, s.Id, "k2"); st != 409 {
		t.Fatalf("second commit with a new key = %d", st)
	}
	if st := l.put(t, alice, s.Id, 0, archive[:1024]); st != 409 {
		t.Fatalf("chunk after commit = %d", st)
	}
	if r := l.call(t, bob.c, "GET", "/operations/"+op.Id, "", nil); r.StatusCode != 404 {
		t.Fatalf("other user reads operation = %d", r.StatusCode)
	}
	done := l.await(t, alice, op.Id)
	if done.Status != "succeeded" || done.ResourceId == nil {
		t.Fatalf("operation = %+v", done)
	}

	var snap apigen.SourceSnapshot
	_ = json.NewDecoder(l.call(t, alice.c, "GET", "/snapshots/"+*done.ResourceId, "", nil).Body).Decode(&snap)
	if snap.FileCount != 2 || *snap.ExcludedEntries != 1 || !strings.HasPrefix(snap.Digest, "sha256:") {
		t.Fatalf("snapshot = %+v", snap)
	}
	if r := l.call(t, bob.c, "GET", "/snapshots/"+snap.Id, "", nil); r.StatusCode != 404 {
		t.Fatalf("other team reads snapshot = %d", r.StatusCode)
	}
	var list apigen.SourceSnapshotList
	_ = json.NewDecoder(l.call(t, alice.c, "GET", "/projects/"+projA+"/snapshots", "", nil).Body).Decode(&list)
	if len(list.Items) != 1 || list.Items[0].Digest != snap.Digest {
		t.Fatalf("list = %+v", list)
	}

	again := l.upload(t, alice, projA, archive, 1024)
	_, op2 := l.commit(t, alice, again.Id, "k3")
	done2 := l.await(t, alice, op2.Id)
	var snap2 apigen.SourceSnapshot
	_ = json.NewDecoder(l.call(t, alice.c, "GET", "/snapshots/"+*done2.ResourceId, "", nil).Body).Decode(&snap2)
	if snap2.Id == snap.Id || snap2.Digest != snap.Digest {
		t.Fatal("re-upload must create a new snapshot with the same content digest")
	}

	t.Run("INT-AUD-01", func(t *testing.T) {
		for _, a := range []string{"upload.create", "upload.commit", "snapshot.create"} {
			var n int
			_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = ?`, a).Scan(&n)
			if n != 2 {
				t.Errorf("%s = %d", a, n)
			}
		}
	})
}

func TestUploadChunkRulesAndFailures(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, projB := twoTeams(t, l)
	if r := l.call(t, alice.c, "POST", "/projects/"+projB+"/uploads", `{"fileCount":1,"totalBytes":10}`, alice.csrf); r.StatusCode != 404 {
		t.Fatalf("upload to other team's project = %d", r.StatusCode)
	}
	if r := l.call(t, alice.c, "POST", "/projects/"+projA+"/uploads", `{"fileCount":1,"totalBytes":10,"excludePatterns":["!keep"]}`, alice.csrf); r.StatusCode != 422 {
		t.Fatalf("negated exclude = %d", r.StatusCode)
	}
	if r := l.call(t, alice.c, "POST", "/projects/"+projA+"/uploads", fmt.Sprintf(`{"fileCount":1,"totalBytes":%d}`, int64(2)<<30), alice.csrf); r.StatusCode != 422 {
		t.Fatalf("oversize upload = %d", r.StatusCode)
	}

	archive := tarOf(t, map[string]string{"README.md": "a", "readme.md": "b"})
	var s apigen.UploadSession
	_ = json.NewDecoder(l.call(t, alice.c, "POST", "/projects/"+projA+"/uploads", fmt.Sprintf(`{"fileCount":2,"totalBytes":%d}`, len(archive)), alice.csrf).Body).Decode(&s)
	if st := l.put(t, alice, s.Id, 0, archive[:100]); st != 422 {
		t.Fatalf("short chunk = %d", st)
	}
	if st := l.put(t, alice, s.Id, s.MaxChunks, archive[:1024]); st != 422 {
		t.Fatalf("index past end = %d", st)
	}
	if st, _ := l.commit(t, alice, s.Id, "x"); st != 422 {
		t.Fatalf("incomplete commit = %d", st)
	}
	for i := 0; i < s.MaxChunks; i++ {
		l.put(t, alice, s.Id, i, archive[i*1024:min((i+1)*1024, len(archive))])
	}
	_, op := l.commit(t, alice, s.Id, "y")
	failed := l.await(t, alice, op.Id)
	if failed.Status != "failed" || failed.Error == nil || failed.Error.Code != "path_collision" {
		t.Fatalf("collision operation = %+v", failed)
	}
}

func TestRequestLimits(t *testing.T) {
	t.Run("SEC-DOS-01", func(t *testing.T) {
		cfg := smallChunks()
		cfg.MaxOpenSessions = 2
		l := newLiveWith(t, false, cfg, func(d *Deps) { d.RateLimit = RateLimit{PerSecond: 0.01, Burst: 25} })
		alice, _, projA, _ := twoTeams(t, l)

		big := `{"name":"` + strings.Repeat("x", 2<<20) + `"}`
		if r := l.call(t, alice.c, "POST", "/projects", big, alice.csrf); r.StatusCode != 413 {
			t.Fatalf("2 MiB JSON body = %d", r.StatusCode)
		}
		var s apigen.UploadSession
		_ = json.NewDecoder(l.call(t, alice.c, "POST", "/projects/"+projA+"/uploads", `{"fileCount":1,"totalBytes":100}`, alice.csrf).Body).Decode(&s)
		if st := l.put(t, alice, s.Id, 0, make([]byte, 9<<20)); st != 413 {
			t.Fatalf("9 MiB chunk = %d", st)
		}
		l.call(t, alice.c, "POST", "/projects/"+projA+"/uploads", `{"fileCount":1,"totalBytes":100}`, alice.csrf)
		if r := l.call(t, alice.c, "POST", "/projects/"+projA+"/uploads", `{"fileCount":1,"totalBytes":100}`, alice.csrf); r.StatusCode != 429 {
			t.Fatalf("third open upload = %d", r.StatusCode)
		}

		limited := false
		for range 40 {
			if r := l.call(t, alice.c, "GET", "/me", "", nil); r.StatusCode == 429 {
				if r.Header.Get("Retry-After") == "" {
					t.Fatal("429 without Retry-After")
				}
				limited = true
				break
			}
		}
		if !limited {
			t.Fatal("per-caller rate limit never applied")
		}
	})
}
