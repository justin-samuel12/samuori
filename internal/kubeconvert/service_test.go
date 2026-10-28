package kubeconvert

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/convertjob"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/operations"
	"samuori.invalid/samuori/internal/pki"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/tenancy"
	"samuori.invalid/samuori/internal/version"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
	"samuori.invalid/samuori/internal/workers"
)

type fakeChannel struct {
	mu   sync.Mutex
	sent []*workerv1.ControllerMessage
}

func (*fakeChannel) Connected(string) bool { return true }
func (c *fakeChannel) Send(_ string, m *workerv1.ControllerMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

type env struct {
	ctx                context.Context
	d                  *db.DB
	svc                *Service
	sched              *Scheduler
	ch                 *fakeChannel
	admin, dev, other  authz.Principal
	org, project, snap string
	workerID, otherWkr string
	approved           string
}

// ConversionFixture builds a real database, upload, provider approval and an untrusted-eligible worker.
func newEnv(t *testing.T) *env {
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
	e := &env{ctx: ctx, d: d, org: ids.New(), project: ids.New(), workerID: ids.New(), otherWkr: ids.New(), approved: "sha256:" + strings.Repeat("ab", 32)}
	teamA, teamB, otherProject, pool := ids.New(), ids.New(), ids.New(), ids.New()
	e.admin = authz.Principal{ID: ids.New(), OrgID: e.org, Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: e.org}}}
	e.dev = authz.Principal{ID: ids.New(), OrgID: e.org, Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Developer, ScopeType: authz.ScopeTeam, ScopeID: teamA}}}
	e.other = authz.Principal{ID: ids.New(), OrgID: e.org, Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Developer, ScopeType: authz.ScopeTeam, ScopeID: teamB}}}
	now := time.Now().UTC()
	caps, _ := json.Marshal(workers.Capabilities{OS: "linux", Architectures: []string{"arm64"}, IsolationProfiles: []string{"rootless-dedicated-vm"}, DaemonSlots: 1, StrictEgress: true,
		ConfigDigest: "sha256:cfg", Providers: map[string]string{"kompose": "v1.38.0"}})
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id, name, created_at) VALUES (?, 'o', ?)`, []any{e.org, now}},
		{`INSERT INTO teams (id, org_id, name, created_at) VALUES (?, ?, 'a', ?), (?, ?, 'b', ?)`, []any{teamA, e.org, now, teamB, e.org, now}},
		{`INSERT INTO projects (id, org_id, team_id, name, publication_mode, created_at, updated_at) VALUES (?, ?, ?, 'shop', 'manual', ?, ?), (?, ?, ?, 'other', 'manual', ?, ?)`,
			[]any{e.project, e.org, teamA, now, now, otherProject, e.org, teamB, now, now}},
		{`INSERT INTO worker_pools (id, org_id, name, location, isolation_profile, trust_domain, trusted_workloads_only, profile_approved, created_at) VALUES (?, ?, 'p', 'default', 'rootless-dedicated-vm', 'd', 0, 1, ?)`, []any{pool, e.org, now}},
		{`INSERT INTO workers (id, org_id, pool_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, created_at) VALUES (?, ?, ?, 'w1', 'enrolled', 'active', 's1', ?, ?, ?, 4, ?), (?, ?, ?, 'w2', 'enrolled', 'active', 's2', ?, ?, ?, 4, ?)`,
			[]any{e.workerID, e.org, pool, now.Add(time.Hour), string(caps), version.Version, now, e.otherWkr, e.org, pool, now.Add(time.Hour), "{}", version.Version, now}},
		{`INSERT INTO pool_self_tests (id, org_id, pool_id, worker_id, status, failed_checks, started_at, finished_at, profile, config_digest) VALUES (?, ?, ?, ?, 'passed', '[]', ?, ?, 'rootless-dedicated-vm', 'sha256:cfg')`,
			[]any{ids.New(), e.org, pool, e.workerID, now, now}},
	} {
		if _, err := d.SQL.Exec(q.sql, q.args...); err != nil {
			t.Fatal(q.sql, err)
		}
	}
	blobs, err := storage.NewBlobs(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	tn := tenancy.NewStore(d)
	ops := operations.NewStore(d)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	src, err := sources.NewService(d, blobs, ops, tn, log, filepath.Join(dir, "staging"), sources.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "shop.tar")
	writeTar(t, archive, "../../examples/compose-shop")
	snap, err := src.Ingest(ctx, e.dev, WriteRoles, e.project, archive, "")
	if err != nil {
		t.Fatal(err)
	}
	e.snap = snap.ID
	prov := providers.NewStore(d)
	if _, err := prov.Set(ctx, e.admin, "kompose", providers.Config{Enabled: true, Version: "v1.38.0", Artifacts: map[string]string{"binary-arm64": e.approved, "binary-amd64": e.approved}}, ""); err != nil {
		t.Fatal(err)
	}
	ca, err := pki.Open(filepath.Join(dir, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	e.svc = NewService(d, tn, src, blobs, prov, ops)
	e.ch = &fakeChannel{}
	e.sched = NewScheduler(e.svc, workers.NewStore(d, ca), e.ch, log)
	return e
}

func writeTar(t *testing.T, dst, dir string) {
	t.Helper()
	f, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

func (e *env) create(t *testing.T) Conversion {
	t.Helper()
	c, err := e.svc.Create(e.ctx, e.dev, e.project, CreateRequest{Name: "shop", Provider: "kompose", Input: Input{Kind: InputCompose, SnapshotID: e.snap, ComposePath: "compose.yml"},
		Options: Options{Namespace: "shop"}}, ids.New(), "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// lease dispatches and returns the assignment the worker received.
func (e *env) lease(t *testing.T) *workerv1.ConversionAssignment {
	t.Helper()
	n, err := e.sched.Dispatch(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("dispatched %d: %v", n, err)
	}
	var runID string
	var gen int64
	if err := e.d.SQL.QueryRow(`SELECT id, generation FROM kube_conversion_runs WHERE state = 'assigned'`).Scan(&runID, &gen); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(wakePayload{RunID: runID, Generation: gen})
	if err := e.sched.Deliver(e.ctx, SubjectAssign, raw); err != nil {
		t.Fatal(err)
	}
	e.ch.mu.Lock()
	defer e.ch.mu.Unlock()
	as := e.ch.sent[len(e.ch.sent)-1].GetConversion()
	if as == nil {
		t.Fatal("no conversion assignment sent")
	}
	return as
}

func goldenReport(t *testing.T, lease *workerv1.LeaseRef, digest string) *workerv1.ConversionReport {
	g := loadGolden(t, "manifests")
	rep := &workerv1.ConversionReport{Lease: lease, State: workerv1.ConversionState_CONVERSION_STATE_COMPLETED, ToolStderr: []byte(g.Stderr), ToolDigest: digest,
		Cleanup: workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED}
	for name, c := range g.Files {
		rep.Files = append(rep.Files, &workerv1.ConversionFile{Path: "manifests/" + name, Content: []byte(c)})
	}
	return rep
}

// TestConversionLifecycle (SEC-K8S-01, SEC-K8S-06): a run goes only to a worker that reports the approved provider
// and passes untrusted-input eligibility, every call is fenced by its lease, the result becomes revision 1, and an
// edit becomes revision 2 that is validated again.
func TestConversionLifecycle(t *testing.T) {
	e := newEnv(t)
	c := e.create(t)
	if c.State != StateQueued || c.LatestRun == nil || c.LatestRun.ProviderVersion != "v1.38.0" {
		t.Fatalf("created %+v", c)
	}
	as := e.lease(t)
	var leasedTo string
	_ = e.d.SQL.QueryRow(`SELECT lease_worker_id FROM kube_conversion_runs`).Scan(&leasedTo)
	if leasedTo != e.workerID {
		t.Fatalf("leased to %s, want the eligible worker %s", leasedTo, e.workerID)
	}
	if _, _, err := e.sched.Job(e.ctx, e.otherWkr, as.GetLease()); !errors.Is(err, workers.ErrStaleLease) {
		t.Errorf("another worker read the job: %v", err)
	}
	digest, raw, err := e.sched.Job(e.ctx, e.workerID, as.GetLease())
	if err != nil || digest != as.GetJobDigest() || convertjob.Digest(raw) != digest {
		t.Fatalf("job digest %s, assignment %s: %v", digest, as.GetJobDigest(), err)
	}
	var job convertjob.Job
	_ = json.Unmarshal(raw, &job)
	if strings.Contains(string(raw), "password") || job.ToolSHA256["arm64"] != e.approved || !strings.Contains(strings.Join(job.Args, " "), "--build none") {
		t.Errorf("job = %s", raw)
	}
	var manifest []byte
	if err := e.sched.Input(e.ctx, e.workerID, as.GetLease(), func(ch *workerv1.SourceChunk) error {
		if m := ch.GetManifestJson(); m != nil {
			manifest = m
		}
		return nil
	}); err != nil || convertjob.Digest(manifest) != job.InputDigest {
		t.Fatalf("input manifest does not match the job: %v", err)
	}

	if err := e.sched.Report(e.ctx, e.workerID, goldenReport(t, as.GetLease(), "sha256:"+strings.Repeat("cd", 32))); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.Get(e.ctx, e.dev, c.ID)
	if got.State != StateFailed || !strings.Contains(got.LatestRun.Detail, "not the approved one") {
		t.Fatalf("a result from an unapproved tool was accepted: %+v", got.LatestRun)
	}

	if _, err := e.svc.Rerun(e.ctx, e.dev, c.ID, nil, nil, ids.New(), ""); err != nil {
		t.Fatal(err)
	}
	as = e.lease(t)
	stale := &workerv1.LeaseRef{AttemptId: as.GetLease().GetAttemptId(), FencingGeneration: as.GetLease().GetFencingGeneration() - 1}
	if err := e.sched.Report(e.ctx, e.workerID, goldenReport(t, stale, e.approved)); !errors.Is(err, workers.ErrStaleLease) {
		t.Errorf("a stale generation reported: %v", err)
	}
	if err := e.sched.Report(e.ctx, e.workerID, goldenReport(t, as.GetLease(), e.approved)); err != nil {
		t.Fatal(err)
	}
	got, _ = e.svc.Get(e.ctx, e.dev, c.ID)
	if got.State != StateSucceeded || got.CurrentRevision != 1 {
		t.Fatalf("after report: %+v %+v", got, got.LatestRun)
	}
	_, rev, err := e.svc.Revision(e.ctx, e.dev, c.ID, 0)
	if err != nil || rev.Errors == 0 || rev.Lineage.ToolDigest != e.approved || rev.Lineage.SnapshotDigest == "" || rev.Lineage.WorkerID != e.workerID {
		t.Fatalf("revision 1: errors %d lineage %+v: %v", rev.Errors, rev.Lineage, err)
	}
	for _, f := range rev.Files {
		if strings.Contains(f.Content, examplePassword) {
			t.Errorf("%s holds the literal password", f.Path)
		}
	}

	edited := strings.Replace(content(rev.Files, "manifests/web-deployment.yml"), "replicas: 1", "replicas: 1\n  replica: 2", 1)
	rev2, err := e.svc.Edit(e.ctx, e.dev, c.ID, 1, "manifests/web-deployment.yml", edited, "")
	if err != nil {
		t.Fatal(err)
	}
	if rev2.Number != 2 || rev2.Parent != 1 || rev2.Origin != OriginEdit || !has(rev2.Findings, SeverityError, "schema", "") || !has(rev2.Findings, SeverityReview, "host-volume-not-carried", "") {
		t.Errorf("revision 2: %+v", rev2)
	}
	withSecret := content(rev.Files, "manifests/web-deployment.yml") + "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: x\nstringData:\n  token: hunter2\n"
	rev3, err := e.svc.Edit(e.ctx, e.dev, c.ID, 1, "manifests/web-deployment.yml", withSecret, "")
	if err != nil || strings.Contains(content(rev3.Files, "manifests/web-deployment.yml"), "hunter2") {
		t.Errorf("an edit stored a literal secret: %v", err)
	}
	var audited int
	_ = e.d.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('kube-conversion.create', 'kube-conversion.rerun', 'kube-conversion.edit')`).Scan(&audited)
	if audited != 4 {
		t.Errorf("audited %d conversion actions, want 4", audited)
	}
}

