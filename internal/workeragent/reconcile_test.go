package workeragent

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"samuori.invalid/samuori/internal/isolation"
)

type stubProvider struct{ isolation.Provider }

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Capabilities(context.Context) (isolation.Capabilities, error) {
	return isolation.Capabilities{Profiles: []isolation.Profile{isolation.VMPerJob}, FreeSlots: 2}, nil
}

type leftoverProvider struct {
	stubProvider
	mu        sync.Mutex
	envs      []isolation.Env
	destroyed []string
}

func (p *leftoverProvider) Environments(context.Context) ([]isolation.Env, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]isolation.Env(nil), p.envs...), nil
}

func (p *leftoverProvider) Destroy(_ context.Context, e isolation.Env) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyed = append(p.destroyed, e.AttemptID)
	return nil
}

func TestReconcileDestroysOrphanedEnvironments(t *testing.T) {
	t.Run("SEC-SEC-14 (crash recovery)", func(t *testing.T) {
		p := &leftoverProvider{envs: []isolation.Env{{ID: "e1", AttemptID: "crashed"}, {ID: "e2", AttemptID: "running"}}}
		a := New(Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		a.UseProvider(p)
		a.leases["running"] = running{cancel: func() {}}

		a.selfTesting = true
		if got := a.Reconcile(context.Background()); len(got) != 0 || len(p.destroyed) != 0 {
			t.Fatalf("reconciled during a self-test: %+v", p.destroyed)
		}
		a.selfTesting = false
		got := a.Reconcile(context.Background())
		if !slices.Equal(p.destroyed, []string{"crashed"}) || len(got) != 1 || got[0].Outcome != isolation.CleanupDestroyed {
			t.Fatalf("destroyed %v, cleanups %+v", p.destroyed, got)
		}
	})
}
