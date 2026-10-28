package isolation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Manager owns every environment on a worker: it enforces one build per daemon slot, bounds each run by its
// wall-clock limit, always destroys the environment, and quarantines itself when cleanup fails.
type Manager struct {
	p     Provider
	slots int
	now   func() time.Time

	mu          sync.Mutex
	active      map[string]Env
	quarantined string
}

func NewManager(p Provider, slots int) *Manager {
	return &Manager{p: p, slots: slots, now: time.Now, active: map[string]Env{}}
}

func (m *Manager) Provider() string { return m.p.Name() }

// Quarantined returns the reason the worker must stop accepting work, if any.
func (m *Manager) Quarantined() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quarantined
}

func (m *Manager) FreeSlots() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quarantined != "" {
		return 0
	}
	return m.slots - len(m.active)
}

func (m *Manager) reserve(attemptID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quarantined != "" {
		return &Failure{ErrInfrastructure, "worker is quarantined: " + m.quarantined}
	}
	if _, busy := m.active[attemptID]; busy || len(m.active) >= m.slots {
		return ErrOccupied
	}
	m.active[attemptID] = Env{AttemptID: attemptID}
	return nil
}

func (m *Manager) release(attemptID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.active, attemptID)
}

// Execute prepares, runs and collects one attempt, then destroys the environment on every outcome. The returned
// Cleanup is recorded on the attempt.
func (m *Manager) Execute(ctx context.Context, s Spec, plan []byte) (RunResult, Cleanup, error) {
	if err := validate(s); err != nil {
		return RunResult{}, Cleanup{}, err
	}
	if err := m.reserve(s.AttemptID); err != nil {
		return RunResult{}, Cleanup{}, err
	}
	defer m.release(s.AttemptID)

	runCtx, cancel := context.WithTimeout(ctx, s.Limits.WallClock)
	defer cancel()
	env, err := m.p.Prepare(runCtx, s)
	if err != nil {
		return RunResult{}, Cleanup{AttemptID: s.AttemptID, Outcome: CleanupDestroyed, Detail: "nothing was created"}, classify(runCtx, err)
	}
	cleanup := m.destroy(env)
	if !s.SecretBearing {
		env.Progress = s.Progress
	}
	res, err := m.p.Run(runCtx, env, plan)
	if err == nil {
		err = m.p.Collect(runCtx, env)
	}
	return res, cleanup(), classify(runCtx, err)
}

// Convert prepares an environment for one conversion run, runs the tool and destroys the environment on every
// outcome, as Execute does for builds. The environment never receives secrets, mirror tokens or a cache.
func (m *Manager) Convert(ctx context.Context, s Spec, cs ConvertSpec) (ConvertResult, Cleanup, error) {
	conv, ok := m.p.(Converter)
	if !ok {
		return ConvertResult{}, Cleanup{}, &Failure{ErrUnsupported, m.p.Name() + " cannot run conversions"}
	}
	s.ReleaseSecret, s.MirrorToken, s.SecretBearing, s.CacheDir = nil, nil, false, ""
	if err := validate(s); err != nil {
		return ConvertResult{}, Cleanup{}, err
	}
	if err := m.reserve(s.AttemptID); err != nil {
		return ConvertResult{}, Cleanup{}, err
	}
	defer m.release(s.AttemptID)
	runCtx, cancel := context.WithTimeout(ctx, s.Limits.WallClock)
	defer cancel()
	env, err := m.p.Prepare(runCtx, s)
	if err != nil {
		return ConvertResult{}, Cleanup{AttemptID: s.AttemptID, Outcome: CleanupDestroyed, Detail: "nothing was created"}, classify(runCtx, err)
	}
	cleanup := m.destroy(env)
	res, err := conv.Convert(runCtx, env, cs)
	return res, cleanup(), classify(runCtx, err)
}

// ConvertReady reports whether the provider can run conversions.
func (m *Manager) ConvertReady() error {
	conv, ok := m.p.(Converter)
	if !ok {
		return errors.New(m.p.Name() + " cannot run conversions")
	}
	return conv.ConvertReady()
}

// destroy returns a function that tears env down with a fresh context, so cancellation never skips cleanup.
func (m *Manager) destroy(env Env) func() Cleanup {
	return func() Cleanup {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		c := Cleanup{EnvID: env.ID, AttemptID: env.AttemptID, Outcome: CleanupDestroyed}
		if err := m.p.Destroy(ctx, env); err != nil {
			c.Outcome, c.Detail = CleanupQuarantined, err.Error()
			m.mu.Lock()
			m.quarantined = fmt.Sprintf("cleanup of environment %s failed: %v", env.ID, err)
			m.mu.Unlock()
		}
		return c
	}
}

func classify(ctx context.Context, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &Failure{ErrTimeout, "the build was stopped at its wall-clock limit"}
	default:
		return err
	}
}

// Reconcile destroys environments that no live attempt owns; it runs at worker start and periodically.
func (m *Manager) Reconcile(ctx context.Context, live func(attemptID string) bool) ([]Cleanup, error) {
	envs, err := m.p.Environments(ctx)
	if err != nil {
		return nil, err
	}
	var out []Cleanup
	for _, e := range envs {
		m.mu.Lock()
		_, running := m.active[e.AttemptID]
		m.mu.Unlock()
		if running || live(e.AttemptID) {
			continue
		}
		out = append(out, m.destroy(e)())
	}
	return out, nil
}

func validate(s Spec) error {
	switch {
	case s.AttemptID == "":
		return &Failure{ErrUnsupported, "attempt ID is required"}
	case !s.Profile.Valid():
		return &Failure{ErrUnsupported, "unknown isolation profile " + string(s.Profile)}
	case (s.BaseImage != "" || s.Profile == VMPerJob) && !strings.Contains(s.BaseImage, "@sha256:"):
		return &Failure{ErrUnsupported, "the environment image must be pinned by digest"}
	case s.Limits.WallClock <= 0 || s.Limits.VCPUs == 0 || s.Limits.MemoryBytes == 0 || s.Limits.DiskBytes == 0:
		return &Failure{ErrUnsupported, "CPU, memory, disk and wall-clock limits are required"}
	}
	return nil
}