// TestConversionAuthorization (SEC-K8S-06): another team cannot read, download, edit or re-run a conversion, and a
// provider that is not enabled cannot be used.
func TestConversionAuthorization(t *testing.T) {
	e := newEnv(t)
	c := e.create(t)
	if _, err := e.svc.Get(e.ctx, e.other, c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("other team get: %v", err)
	}
	if _, _, err := e.svc.Download(e.ctx, e.other, c.ID, 0, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("other team download: %v", err)
	}
	if _, err := e.svc.Edit(e.ctx, e.other, c.ID, 0, "manifests/x.yml", "kind: ConfigMap", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("other team edit: %v", err)
	}
	if _, err := e.svc.List(e.ctx, e.other, e.project, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("other team list: %v", err)
	}
	if _, err := e.svc.Create(e.ctx, e.other, e.project, CreateRequest{Name: "x", Provider: "kompose", Input: Input{Kind: InputCompose, SnapshotID: e.snap, ComposePath: "compose.yml"}}, ids.New(), ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("other team create: %v", err)
	}
	reader := authz.Principal{ID: ids.New(), OrgID: e.org, Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Publisher, ScopeType: authz.ScopeProject, ScopeID: e.project}}}
	if _, err := e.svc.Get(e.ctx, reader, c.ID); err != nil {
		t.Errorf("a publisher on the project cannot read: %v", err)
	}
	if _, err := e.svc.Rerun(e.ctx, reader, c.ID, nil, nil, ids.New(), ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("a publisher re-ran a conversion: %v", err)
	}
	if _, err := e.svc.Create(e.ctx, e.dev, e.project, CreateRequest{Name: "y", Provider: "move2kube", Input: Input{Kind: InputSource, SnapshotID: e.snap}}, ids.New(), ""); err == nil {
		t.Error("a provider that is not enabled was used")
	}
}
