package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/cli"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/storage"
)

// TestCLI drives the samuori command against a live server with a service identity's API token: one success and one
// refusal per command group, profiles stored privately, and ci failing on a blocked build.
func TestCLI(t *testing.T) {
	cli.PollInterval = 20 * time.Millisecond
	l := newLiveWith(t, false, smallChunks(), nil)
	_, _, projA, projB := twoTeams(t, l)
	admin := l.adminMember(t)
	_, _, bot := l.json(t, admin, "POST", "/service-identities", `{"username":"ci-bot"}`, nil)
	botID := bot["id"].(string)
	l.json(t, admin, "POST", "/role-bindings", `{"principalId":"`+botID+`","role":"developer","scopeType":"project","scopeId":"`+projA+`"}`, nil)
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	_, _, tok := l.json(t, admin, "POST", "/service-identities/"+botID+"/api-tokens", `{"name":"ci","expiresAt":"`+expires+`","scopes":["read","write"]}`, nil)
	token := tok["token"].(string)

	// With succeed set, a new build is finished as a worker would, before the CLI sees it.
	var succeed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !succeed.Load() || r.Method != "POST" || r.URL.Path != "/api/v1/builds" {
			l.srv.Config.Handler.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		l.srv.Config.Handler.ServeHTTP(rec, r)
		var b struct{ ID string }
		if rec.Code == http.StatusCreated && json.Unmarshal(rec.Body.Bytes(), &b) == nil {
			finishBuild(t, l, b.ID, projA)
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	envs := map[string]string{"SAMUORI_URL": srv.URL, "SAMUORI_TOKEN": token, "HOME": home}
	stdin := ""
	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), args, func(k string) string { return envs[k] }, strings.NewReader(stdin), &out, &errOut)
		if strings.Contains(out.String()+errOut.String(), token) {
			t.Fatalf("%v printed the token", args)
		}
		return code, strings.TrimSpace(out.String()), errOut.String()
	}
	expect := func(want int, args ...string) string {
		t.Helper()
		code, out, msg := run(args...)
		if code != want {
			t.Fatalf("%v = %d, want %d\n%s\n%s", args, code, want, out, msg)
		}
		return out + msg
	}

	t.Run("profiles", func(t *testing.T) {
		envs = map[string]string{"HOME": home}
		stdin = token + "\n"
		if out := expect(0, "login", "--profile", "dev", "--url", srv.URL); !strings.Contains(out, "Signed in") {
			t.Fatalf("login = %s", out)
		}
		dir := filepath.Join(home, ".config", "samuori")
		for path, want := range map[string]fs.FileMode{dir: 0o700, filepath.Join(dir, "config.json"): 0o600, filepath.Join(dir, "tokens", "dev"): 0o600} {
			if st, err := os.Stat(path); err != nil || st.Mode().Perm() != want {
				t.Errorf("%s mode = %v, %v; want %v", path, st.Mode().Perm(), err, want)
			}
		}
		if out := expect(0, "whoami"); !strings.Contains(out, "developer on project "+projA) {
			t.Fatalf("whoami from the profile = %s", out)
		}
		if out := expect(0, "profiles"); !strings.Contains(out, "dev") || !strings.Contains(out, "stored") {
			t.Fatalf("profiles = %s", out)
		}
		envs["SAMUORI_URL"] = "https://other.example.com"
		if out := expect(cli.ExitUsage, "whoami"); !strings.Contains(out, "SAMUORI_TOKEN") {
			t.Fatalf("the profile token went to another URL: %s", out)
		}
		delete(envs, "SAMUORI_URL")
		stdin = "samuori_" + strings.Repeat("x", 40) + "\n"
		if out := expect(cli.ExitError, "login", "--profile", "bad", "--url", srv.URL); !strings.Contains(out, "401") {
			t.Fatalf("login with a bad token = %s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, "tokens", "bad")); err == nil {
			t.Fatal("a refused token was stored")
		}
		expect(0, "logout", "--profile", "dev")
		if _, err := os.Stat(filepath.Join(dir, "tokens", "dev")); err == nil {
			t.Fatal("logout left the token")
		}
		envs = map[string]string{"SAMUORI_URL": srv.URL, "SAMUORI_TOKEN": token, "HOME": home}
		stdin = ""
	})

	t.Run("projects", func(t *testing.T) {
		if out := expect(0, "projects"); !strings.Contains(out, projA) || strings.Contains(out, projB) {
			t.Fatalf("projects = %s", out)
		}
		var team string
		_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), projA).Scan(&team)
		if out := expect(cli.ExitError, "projects", "create", "web", "--team", team); !strings.Contains(out, "403") {
			t.Fatalf("a developer creates a project = %s", out)
		}
	})

	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM alpine:3.20\nEXPOSE 8080\n"), 0o644)
	_ = os.WriteFile(filepath.Join(src, ".env"), []byte("TOKEN=hunter2"), 0o600)
	work := t.TempDir()
	specFile := filepath.Join(work, "samuori.spec.yml")
	var snap string

	t.Run("specs and sources", func(t *testing.T) {
		expect(0, "spec", "init", "web", "--method", "dockerfile", "-o", specFile)
		if out := expect(0, "spec", "validate", specFile); !strings.Contains(out, "valid sha256:") {
			t.Fatalf("validate = %s", out)
		}
		bad := filepath.Join(work, "bad.yml")
		_ = os.WriteFile(bad, []byte("apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\nmetadata:\n  name: web\nspec:\n  build:\n    method: magic\n"), 0o644)
		expect(cli.ExitFailed, "spec", "validate", bad)
		code, specID, msg := run("spec", "apply", specFile, "--project", "app")
		if code != 0 || !strings.Contains(msg, "created spec web revision 1") {
			t.Fatalf("apply = %d %s %s", code, specID, msg)
		}
		if out := expect(0, "spec", "apply", specFile, "--project", "app"); !strings.Contains(out, "unchanged at revision 1") {
			t.Fatalf("second apply = %s", out)
		}
		if out := expect(0, "projects", "show", "app"); !strings.Contains(out, "web") || !strings.Contains(out, specID) {
			t.Fatalf("projects show = %s", out)
		}
		code, out, msg := run("upload", "--project", "app", src)
		if code != 0 || !strings.Contains(msg, "packed 1 files (1 excluded") {
			t.Fatalf("upload = %d %s %s", code, out, msg)
		}
		snap = out
		if out := expect(0, "analyze", snap); !strings.Contains(out, "Dockerfile") || !strings.Contains(out, "8080") {
			t.Fatalf("analyze = %s", out)
		}
		if out := expect(0, "spec", "init", "web", "--snapshot", snap, "-o", "-"); !strings.Contains(out, "method: dockerfile") {
			t.Fatalf("spec init from discovery = %s", out)
		}
		expect(cli.ExitError, "upload", "--project", projB, src)
	})

	t.Run("builds and ci", func(t *testing.T) {
		code, id, msg := run("build", "--spec", "web", "--snapshot", snap, "--wait", "--timeout", "30s")
		if code != cli.ExitFailed || !strings.Contains(msg, "blocked") || id == "" {
			t.Fatalf("build --wait = %d %q %s", code, id, msg)
		}
		if out := expect(0, "status", id); !strings.Contains(out, `"state": "blocked"`) {
			t.Fatalf("status = %s", out)
		}
		if out := expect(0, "builds", "--project", "app"); !strings.Contains(out, id) || !strings.Contains(out, "blocked") {
			t.Fatalf("builds = %s", out)
		}
		expect(0, "logs", "--follow", id)
		expect(cli.ExitError, "cancel", id)
		if out := expect(cli.ExitFailed, "ci", "--spec", "web", "--project", "app", src); !strings.Contains(out, "blocked") {
			t.Fatalf("ci with a blocked build = %s", out)
		}

		succeed.Store(true)
		defer succeed.Store(false)
		deploy := filepath.Join(work, "deploy")
		var sum struct {
			BuildID, ArtifactID string
			DeploymentFiles     []string
		}
		code, out, msg := run("--json", "ci", "--spec", "web", "--deploy-files", deploy, src)
		if err := json.Unmarshal([]byte(out), &sum); code != 0 || err != nil || sum.ArtifactID == "" || len(sum.DeploymentFiles) != 2 {
			t.Fatalf("ci = %d %+v %v\n%s", code, sum, err, msg)
		}
		if raw, err := os.ReadFile(filepath.Join(deploy, "compose.yml")); err != nil || !bytes.Contains(raw, []byte("services:")) {
			t.Fatalf("compose.yml = %s %v", raw, err)
		}
		if out := expect(0, "images", "--project", "app"); !strings.Contains(out, sum.ArtifactID) || !strings.Contains(out, "passed") {
			t.Fatalf("images = %s", out)
		}
		if out := expect(cli.ExitError, "download", sum.BuildID, "-o", work); !strings.Contains(out, "403") {
			t.Fatalf("developer download = %s", out)
		}
		expect(cli.ExitError, "publish", "--build", sum.BuildID, "--destination", ids.New())
	})

	t.Run("requests", func(t *testing.T) {
		approval := expect(0, "request", "repository", "--project", "app", "--repository", "team-a/web", "--justification", "release images")
		approval = strings.Fields(approval)[0]
		if out := expect(0, "requests", "--project", "app"); !strings.Contains(out, approval) || !strings.Contains(out, "pending") {
			t.Fatalf("requests = %s", out)
		}
		expect(0, "withdraw", approval)
		if out := expect(cli.ExitError, "withdraw", approval); !strings.Contains(out, "409") {
			t.Fatalf("second withdrawal = %s", out)
		}
	})

	t.Run("pipelines", func(t *testing.T) {
		expect(0, "pipelines", "--project", "app")
		expect(cli.ExitError, "pipeline", "run", ids.New(), "--ref", "main")
	})

	t.Run("api", func(t *testing.T) {
		if out := expect(0, "api", "getProject", "--path", "projectId="+projA); !strings.Contains(out, `"id": "`+projA+`"`) {
			t.Fatalf("api getProject = %s", out)
		}
		if out := expect(cli.ExitError, "api", "getProject", "--path", "projectId="+projB); !strings.Contains(out, "404") {
			t.Fatalf("api on another team's project = %s", out)
		}
		stdin = `{"yaml":"apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\nmetadata:\n  name: x\nspec:\n  build:\n    method: railpack\n"}`
		if out := expect(0, "api", "validateSpecDocument", "--body", "-"); !strings.Contains(out, `"valid": true`) {
			t.Fatalf("api validateSpecDocument = %s", out)
		}
		stdin = ""
		expect(cli.ExitUsage, "api", "getProject")
		expect(cli.ExitUsage, "api", "noSuchOperation")
	})

	t.Run("the token is checked by the server", func(t *testing.T) {
		envs["SAMUORI_TOKEN"] = "samuori_" + strings.Repeat("x", 40)
		if out := expect(cli.ExitError, "whoami"); !strings.Contains(out, "401") {
			t.Fatalf("bad token = %s", out)
		}
	})
}

