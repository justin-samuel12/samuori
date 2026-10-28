package observability

import (
	"context"

	"samuori.invalid/samuori/internal/health"
	"samuori.invalid/samuori/internal/telemetry"
)

// Gauges reports the installation-wide snapshot as metrics. Labels are states, pools and health components only,
// never organization, project or user names.
func (s Source) Gauges(ctx context.Context) ([]telemetry.Sample, error) {
	snap, err := s.Take(ctx, "")
	if err != nil {
		return nil, err
	}
	return snap.Samples(), nil
}

func (snap Snapshot) Samples() []telemetry.Sample {
	var out []telemetry.Sample
	add := func(name, help string, v float64, labels ...string) {
		l := map[string]string{}
		for i := 0; i+1 < len(labels); i += 2 {
			l[labels[i]] = labels[i+1]
		}
		out = append(out, telemetry.Sample{Name: name, Help: help, Labels: l, Value: v})
	}
	byState := func(name, help, label string, m map[string]int) {
		for k, v := range m {
			add(name, help, float64(v), label, k)
		}
	}
	byState("samuori_builds", "Build runs by current state.", "state", snap.Builds.ByState)
	byState("samuori_builds_finished_24h", "Build runs that reached a terminal state in the last 24 hours.", "state", snap.Builds.Last24h)
	add("samuori_build_queue_oldest_seconds", "Age of the oldest queued build run.", snap.Builds.QueuedOldestSeconds)
	for _, c := range snap.Builds.FailureClasses24h {
		add("samuori_build_failures_24h", "Failed or timed-out build runs in the last 24 hours by failure class (top five).", float64(c.Count), "class", c.Key)
	}
	byState("samuori_validations", "Validation runs by current state.", "state", snap.Validations.ByState)
	byState("samuori_validations_finished_24h", "Validation runs finished in the last 24 hours by verdict.", "verdict", snap.Validations.Last24h)
	byState("samuori_publications", "Publications by current state.", "state", snap.Publications.ByState)
	byState("samuori_publications_finished_24h", "Publications that reached a terminal state in the last 24 hours.", "state", snap.Publications.Last24h)
	byState("samuori_workers", "Workers by enrollment state.", "enrollment", snap.Workers.ByEnrollment)
	add("samuori_workers_connected", "Enrolled workers holding a live channel to the controller.", float64(snap.Workers.Connected))
	add("samuori_workers_stale", "Enrolled workers without a heartbeat for more than two minutes.", float64(snap.Workers.Stale))
	for _, p := range snap.Workers.Pools {
		for state, n := range map[string]int{"enrolled": p.Enrolled, "connected": p.Connected, "stale": p.Stale} {
			add("samuori_pool_workers", "Workers per pool by state.", float64(n), "pool_id", p.ID, "state", state)
		}
		add("samuori_pool_profile_approved", "Whether the pool's isolation profile is approved.", boolValue(p.ProfileApproved), "pool_id", p.ID, "profile", p.IsolationProfile)
	}
	status := map[string]int{}
	for _, r := range snap.Registries {
		status[r.Status]++
	}
	byState("samuori_registry_profiles", "Registry profiles by last connection test result.", "status", status)
	add("samuori_outbox_pending", "Messages waiting in the transactional outbox.", float64(snap.Outbox.Pending))
	add("samuori_outbox_oldest_seconds", "Age of the oldest unpublished outbox message.", snap.Outbox.OldestSeconds)
	for _, c := range snap.Health.Components {
		add("samuori_health_component_up", "Whether a health check passes.", boolValue(c.Status == health.StatusOK), "component", c.Name)
	}
	if snap.Health.Status != "" {
		add("samuori_health_ok", "Whether the controller reports overall health ok.", boolValue(snap.Health.Status == health.StatusOK))
	}
	return out
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
