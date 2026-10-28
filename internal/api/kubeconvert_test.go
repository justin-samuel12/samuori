package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/kubeconvert"
	"samuori.invalid/samuori/internal/version"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
	"samuori.invalid/samuori/internal/workers"
)

type kubeChannel struct{ sent []*workerv1.ControllerMessage }

func (*kubeChannel) Connected(string) bool { return true }
func (c *kubeChannel) Send(_ string, m *workerv1.ControllerMessage) error {
	c.sent = append(c.sent, m)
	return nil
}

// TestKubeConversionAPI (SEC-K8S-05, SEC-K8S-06): a developer generates, previews, edits and downloads Kubernetes
// files; another team gets 404 on every call, and the ZIP holds no credential.
func TestKubeConversionAPI(t *testing.T) {
	var deps Deps
	l := newLiveWith(t, false, smallChunks(), func(d *Deps) { deps = *d })
	alice, bob, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	sum := "sha256:" + strings.Repeat("ab", 32)
	if st, _, b := l.json(t, admin, "PUT", "/providers/kompose", `{"enabled":true,"version":"v1.38.0","artifacts":{"binary-amd64":"`+sum+`","binary-arm64":"`+sum+`"},"pools":[],"projects":[],"egressPresets":[],"timeoutSeconds":120}`, nil); st != 200 {
		t.Fatalf("enable kompose = %d %v", st, b)
	}
	st, _, provs := l.json(t, alice, "GET", "/projects/"+projA+"/kube-providers", "", nil)
	items, _ := provs["items"].([]any)
	if st != 200 || len(items) != 2 {
		t.Fatalf("providers = %d %v", st, provs)
	}
	for _, it := range items {
		p := it.(map[string]any)
		if p["name"] == "move2kube" && (p["available"] != false || p["maintenance"] == "" || p["asksQuestions"] != true) {
			t.Errorf("move2kube = %v", p)
		}
		if p["name"] == "kompose" && p["available"] != true {
			t.Errorf("kompose = %v", p)
		}
		if _, leaked := p["version"]; leaked {
			t.Errorf("the provider list shows the approval: %v", p)
		}
	}
	if st, _, _ := l.json(t, bob, "GET", "/projects/"+projA+"/kube-providers", "", nil); st != 404 {
		t.Errorf("other team providers = %d", st)
	}
	files := map[string]string{}
	for _, name := range []string{"compose.yml", "api/Dockerfile", "api/server.js"} {
		b, err := os.ReadFile("../../examples/compose-shop/" + name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(b)
	}
	snap := l.snapshotOf(t, alice, projA, files)
	key := map[string]string{"Idempotency-Key": "kube-" + ids.New()}
	create := `{"name":"shop","provider":"kompose","input":{"kind":"compose","snapshotId":"` + snap + `","composePath":"compose.yml"},"options":{"format":"manifests","namespace":"shop"}}`
	if st, _, b := l.json(t, bob, "POST", "/projects/"+projA+"/kube-conversions", create, key); st != 404 {
		t.Errorf("other team create = %d %v", st, b)
	}
	st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/kube-conversions", create, key)
	if st != 202 || b["state"] != "queued" {
		t.Fatalf("create = %d %v", st, b)
	}
	id := b["id"].(string)
	if st, _, again := l.json(t, alice, "POST", "/projects/"+projA+"/kube-conversions", create, key); st != 202 || again["id"] != id {
		t.Errorf("a repeated idempotency key made another conversion: %v", again)
	}
	if st, _, _ := l.json(t, bob, "GET", "/kube-conversions/"+id, "", nil); st != 404 {
		t.Errorf("other team get = %d", st)
	}
	if st, _, _ := l.json(t, bob, "GET", "/projects/"+projA+"/kube-conversions", "", nil); st != 404 {
		t.Errorf("other team list = %d", st)
	}

	worker := enrollConversionWorker(t, l)
	ch := &kubeChannel{}
	sched := kubeconvert.NewScheduler(deps.Kube, deps.Workers, ch, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n, err := sched.Dispatch(context.Background()); err != nil || n != 1 {
		t.Fatalf("dispatch = %d %v", n, err)
	}
	var runID string
	var gen int64
	_ = l.db.SQL.QueryRow(`SELECT id, generation FROM kube_conversion_runs WHERE state = 'assigned'`).Scan(&runID, &gen)
	raw, _ := json.Marshal(map[string]any{"runId": runID, "generation": gen})
	if err := sched.Deliver(context.Background(), kubeconvert.SubjectAssign, raw); err != nil || len(ch.sent) != 1 {
		t.Fatalf("deliver: %v", err)
	}
	lease := ch.sent[0].GetConversion().GetLease()
	var golden struct {
		Stderr string            `json:"stderr"`
		Files  map[string]string `json:"files"`
	}
	g, _ := os.ReadFile("../kubeconvert/testdata/kompose-v1.38.0-shop-manifests.json")
	_ = json.Unmarshal(g, &golden)
	rep := &workerv1.ConversionReport{Lease: lease, State: workerv1.ConversionState_CONVERSION_STATE_COMPLETED, ToolStderr: []byte(golden.Stderr), ToolDigest: sum}
	for name, c := range golden.Files {
		rep.Files = append(rep.Files, &workerv1.ConversionFile{Path: "manifests/" + name, Content: []byte(c)})
	}
	if err := sched.Report(context.Background(), worker, rep); err != nil {
		t.Fatal(err)
	}

	st, _, b = l.json(t, alice, "GET", "/kube-conversions/"+id+"/revisions/1", "", nil)
	if st != 200 || b["origin"] != "provider" || b["errors"].(float64) == 0 {
		t.Fatalf("revision 1 = %d %v", st, b)
	}
	path := "/kube-conversions/" + id + "/revisions/1/file?path=manifests/api-deployment.yml"
	st, _, f := l.json(t, alice, "GET", path, "", nil)
	if st != 200 || !strings.Contains(f["content"].(string), "secretKeyRef") {
		t.Fatalf("file = %d %v", st, f)
	}
	if st, _, _ := l.json(t, bob, "GET", path, "", nil); st != 404 {
		t.Errorf("other team file = %d", st)
	}
	edit, _ := json.Marshal(map[string]string{"content": strings.Replace(f["content"].(string), "replicas: 1", "replicas: 2", 1)})
	if st, _, _ := l.json(t, bob, "PUT", "/kube-conversions/"+id+"/revisions/1/file?path=manifests/api-deployment.yml", string(edit), nil); st != 404 {
		t.Errorf("other team edit = %d", st)
	}
	st, _, b = l.json(t, alice, "PUT", "/kube-conversions/"+id+"/revisions/1/file?path=manifests/api-deployment.yml", string(edit), nil)
	if st != 201 || b["number"].(float64) != 2 || b["origin"] != "edit" {
		t.Fatalf("edit = %d %v", st, b)
	}
	if st, _, b := l.json(t, alice, "GET", "/kube-conversions/"+id+"/revisions", "", nil); st != 200 || len(b["items"].([]any)) != 2 {
		t.Errorf("revisions = %d %v", st, b)
	}

	h := map[string]string{auth.CSRFHeader: bob.csrf[auth.CSRFHeader]}
	if r := l.call(t, bob.c, "GET", "/kube-conversions/"+id+"/revisions/2/bundle", "", h); r.StatusCode != 404 {
		t.Errorf("other team download = %d", r.StatusCode)
	}
	r := l.call(t, alice.c, "GET", "/kube-conversions/"+id+"/revisions/2/bundle", "", map[string]string{auth.CSRFHeader: alice.csrf[auth.CSRFHeader]})
	zipped, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("download = %d %s", r.StatusCode, zipped)
	}
	z, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	if len(z.File) < 9 {
		t.Errorf("bundle has %d entries", len(z.File))
	}
	for _, zf := range z.File {
		rc, _ := zf.Open()
		content, _ := io.ReadAll(rc)
		rc.Close()
		if bytes.Contains(content, []byte("example-not-a-real-password")) || bytes.Contains(content, []byte(sum[7:])) && zf.Name != "shop/samuori-review.json" {
			t.Errorf("%s holds a credential or unexpected checksum", zf.Name)
		}
	}
	var audited int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('kube-conversion.create', 'kube-conversion.edit', 'kube-conversion.download')`).Scan(&audited)
	if audited != 3 {
		t.Errorf("audited %d conversion actions, want 3", audited)
	}
}

// enrollConversionWorker records a worker that passes untrusted-input eligibility and reports Kompose v1.38.0.
func enrollConversionWorker(t *testing.T, l *live) string {
	t.Helper()
	var org string
	if err := l.db.SQL.QueryRow(`SELECT id FROM organizations`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pool, worker := ids.New(), ids.New()
	caps, _ := json.Marshal(workers.Capabilities{OS: "linux", Architectures: []string{"arm64"}, IsolationProfiles: []string{"rootless-dedicated-vm"}, DaemonSlots: 1, StrictEgress: true,
		ConfigDigest: "sha256:cfg", Providers: map[string]string{"kompose": "v1.38.0"}})
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO worker_pools (id, org_id, name, location, isolation_profile, trust_domain, trusted_workloads_only, profile_approved, created_at) VALUES (?, ?, 'conv', 'default', 'rootless-dedicated-vm', 'd', ?, ?, ?)`, []any{pool, org, false, true, now}},
		{`INSERT INTO workers (id, org_id, pool_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, created_at) VALUES (?, ?, ?, 'w1', 'enrolled', 'active', 's1', ?, ?, ?, 4, ?)`,
			[]any{worker, org, pool, now.Add(time.Hour), string(caps), version.Version, now}},
		{`INSERT INTO pool_self_tests (id, org_id, pool_id, worker_id, status, failed_checks, started_at, finished_at, profile, config_digest) VALUES (?, ?, ?, ?, 'passed', '[]', ?, ?, 'rootless-dedicated-vm', 'sha256:cfg')`,
			[]any{ids.New(), org, pool, worker, now, now}},
	} {
		if _, err := l.db.SQL.Exec(l.db.Rebind(q.sql), q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return worker
}
