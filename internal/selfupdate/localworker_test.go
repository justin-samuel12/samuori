package selfupdate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/workers"
)

func TestLocalWorkerUpdatesWithItsController(t *testing.T) {
	s, d, fw := setup(t, "0.2.0-dev")
	ctx := context.Background()
	good := newSigner(t, t.TempDir(), "good")
	s.signers = good.signers
	addPool(t, d, "pl", "local", "here", "rootless-dedicated-vm")
	addPool(t, d, "p1", "a", "here", "rootless-dedicated-vm")
	addWorker(t, d, "wl", "pl", "local", "active", "0.2.0-dev", 3)
	addWorker(t, d, "w1", "p1", "a-1", "active", "0.2.0-dev", 3)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(q), args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE controllers SET local_worker = ?`, true)
	if _, err := s.Upload(ctx, admin, bundle(t, good, "0.3.0-dev", workers.ProtocolMinor, "arm64"), ""); err != nil {
		t.Fatal(err)
	}
	r, err := s.Start(ctx, admin, StartRequest{Version: "0.3.0-dev", Reason: "monthly update"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for _, st := range r.Steps {
		plan = append(plan, fmt.Sprintf("%s:%s:%d:%s", st.Kind, st.Name, st.Wave, st.LocalWorker))
	}
	host := s.member.Hostname
	if got := strings.Join(plan, ","); got != "controller:"+host+" with worker local:1:wl,worker:a-1:2:" {
		t.Fatalf("plan = %s; want the local worker in its controller's step, not in a wave", got)
	}
	steps := func() []Step {
		r, _ := s.Get(ctx, admin, r.ID)
		return r.Steps
	}
	advance := func() {
		t.Helper()
		if err := s.advance(ctx); err != nil {
			t.Fatal(err)
		}
	}
	advance()
	if st := steps()[0]; st.State != "draining" || !slices.Contains(fw.actions, "drain:wl") {
		t.Fatalf("the local worker was not drained before its controller: %s %v", st.State, fw.actions)
	}
	advance()
	if st := steps()[0]; st.State != "requested" {
		t.Fatalf("a drained local worker did not let its controller update: %s", st.State)
	}
	staged := time.Now().UTC().Add(-time.Minute)
	exec(`UPDATE rollout_steps SET state = 'applying', updated_at = ? WHERE seq = 1`, staged)
	exec(`UPDATE controllers SET version = '0.3.0-dev', started_at = ?`, staged.Add(-time.Hour))
	exec(`UPDATE workers SET version = '0.3.0-dev' WHERE id = 'wl'`)
	advance()
	if st := steps()[0]; st.State != "applying" {
		t.Fatalf("a controller process from before the install counted as updated: %s", st.State)
	}
	exec(`UPDATE controllers SET started_at = ?`, time.Now().UTC())
	advance()
	if st := steps()[0]; st.State != "testing" || !slices.Contains(fw.actions, "self-test:wl") {
		t.Fatalf("the restarted local worker was not self-tested in its controller's step: %s %v", st.State, fw.actions)
	}
	if st := steps()[1]; st.State != "pending" {
		t.Fatalf("a worker wave started before the controller step finished: %s", st.State)
	}
	exec(`INSERT INTO pool_self_tests (id, org_id, pool_id, worker_id, status, failed_checks, started_at) VALUES ('t1', 'o1', 'pl', 'wl', 'failed', '[]', ?)`, time.Now().UTC())
	advance()
	if st := steps()[0]; st.State != "rolling_back" || fw.sent[len(fw.sent)-1] != "rollback:wl:0.3.0-dev" || !strings.Contains(st.Detail, "previous release of the controller and the worker") {
		t.Fatalf("a failed local worker self-test did not roll its host back: %s %q %v", st.State, st.Detail, fw.sent)
	}
	exec(`UPDATE workers SET version = '0.2.0-dev' WHERE id = 'wl'`)
	advance()
	if st := steps()[0]; st.State != "failed" || !strings.Contains(st.Detail, "rolled back to 0.2.0-dev") {
		t.Fatalf("after the rollback = %s %q", st.State, st.Detail)
	}
	advance()
	if got, _ := s.Get(ctx, admin, r.ID); !got.Paused || got.Steps[1].State != "pending" {
		t.Fatalf("a failed controller step did not pause the rollout: paused %v, next %s", got.Paused, got.Steps[1].State)
	}
}
