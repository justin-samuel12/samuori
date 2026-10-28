package kubeconvert

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/convertjob"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/providers"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
	"samuori.invalid/samuori/internal/workers"
)

func (e *env) enableMove2Kube(t *testing.T) {
	t.Helper()
	if _, err := e.svc.providers.Set(e.ctx, e.admin, "move2kube", providers.Config{Enabled: true, Version: "v0.3.15", Artifacts: map[string]string{"binary-arm64": e.approved, "binary-amd64": e.approved}}, ""); err != nil {
		t.Fatal(err)
	}
	caps, _ := json.Marshal(workers.Capabilities{OS: "linux", Architectures: []string{"arm64"}, IsolationProfiles: []string{"rootless-dedicated-vm"}, DaemonSlots: 1, StrictEgress: true,
		ConfigDigest: "sha256:cfg", Providers: map[string]string{"kompose": "v1.38.0", "move2kube": "v0.3.15"}})
	if _, err := e.d.SQL.Exec(`UPDATE workers SET capabilities = ? WHERE id = ?`, string(caps), e.workerID); err != nil {
		t.Fatal(err)
	}
}

func m2kReport(lease *workerv1.LeaseRef, digest string, g m2kGolden) *workerv1.ConversionReport {
	rep := &workerv1.ConversionReport{Lease: lease, State: workerv1.ConversionState_CONVERSION_STATE_COMPLETED, ToolStderr: []byte(g.Stderr), ToolDigest: digest,
		Cleanup: workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED}
	for name, c := range g.Files {
		rep.Files = append(rep.Files, &workerv1.ConversionFile{Path: m2kOut + "/" + name, Content: []byte(c)})
	}
	return rep
}

// job reads the job of the run currently leased, as the worker would.
func (e *env) job(t *testing.T, as *workerv1.ConversionAssignment) convertjob.Job {
	t.Helper()
	_, raw, err := e.sched.Job(e.ctx, e.workerID, as.GetLease())
	if err != nil {
		t.Fatal(err)
	}
	var j convertjob.Job
	if err := json.Unmarshal(raw, &j); err != nil || j.Validate() != nil {
		t.Fatalf("job %s: %v", raw, err)
	}
	return j
}

// TestMove2KubeNeedsAnswersResume (SEC-M2K-01, SEC-M2K-02): a Move2Kube run that answered questions with defaults
// stores a revision and waits in needs-answers with its questions; invalid answers are refused before anything is
// queued; valid answers re-run it with a configuration file and no credentials, until no question is left open.
// A Compose file Move2Kube would ignore is refused up front. The reports are recorded real runs.
func TestMove2KubeNeedsAnswersResume(t *testing.T) {
	e := newEnv(t)
	e.enableMove2Kube(t)
	_, err := e.svc.Create(e.ctx, e.dev, e.project, CreateRequest{Name: "shop", Provider: "move2kube", Input: Input{Kind: InputCompose, SnapshotID: e.snap, ComposePath: "compose.yml"}}, ids.New(), "")
	var inv *Invalid
	if !errors.As(err, &inv) || !strings.Contains(inv.Reason, "Compose Specification") {
		t.Fatalf("a Compose Specification file was queued for Move2Kube: %v", err)
	}
	c, err := e.svc.Create(e.ctx, e.dev, e.project, CreateRequest{Name: "shop", Provider: "move2kube", Input: Input{Kind: InputSource, SnapshotID: e.snap}}, ids.New(), "")
	if err != nil {
		t.Fatal(err)
	}

	runs := []struct {
		golden string
		state  string
	}{{"shop-defaults", StateNeedsAnswers}, {"shop-answered", StateNeedsAnswers}, {"shop-resumed", StateSucceeded}}
	for i, r := range runs {
		g := loadM2K(t, r.golden)
		if i > 0 {
			bad := map[string]string{`move2kube.storage.type."db"..options`: "HostPath"}
			if _, err := e.svc.Rerun(e.ctx, e.dev, c.ID, nil, bad, ids.New(), ""); !errors.As(err, &inv) {
				t.Fatalf("a host path answer was accepted: %v", err)
			}
			if _, err := e.svc.Rerun(e.ctx, e.dev, c.ID, nil, g.Answers, ids.New(), ""); err != nil {
				t.Fatalf("run %d: %v", i+1, err)
			}
		}
		as := e.lease(t)
		j := e.job(t, as)
		args := strings.Join(j.Args, " ")
		if j.Files[m2kConfigName] != g.Config || !strings.Contains(args, "--qa-skip") || !strings.Contains(args, "--disable-local-execution") || !strings.Contains(args, "--ignore-env") {
			t.Errorf("run %d job: %v\n%s", i+1, j.Args, j.Files[m2kConfigName])
		}
		if strings.Contains(string(j.Canonical()), examplePassword) || !strings.Contains(j.Files[m2kConfigName], "spawncontainers: false") || len(j.Keep) != 2 {
			t.Errorf("run %d job carries credentials, allows containers or keeps everything: %+v", i+1, j)
		}
		if err := e.sched.Report(e.ctx, e.workerID, m2kReport(as.GetLease(), e.approved, g)); err != nil {
			t.Fatal(err)
		}
		got, _ := e.svc.Get(e.ctx, e.dev, c.ID)
		if got.State != r.state || got.CurrentRevision != i+1 || len(got.LatestRun.Questions) == 0 {
			t.Fatalf("run %d: state %s revision %d questions %d (%s)", i+1, got.State, got.CurrentRevision, len(got.LatestRun.Questions), got.LatestRun.Detail)
		}
		_, rev, _ := e.svc.Revision(e.ctx, e.dev, c.ID, 0)
		for _, f := range rev.Files {
			if strings.Contains(f.Content, examplePassword) {
				t.Errorf("revision %d: %s holds the literal password", rev.Number, f.Path)
			}
		}
	}

	if _, err := e.svc.Rerun(e.ctx, e.dev, c.ID, &Options{Format: FormatManifests, Replicas: 2}, nil, ids.New(), ""); err != nil {
		t.Fatal(err)
	}
	j := e.job(t, e.lease(t))
	cfg := j.Files[m2kConfigName]
	for _, want := range []string{`minreplicas: "2"`, `port: "5432"`, "options: PVC", "host: shop.example.com"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("a re-run with new options lost %s:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "urlpath: /db") {
		t.Error("an answer to a question the last run no longer asked was carried over")
	}
	if _, err := e.svc.Rerun(e.ctx, e.dev, c.ID, nil, map[string]string{"x": "y"}, ids.New(), ""); !errors.Is(err, ErrConflict) {
		t.Errorf("answers while a run is active: %v", err)
	}
}
