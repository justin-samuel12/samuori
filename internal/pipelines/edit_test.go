package pipelines

import (
	"errors"
	"strings"
	"testing"
)

// TestUpdateAndDelete: only administrators edit or delete a pipeline. An edit keeps the webhook URL and secret and
// stores a replaced token only in the secret store. A delete is refused while a run is queued, then stops the
// webhook, hides the pipeline, frees its name and keeps its runs and audit trail.
func TestUpdateAndDelete(t *testing.T) {
	f := setup(t)
	p, secret := f.create(t, SchemeHMAC)
	name, repo, token, rev := "orders-release", "https://git.example/payments/orders-v2.git", "new-read-token", 2

	if _, err := f.svc.Update(f.ctx, f.dev, p.ID, Changes{Name: &name}, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer edited a pipeline: %v", err)
	}
	bad, missing, badRef := "http://git.example/r.git", 9, []string{"main"}
	for what, c := range map[string]Changes{"plain HTTP": {RepoURL: &bad}, "missing revision": {SpecRevision: &missing}, "short ref": {Refs: badRef}} {
		var inv *Invalid
		if _, err := f.svc.Update(f.ctx, f.admin, p.ID, c, ""); !errors.As(err, &inv) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if _, err := f.d.SQL.Exec(`INSERT INTO spec_revisions (spec_id, revision, digest, api_version, document, created_by, created_at) VALUES (?, 2, 'sha256:x', 'v1', '{}', 'u', CURRENT_TIMESTAMP)`, f.specID); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Update(f.ctx, f.admin, p.ID, Changes{Name: &name, RepoURL: &repo, Refs: []string{"refs/heads/release/*"}, SpecRevision: &rev, Token: &token}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != name || got.RepoURL != repo || got.SpecRevision != 2 || !got.HasToken || got.Refs[0] != "refs/heads/release/*" {
		t.Fatalf("updated = %+v", got)
	}
	var stored string
	_ = f.d.SQL.QueryRow(`SELECT repo_url || refs || COALESCE(username, '') FROM pipelines WHERE id = ?`, p.ID).Scan(&stored)
	if strings.Contains(stored, token) {
		t.Fatal("the replaced token is stored outside the secret store")
	}

	body := push("refs/heads/release/1.0", headCommit)
	res, err := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "e1", body), body)
	if err != nil || res.Status != Queued {
		t.Fatalf("the webhook secret changed with the edit: %+v %v", res, err)
	}
	if err := f.svc.Delete(f.ctx, f.dev, p.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer deleted a pipeline: %v", err)
	}
	if err := f.svc.Delete(f.ctx, f.admin, p.ID, ""); !errors.Is(err, ErrActive) {
		t.Fatalf("delete with a queued run = %v", err)
	}
	if _, err := f.svc.ProcessNext(f.ctx); err != nil {
		t.Fatal(err)
	}
	if req := f.fetch.reqs[len(f.fetch.reqs)-1]; req.URL != repo || req.Token != token {
		t.Fatalf("the run used %+v", req)
	}
	if err := f.svc.Delete(f.ctx, f.admin, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Hook(f.ctx, p.ID, signed(secret, "push", "e2", body), body); !errors.Is(err, ErrUnverified) {
		t.Fatalf("a deleted pipeline answered its webhook: %v", err)
	}
	if _, err := f.svc.Get(f.ctx, f.admin, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
	if list, _ := f.svc.List(f.ctx, f.admin, f.project); len(list) != 0 {
		t.Fatalf("a deleted pipeline is listed: %+v", list)
	}
	if err := f.svc.Delete(f.ctx, f.admin, p.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	var runs, audited int
	_ = f.d.SQL.QueryRow(`SELECT COUNT(*) FROM pipeline_runs WHERE pipeline_id = ?`, p.ID).Scan(&runs)
	_ = f.d.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE resource_id = ? AND action IN ('pipeline.update', 'pipeline.delete')`, p.ID).Scan(&audited)
	if runs != 1 || audited != 2 {
		t.Fatalf("runs kept = %d, audited = %d", runs, audited)
	}
	if _, _, err := f.svc.Create(f.ctx, f.admin, Pipeline{Name: name, ProjectID: f.project, SpecID: f.specID, RepoURL: repo, Refs: []string{"refs/heads/main"}, HookScheme: SchemeHMAC, OwnerID: f.robot.ID}, "", ""); err != nil {
		t.Fatalf("the deleted pipeline's name is still taken: %v", err)
	}
}
