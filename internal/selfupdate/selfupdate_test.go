package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
	"samuori.invalid/samuori/internal/updatebundle"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
	"samuori.invalid/samuori/internal/workers"
)

var (
	quiet = slog.New(slog.NewTextHandler(io.Discard, nil))
	admin = authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	op    = authz.Principal{ID: "op", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Operator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
)

type fakeWorkers struct {
	actions   []string
	sent      []string
	connected bool
}

func (f *fakeWorkers) ApplyAction(_ context.Context, _ authz.Principal, id, action, _ string, _ *time.Time, _ string) (workers.Worker, error) {
	f.actions = append(f.actions, action+":"+id)
	return workers.Worker{ID: id}, nil
}

func (f *fakeWorkers) Send(id string, m *workerv1.ControllerMessage) error {
	kind := "update"
	if m.GetUpdate().GetRollback() {
		kind = "rollback"
	}
	f.sent = append(f.sent, kind+":"+id+":"+m.GetUpdate().GetVersion())
	return nil
}

func (f *fakeWorkers) Connected(string) bool { return f.connected }

func (f *fakeWorkers) RequestWorkerSelfTest(_ context.Context, _ authz.Principal, id, _ string) (workers.SelfTest, error) {
	f.actions = append(f.actions, "self-test:"+id)
	return workers.SelfTest{WorkerID: id}, nil
}

type signer struct{ key, signers string }

func newSigner(t *testing.T, dir, name string) signer {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	key := filepath.Join(dir, name)
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "release@samuori.invalid", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	signers := filepath.Join(dir, name+".signers")
	_ = os.WriteFile(signers, append([]byte(`release@samuori.invalid namespaces="samuori-release" `), pub...), 0o644)
	return signer{key: key, signers: signers}
}

func (s signer) sign(sums []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "sign-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "SHA256SUMS")
	_ = os.WriteFile(f, sums, 0o600)
	if out, err := exec.Command("ssh-keygen", "-Y", "sign", "-q", "-n", "samuori-release", "-f", s.key, f).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	return os.ReadFile(f + ".sig")
}

func bundle(t *testing.T, sg signer, version string, minor uint32, arches ...string) *bytes.Reader {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{}
	for _, a := range arches {
		p := filepath.Join(dir, a)
		_ = os.WriteFile(p, releaseArchive(t, a, "abc1234\t2026-10-24\trelease "+version+"\n"), 0o600)
		files[a] = p
	}
	var buf bytes.Buffer
	if err := updatebundle.Write(&buf, updatebundle.Manifest{Version: version, WorkerProtocol: updatebundle.Protocol{Major: workers.ProtocolMajor, Minor: minor}}, files, sg.sign); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func releaseArchive(t *testing.T, arch, changes string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	name := "samuori-linux-" + arch + "/" + updatebundle.ChangesName
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(changes)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(changes))
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func setup(t *testing.T, controllerVersion string) (*Service, *db.DB, *fakeWorkers) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, dbtest.SQLite(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := cluster.Join(ctx, d, controllerVersion, quiet)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := storage.NewBlobs(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	fw := &fakeWorkers{connected: true}
	s := New(d, blobs, m, fw, "arm64", quiet)
	s.stageDir = filepath.Join(dir, "stage")
	if err := s.reportHost(ctx); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(q), args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	exec(`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`, now)
	return s, d, fw
}

func addPool(t *testing.T, d *db.DB, id, name, location, profile string) {
	t.Helper()
	if _, err := d.SQL.ExecContext(context.Background(), d.Rebind(`INSERT INTO worker_pools (id, org_id, name, location, isolation_profile, trust_domain, trusted_workloads_only, profile_approved, created_at) VALUES (?, 'o1', ?, ?, ?, 'default', 0, 1, ?)`),
		id, name, location, profile, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func addWorker(t *testing.T, d *db.DB, id, pool, name, state, ver string, minor int) {
	t.Helper()
	if _, err := d.SQL.ExecContext(context.Background(), d.Rebind(`INSERT INTO workers (id, org_id, pool_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, created_at, host_arch, update_free_bytes) VALUES (?, 'o1', ?, ?, 'enrolled', ?, ?, ?, '{}', ?, ?, ?, 'arm64', ?)`),
		id, pool, name, state, id, time.Now().Add(time.Hour), ver, minor, time.Now().UTC(), int64(1<<40)); err != nil {
		t.Fatal(err)
	}
}

func TestBundleUpload(t *testing.T) {
	t.Run("SEC-UPD-01", func(t *testing.T) {
		s, _, _ := setup(t, "0.2.0-dev")
		ctx := context.Background()
		dir := t.TempDir()
		good, other := newSigner(t, dir, "good"), newSigner(t, dir, "other")

		s.signers = filepath.Join(dir, "missing")
		if _, err := s.Upload(ctx, admin, bundle(t, good, "0.3.0-dev", workers.ProtocolMinor, "arm64"), ""); !errors.Is(err, ErrNoSigners) {
			t.Fatalf("an upload without release signers: %v", err)
		}
		s.signers = good.signers
		if _, err := s.Upload(ctx, op, bundle(t, good, "0.3.0-dev", workers.ProtocolMinor, "arm64"), ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("an operator uploaded a release: %v", err)
		}
		var inv *Invalid
		if _, err := s.Upload(ctx, admin, bundle(t, other, "0.3.0-dev", workers.ProtocolMinor, "arm64"), ""); !errors.As(err, &inv) || !strings.Contains(inv.Reason, "signature") {
			t.Errorf("a bundle signed by an unknown key was kept: %v", err)
		}
		if _, err := s.Upload(ctx, admin, bytes.NewReader([]byte("not a bundle")), ""); !errors.As(err, &inv) {
			t.Errorf("a file that is no bundle was kept: %v", err)
		}
		r, err := s.Upload(ctx, admin, bundle(t, good, "0.3.0-dev", workers.ProtocolMinor, "arm64", "amd64"), "")
		if err != nil || r.Version != "0.3.0-dev" || len(r.Architectures) != 2 {
			t.Fatalf("upload = %+v, %v", r, err)
		}
		if _, err := s.Upload(ctx, admin, bundle(t, good, "0.3.0-dev", workers.ProtocolMinor, "arm64"), ""); !errors.Is(err, ErrExists) {
			t.Errorf("the same release was kept twice: %v", err)
		}
		list, err := s.Releases(ctx, admin)
		if err != nil || len(list) != 1 || !list[0].Bundled || len(list[0].Architectures) != 2 {
			t.Fatalf("releases = %+v, %v", list, err)
		}
		if c := list[0].Changes; len(c) != 1 || c[0].Commit != "abc1234" || c[0].Summary != "release 0.3.0-dev" {
			t.Errorf("changes = %+v", c)
		}
	})
}

func TestPreflight(t *testing.T) {
	t.Run("SEC-UPD-04", func(t *testing.T) {
		s, d, _ := setup(t, "0.2.0-dev")
		ctx := context.Background()
		dir := t.TempDir()
		good, other := newSigner(t, dir, "good"), newSigner(t, dir, "other")
		s.signers = good.signers
		addPool(t, d, "p1", "pool", "here", "rootless-dedicated-vm")
		addWorker(t, d, "w1", "p1", "build-1", "active", "0.2.0-dev", 3)
		state := func(p Preflight, id string) string {
			for _, c := range p.Checks {
				if c.ID == id {
					return c.State
				}
			}
			return "missing"
		}
		for _, v := range []struct {
			version string
			arches  []string
		}{{"0.3.0-dev", []string{"arm64", "amd64"}}, {"0.1.0-dev", []string{"arm64"}}, {"0.3.1-dev", []string{"amd64"}}} {
			if _, err := s.Upload(ctx, admin, bundle(t, good, v.version, workers.ProtocolMinor, v.arches...), ""); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Preflight(ctx, op, "0.3.0-dev", false, 0); !errors.Is(err, ErrForbidden) {
			t.Errorf("an operator ran the checks: %v", err)
		}
		p, err := s.Preflight(ctx, admin, "0.3.0-dev", false, 0)
		if err != nil || !p.Ready || len(p.Steps) != 2 {
			t.Fatalf("a valid upgrade = %+v, %v", p, err)
		}
		if state(p, "disk") != CheckWarning {
			t.Errorf("a controller that reports no free space passed the disk check: %s", state(p, "disk"))
		}

		if p, _ = s.Preflight(ctx, admin, "0.1.0-dev", false, 0); p.Ready || state(p, "version") != CheckFailed {
			t.Errorf("a downgrade passed the checks: %+v", p.Checks)
		}
		if p, _ = s.Preflight(ctx, admin, "0.1.0-dev", true, 0); state(p, "version") != CheckWarning {
			t.Errorf("an allowed downgrade = %s", state(p, "version"))
		}
		if p, _ = s.Preflight(ctx, admin, "0.3.1-dev", false, 0); p.Ready || state(p, "architectures") != CheckFailed {
			t.Errorf("a bundle without the hosts' architecture passed: %+v", p.Checks)
		}
		if _, err := s.Start(ctx, admin, StartRequest{Version: "0.3.1-dev", Reason: "missing arch"}, ""); err == nil {
			t.Error("a rollout started although its checks failed")
		}

		_, _ = d.SQL.ExecContext(ctx, `UPDATE workers SET update_free_bytes = 10`)
		if p, _ = s.Preflight(ctx, admin, "0.3.0-dev", false, 0); state(p, "disk") != CheckFailed {
			t.Errorf("a worker short of disk passed: %s", state(p, "disk"))
		}
		_, _ = d.SQL.ExecContext(ctx, `UPDATE workers SET update_free_bytes = NULL`)

		s.workers.(*fakeWorkers).connected = false
		if p, _ = s.Preflight(ctx, admin, "0.3.0-dev", false, 0); state(p, "reachability") != CheckFailed {
			t.Errorf("an unreachable worker passed: %s", state(p, "reachability"))
		}
		s.workers.(*fakeWorkers).connected = true

		s.signers = other.signers
		if p, _ = s.Preflight(ctx, admin, "0.3.0-dev", false, 0); p.Ready || state(p, "signature") != CheckFailed {
			t.Errorf("a bundle no longer signed by an allowed key passed: %s", state(p, "signature"))
		}
	})
}

func TestWaveSize(t *testing.T) {
	for _, c := range []struct{ n, pct, want int }{{1, 25, 1}, {3, 25, 1}, {4, 25, 1}, {8, 25, 2}, {10, 25, 2}, {100, 25, 25}, {5, 100, 5}} {
		if got := waveSize(c.n, c.pct); got != c.want {
			t.Errorf("waveSize(%d, %d) = %d; want %d", c.n, c.pct, got, c.want)
		}
	}
}

func TestPlanSteps(t *testing.T) {
	controllers := []host{
		{kind: "controller", name: "c1", version: "1", leader: true},
		{kind: "controller", name: "c2", version: "1"},
		{kind: "controller", name: "c3", version: "2"},
		{kind: "controller", name: "c4", version: "1"},
	}
	var ws []host
	for i := range 8 {
		loc := "east"
		if i%2 == 1 {
			loc = "west"
		}
		ws = append(ws, host{kind: "worker", id: fmt.Sprintf("a%d", i), name: fmt.Sprintf("a%d", i), version: "1", poolID: "pa", pool: "a", location: loc})
	}
	ws = append(ws, host{kind: "worker", id: "b0", name: "b0", version: "1", poolID: "pb", pool: "b", location: "east"})
	steps := planSteps(controllers, ws, "2", 25, time.Now())
	var order []string
	for _, st := range steps {
		if st.Kind == "controller" {
			order = append(order, st.Name)
		}
	}
	if strings.Join(order, ",") != "c2,c4,c1" {
		t.Fatalf("controller order = %v; want one at a time, the up-to-date one left out, the leader last", order)
	}
	waves := map[string]map[int][]string{}
	for _, st := range steps {
		if st.Kind == "worker" {
			if waves[st.Pool] == nil {
				waves[st.Pool] = map[int][]string{}
			}
			waves[st.Pool][st.Wave] = append(waves[st.Pool][st.Wave], st.Location)
		}
	}
	if len(waves["a"]) != 4 || len(waves["b"]) != 1 {
		t.Fatalf("waves = %v; want 4 waves of 2 in pool a and 1 in pool b", waves)
	}
	for w, locs := range waves["a"] {
		if len(locs) != 2 || locs[0] == locs[1] {
			t.Errorf("wave %d of pool a = %v; want 2 workers spread across locations", w, locs)
		}
	}
	if _, ok := waves["b"][4]; !ok {
		t.Errorf("pool b's first wave is not the same wave as pool a's: %v", waves)
	}
}

func TestStartableLeaderLastPoolsParallel(t *testing.T) {
	s := &Service{member: &cluster.Member{Hostname: "leader"}}
	r := Rollout{Steps: []Step{
		{Kind: "controller", Target: "leader", State: "pending", Wave: 1},
		{Kind: "controller", Target: "c2", State: "pending", Wave: 2},
		{Kind: "worker", PoolID: "a", State: "pending", Wave: 3},
		{Kind: "worker", PoolID: "a", State: "pending", Wave: 4},
		{Kind: "worker", PoolID: "b", State: "pending", Wave: 3},
	}}
	if got := s.startable(r); len(got) != 1 || got[0] != 1 {
		t.Fatalf("first = %v; want c2 before the leader", got)
	}
	r.Steps[1].State = "applying"
	if got := s.startable(r); len(got) != 0 {
		t.Fatalf("a second controller started while one is updating: %v", got)
	}
	r.Steps[1].State = "succeeded"
	if got := s.startable(r); len(got) != 1 || got[0] != 0 {
		t.Fatalf("after c2 = %v; want the leader", got)
	}
	r.Steps[0].State = "succeeded"
	if got := s.startable(r); len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("workers = %v; want the first wave of both pools together", got)
	}
	r.Steps[4].State = "succeeded"
	r.Steps[2].State = "testing"
	if got := s.startable(r); len(got) != 0 {
		t.Fatalf("pool a's second wave started before its first finished: %v", got)
	}
}

func TestRolloutWavesAndRollback(t *testing.T) {
	t.Run("SEC-UPD-02", func(t *testing.T) {
		s, d, fw := setup(t, "0.2.0-dev")
		ctx := context.Background()
		good := newSigner(t, t.TempDir(), "good")
		s.signers = good.signers
		addPool(t, d, "p1", "a", "here", "rootless-dedicated-vm")
		addPool(t, d, "p2", "b", "here", "rootless-dedicated-vm")
		addPool(t, d, "p3", "kvm", "here", "vm-per-job")
		addWorker(t, d, "w1", "p1", "a-1", "active", "0.2.0-dev", 3)
		addWorker(t, d, "w2", "p1", "a-2", "active", "0.2.0-dev", 3)
		addWorker(t, d, "w3", "p2", "b-1", "active", "0.2.0-dev", 3)
		addWorker(t, d, "w4", "p2", "b-2", "cordoned", "0.2.0-dev", 3)
		addWorker(t, d, "w5", "p3", "kvm-1", "active", "0.2.0-dev", 3)
		addWorker(t, d, "w6", "p1", "local", "active", "0.2.0-dev", 3)
		if _, err := s.Upload(ctx, admin, bundle(t, good, "0.3.0-dev", workers.ProtocolMinor, "arm64"), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Start(ctx, op, StartRequest{Version: "0.3.0-dev", Reason: "rotate"}, ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("an operator started a rollout: %v", err)
		}
		r, err := s.Start(ctx, admin, StartRequest{Version: "0.3.0-dev", PauseAfterFirstWave: true}, "")
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for _, st := range r.Steps {
			plan = append(plan, fmt.Sprintf("%s:%d", st.Name, st.Wave))
		}
		if got := strings.Join(plan, ","); got != s.member.Hostname+":1,a-1:2,b-1:2,a-2:3" {
			t.Fatalf("plan = %s", got)
		}
		if !strings.Contains(r.Detail, "kvm-1 (pool kvm cannot run self-tests)") || !strings.Contains(r.Detail, "b-2 (cordoned)") {
			t.Errorf("left-out workers not named: %q", r.Detail)
		}
		if _, err := s.Start(ctx, admin, StartRequest{Version: "0.3.0-dev", Reason: "again"}, ""); err == nil {
			t.Error("a second rollout started")
		}
		for _, id := range []string{"p", "r"} {
			ctrl := map[string]func(context.Context, authz.Principal, string, string) (Rollout, error){"p": s.Pause, "r": s.Resume}[id]
			if _, err := ctrl(ctx, op, r.ID, ""); !errors.Is(err, ErrForbidden) {
				t.Errorf("an operator controlled a rollout: %v", err)
			}
		}
		if _, err := s.Cancel(ctx, op, r.ID, ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("an operator stopped a rollout: %v", err)
		}
		if _, err := s.List(ctx, op, "", 10); !errors.Is(err, ErrForbidden) {
			t.Errorf("an operator listed rollouts: %v", err)
		}

		step := func(seq int) Step {
			r, _ := s.Get(ctx, admin, r.ID)
			return r.Steps[seq-1]
		}
		advance := func() {
			t.Helper()
			if err := s.advance(ctx); err != nil {
				t.Fatal(err)
			}
		}
		_, _ = d.SQL.ExecContext(ctx, `UPDATE rollout_steps SET state = 'succeeded' WHERE seq = 1`)
		advance()
		if step(2).State != "draining" || step(3).State != "draining" || step(4).State != "pending" {
			t.Fatalf("the first wave of both pools did not start together: %s %s %s", step(2).State, step(3).State, step(4).State)
		}
		advance()
		if len(fw.sent) != 2 {
			t.Fatalf("drained workers were not asked to update: %v", fw.sent)
		}
		_, _ = d.SQL.ExecContext(ctx, `UPDATE workers SET version = '0.3.0-dev' WHERE id IN ('w1', 'w3')`)
		advance()
		if step(2).State != "testing" || step(3).State != "testing" {
			t.Fatalf("updated workers were not self-tested: %s %s", step(2).State, step(3).State)
		}
		now := time.Now().UTC()
		_, _ = d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO pool_self_tests (id, org_id, pool_id, worker_id, status, failed_checks, started_at) VALUES ('t1', 'o1', 'p1', 'w1', 'failed', '[]', ?), ('t3', 'o1', 'p2', 'w3', 'passed', '[]', ?)`), now, now)
		advance()
		if step(2).State != "rolling_back" || step(3).State != "succeeded" || fw.sent[len(fw.sent)-1] != "rollback:w1:0.3.0-dev" {
			t.Fatalf("a worker whose self-test failed was not rolled back: %s %s %v", step(2).State, step(3).State, fw.sent)
		}
		_, _ = d.SQL.ExecContext(ctx, `UPDATE workers SET version = '0.2.0-dev' WHERE id = 'w1'`)
		advance()
		if st := step(2); st.State != "failed" || !strings.Contains(st.Detail, "rolled back to 0.2.0-dev") {
			t.Fatalf("after the rollback = %s %q", st.State, st.Detail)
		}
		advance()
		got, _ := s.Get(ctx, admin, r.ID)
		if !got.Paused || got.State != "running" || step(4).State != "pending" {
			t.Fatalf("a failed host did not pause the rollout: paused %v, %s, next %s", got.Paused, got.State, step(4).State)
		}
		advance()
		if step(4).State != "pending" {
			t.Fatal("a paused rollout started another host")
		}
		if got, err = s.Resume(ctx, admin, r.ID, ""); err != nil || got.Paused || got.Steps[1].State != "pending" {
			t.Fatalf("resume = %v, paused %v, failed host %s; want it retried", err, got.Paused, got.Steps[1].State)
		}
		hist, err := s.List(ctx, admin, "", 10)
		if err != nil || len(hist) != 1 {
			t.Fatalf("history = %v, %v", hist, err)
		}
		if got, err = s.Cancel(ctx, admin, r.ID, ""); err != nil || got.State != "cancelled" {
			t.Fatalf("stop = %v, %s", err, got.State)
		}
	})
}

func TestRolloutPausesAfterFirstWave(t *testing.T) {
	t.Run("SEC-UPD-03", func(t *testing.T) {
		s, d, fw := setup(t, "0.2.0-dev")
		ctx := context.Background()
		good := newSigner(t, t.TempDir(), "good")
		s.signers = good.signers
		addPool(t, d, "p1", "pool", "here", "rootless-dedicated-vm")
		addWorker(t, d, "w1", "p1", "build-1", "active", "0.2.0-dev", 3)
		addWorker(t, d, "w2", "p1", "build-2", "active", "0.2.0-dev", 3)
		if _, err := s.Upload(ctx, admin, bundle(t, good, "0.3.0-dev", workers.ProtocolMinor, "arm64"), ""); err != nil {
			t.Fatal(err)
		}
		r, err := s.Start(ctx, admin, StartRequest{Version: "0.3.0-dev", Reason: "try one wave first", PauseAfterFirstWave: true}, "")
		if err != nil {
			t.Fatal(err)
		}
		if r.PauseAfter == nil || *r.PauseAfter != 2 {
			t.Fatalf("pause after = %v; want after the first worker wave (2)", r.PauseAfter)
		}
		_, _ = d.SQL.ExecContext(ctx, `UPDATE rollout_steps SET state = 'succeeded' WHERE seq IN (1, 2)`)
		if err := s.advance(ctx); err != nil {
			t.Fatal(err)
		}
		if r, _ = s.Get(ctx, admin, r.ID); !r.Paused || r.Steps[2].State != "pending" || len(fw.actions) != 0 {
			t.Fatalf("the rollout did not wait after the first wave: paused %v, %s, %v", r.Paused, r.Steps[2].State, fw.actions)
		}
		if r, err = s.Resume(ctx, admin, r.ID, ""); err != nil || r.Paused {
			t.Fatalf("resume = %v, paused %v", err, r.Paused)
		}
		if err := s.advance(ctx); err != nil {
			t.Fatal(err)
		}
		if r, _ = s.Get(ctx, admin, r.ID); r.Steps[2].State != "draining" {
			t.Fatalf("after Resume the second worker = %s", r.Steps[2].State)
		}
		var n int
		_ = d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action = 'rollout.resume'`).Scan(&n)
		if n != 1 {
			t.Errorf("resume audited %d times", n)
		}
	})
}

func TestPurgeHistory(t *testing.T) {
	s, d, _ := setup(t, "0.2.0-dev")
	ctx := context.Background()
	good := newSigner(t, t.TempDir(), "good")
	s.signers = good.signers
	addPool(t, d, "p1", "a", "here", "rootless-dedicated-vm")
	addWorker(t, d, "w1", "p1", "a-1", "active", "0.2.0-dev", 3)
	if _, err := s.Upload(ctx, admin, bundle(t, good, "0.3.0-dev", workers.ProtocolMinor, "arm64"), ""); err != nil {
		t.Fatal(err)
	}
	r, err := s.Start(ctx, admin, StartRequest{Version: "0.3.0-dev"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeHistory(ctx, op, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("an operator purged the history: %v", err)
	}
	if n, err := s.PurgeHistory(ctx, admin, ""); err != nil || n != 0 {
		t.Fatalf("purge with only a running rollout = %d, %v", n, err)
	}
	if list, _ := s.List(ctx, admin, "", 20); len(list) != 1 {
		t.Fatalf("the running rollout was purged: %d left", len(list))
	}
	if _, err := s.Cancel(ctx, admin, r.ID, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeHistory(ctx, admin, ""); err != nil || n != 1 {
		t.Fatalf("purge after cancelling = %d, %v", n, err)
	}
	var steps int
	if err := d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM rollout_steps`).Scan(&steps); err != nil || steps != 0 {
		t.Errorf("steps left behind: %d, %v", steps, err)
	}
	if list, _ := s.List(ctx, admin, "", 20); len(list) != 0 {
		t.Errorf("history after purge = %d rollouts", len(list))
	}
}

func TestPlanAllAtOnce(t *testing.T) {
	controllers := []host{
		{kind: "controller", name: "c1", version: "1", leader: true, reachable: true},
		{kind: "controller", name: "c2", version: "1", reachable: true},
		{kind: "controller", name: "c3", version: "1", reachable: true},
		{kind: "controller", name: "c4", version: "1"},
		{kind: "controller", name: "c5", version: "2", reachable: true},
	}
	ws := []host{
		{kind: "worker", id: "a0", name: "a0", version: "1", poolID: "pa", pool: "a"},
		{kind: "worker", id: "a1", name: "a1", version: "1", poolID: "pa", pool: "a"},
		{kind: "worker", id: "b0", name: "b0", version: "1", poolID: "pb", pool: "b"},
	}
	picked := map[string]bool{}
	for _, v := range []string{"2", "3", "4", "5", "6", "7", "8", "9"} {
		steps := planSteps(controllers, ws, v, 100, time.Now())
		var first, second []string
		for _, st := range steps {
			if st.Wave == 1 {
				first = append(first, st.Name)
			} else {
				second = append(second, st.Name)
			}
		}
		if len(first) != 1 || first[0] == "c1" || first[0] == "c4" || (v == "2" && first[0] == "c5") {
			t.Fatalf("version %s: first wave = %v; want one reachable, out-of-date secondary", v, first)
		}
		if again := planSteps(controllers, ws, v, 100, time.Now()); again[0].Name != first[0] {
			t.Fatalf("version %s: the checks and the rollout would name different first controllers", v)
		}
		picked[first[0]] = true
		want := 7
		if v == "2" {
			want = 6
		}
		if len(second) != want {
			t.Fatalf("version %s: second wave = %v; want every other stale controller and every worker", v, second)
		}
	}
	if len(picked) < 2 {
		t.Errorf("the first controller is always the same: %v", picked)
	}
	single := planSteps(controllers[:1], ws, "2", 100, time.Now())
	if single[0].Name != "c1" || single[0].Wave != 1 || single[1].Wave != 2 || single[len(single)-1].Wave != 2 {
		t.Errorf("single controller plan = %+v; want it alone first, then every worker", single)
	}
}

func TestNextWave(t *testing.T) {
	r := Rollout{WavePercent: 100, Steps: []Step{
		{Kind: "controller", Target: "c2", State: "pending", Wave: 1},
		{Kind: "controller", Target: "c1", State: "pending", Wave: 2},
		{Kind: "worker", PoolID: "a", State: "pending", Wave: 2},
		{Kind: "worker", PoolID: "b", State: "pending", Wave: 2},
	}}
	if got := nextWave(r); len(got) != 1 || got[0] != 0 {
		t.Fatalf("start = %v; want the first controller alone", got)
	}
	r.Steps[0].State = "applying"
	if got := nextWave(r); len(got) != 0 {
		t.Fatalf("while the first controller updates = %v", got)
	}
	r.Steps[0].State = "succeeded"
	one := 1
	r.PauseAfter = &one
	if got := nextWave(r); len(got) != 0 {
		t.Fatalf("paused after the first controller = %v", got)
	}
	r.PauseAfter = nil
	if got := nextWave(r); len(got) != 3 {
		t.Fatalf("second wave = %v; want the other controller and every worker at once", got)
	}
}