// finishBuild records a run as a worker and its checks would: succeeded, with an image whose validation passed.
func finishBuild(t *testing.T, l *live, runID, project string) {
	blobs, err := storage.NewBlobs(filepath.Join(l.dataDir, "blobs"))
	if err != nil {
		t.Error(err)
		return
	}
	digest, size, err := blobs.Put(bytes.NewReader(ociArchiveBytes(t)), 1<<20)
	if err != nil {
		t.Error(err)
		return
	}
	var org string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT org_id FROM build_runs WHERE id = ?`), runID).Scan(&org)
	now := time.Now().UTC()
	attempt, artifact := ids.New(), ids.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at, ended_at, artifact_digest) VALUES (?, ?, 1, 1, ?, ?, 'completed', ?, 'destroyed', ?, ?, ?)`, []any{attempt, runID, ids.New(), ids.New(), now, now, now, digest}},
		{`UPDATE build_runs SET state = 'succeeded', current_attempt_id = ?, artifact_digest = ? WHERE id = ?`, []any{attempt, digest, runID}},
		{`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES (?, ?, ?, ?, 1, 'image', ?, ?, ?)`, []any{artifact, org, runID, attempt, digest, size, now}},
		{`INSERT INTO validation_runs (id, org_id, project_id, artifact_id, artifact_digest, build_run_id, trigger_kind, state, verdict, plan, policy_versions, created_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, 'build', 'completed', 'passed', '{}', '{}', ?, ?)`, []any{ids.New(), org, project, artifact, digest, runID, now, now}},
	} {
		if _, err := l.db.SQL.Exec(l.db.Rebind(q.sql), q.args...); err != nil {
			t.Error(err)
		}
	}
}
