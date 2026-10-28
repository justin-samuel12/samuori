package builds

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/secrets"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/telemetry"
	"samuori.invalid/samuori/internal/validation"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
	"samuori.invalid/samuori/internal/workers"
)

// Channel is the worker connection the scheduler delivers over.
type Channel interface {
	Connected(workerID string) bool
	Send(workerID string, m *workerv1.ControllerMessage) error
}

// Scheduler assigns queued runs to eligible workers with compare-and-swap leases and implements the lease-bound
// worker calls (workers.Attempts).
type Scheduler struct {
	s           *Service
	workers     *workers.Store
	ch          Channel
	artifacts   *artifacts.Store
	validations Validations
	mirrors     MirrorTokens
	log         *slog.Logger
}

// Validations records the initial ValidationRun when a build succeeds.
type Validations interface {
	CreateForBuild(ctx context.Context, tx *sql.Tx, in validation.BuildInput) (string, error)
}

func (sc *Scheduler) UseValidations(v Validations) { sc.validations = v }

func checksFrom(reports []*workerv1.CheckReport) []validation.Check {
	var out []validation.Check
	for _, c := range reports {
		vc := validation.Check{ID: c.GetCheckId(), Category: c.GetCategory(), Outcome: c.GetOutcome(), Reason: c.GetReason()}
		for _, f := range c.GetFindings() {
			vc.Findings = append(vc.Findings, validation.Finding{Rule: f.GetRule(), Class: f.GetClass(), Path: f.GetPath(), Layer: f.GetLayerDigest(), Size: int64(min(f.GetSizeBytes(), 1<<62)), SecretID: f.GetLogicalSecretId()})
		}
		out = append(out, vc)
	}
	return out
}

func NewScheduler(s *Service, w *workers.Store, ch Channel, art *artifacts.Store, log *slog.Logger) *Scheduler {
	return &Scheduler{s: s, workers: w, ch: ch, artifacts: art, log: log}
}

type candidate struct {
	w    workers.Worker
	pool workers.Pool
	free int
}

// Dispatch assigns as many queued runs as capacity allows. Teams take turns so one team's backlog cannot starve
// another's; within a team, higher priority and then older runs go first.
func (sc *Scheduler) Dispatch(ctx context.Context) (int, error) {
	ctx, span := telemetry.Start(ctx, "scheduler.dispatch")
	n, err := sc.dispatch(ctx)
	span.Set(telemetry.Int("samuori.assigned", n))
	span.End(err)
	return n, err
}

func (sc *Scheduler) dispatch(ctx context.Context) (int, error) {
	rows, err := sc.s.db.SQL.QueryContext(ctx, sc.s.db.Rebind(`SELECT id, org_id, team_id FROM build_runs WHERE state = ? AND NOT `+federatedQueued+` ORDER BY priority DESC, created_at LIMIT 500`), Queued)
	if err != nil {
		return 0, err
	}
	type queued struct{ id, org, team string }
	byTeam := map[string][]queued{}
	var teams []string
	for rows.Next() {
		var q queued
		if err := rows.Scan(&q.id, &q.org, &q.team); err != nil {
			rows.Close()
			return 0, err
		}
		if _, ok := byTeam[q.team]; !ok {
			teams = append(teams, q.team)
		}
		byTeam[q.team] = append(byTeam[q.team], q)
	}
	rows.Close()
	var order []queued
	for len(byTeam) > 0 {
		for _, t := range teams {
			if list := byTeam[t]; len(list) > 0 {
				order = append(order, list[0])
				if byTeam[t] = list[1:]; len(byTeam[t]) == 0 {
					delete(byTeam, t)
				}
			}
		}
	}
	pools := map[string][]candidate{}
	assigned := 0
	for _, q := range order {
		cands, ok := pools[q.org]
		if !ok {
			if cands, err = sc.candidates(ctx, q.org); err != nil {
				return assigned, err
			}
			pools[q.org] = cands
		}
		run, err := sc.s.load(ctx, sc.s.db.SQL, `id = ?`, q.id)
		if err != nil || run.State != Queued {
			continue
		}
		allowed, err := sc.secretPools(ctx, run)
		if err != nil {
			return assigned, err
		}
		locations, err := sc.workers.ProjectLocations(ctx, run.ProjectID)
		if err != nil {
			return assigned, err
		}
		need, poolAllows, err := sc.providerNeeds(ctx, run)
		if err != nil {
			return assigned, err
		}
		pins, err := sc.pinsBaseImages(ctx, run)
		if err != nil {
			return assigned, err
		}
		minMinor, err := sc.planMinor(ctx, run)
		if err != nil {
			return assigned, err
		}
		pick := -1
		now := sc.s.now()
		better := func(i int) bool {
			if pick < 0 {
				return true
			}
			pi, pp := cands[i].pool.Location == run.PreferredLocation, cands[pick].pool.Location == run.PreferredLocation
			if pi != pp {
				return pi
			}
			return cands[i].free > cands[pick].free
		}
		for i, c := range cands {
			if c.free <= 0 || (allowed != nil && !allowed[c.pool.ID]) || (len(locations) > 0 && !slices.Contains(locations, c.pool.Location)) || !poolAllows(c.pool.ID) {
				continue
			}
			if minMinor > 0 && !sc.negotiated(ctx, c.w.ID, minMinor) {
				continue
			}
			ws := c.w.State(now)
			ws.FreeSlots = uint32(c.free)
			if ok, _ := isolation.Eligible(c.pool.State(), ws, isolation.Requirement{Untrusted: run.Untrusted, Platforms: run.Platforms, Secrets: run.SecretBearing, Providers: need, BaseImagePins: pins}, now); ok && better(i) {
				pick = i
			}
		}
		if pick < 0 {
			continue
		}
		if err := sc.assign(ctx, run, cands[pick]); err != nil {
			if errors.Is(err, errNoCapacity) {
				cands[pick].free = 0
			}
			if errors.Is(err, errLostRace) || errors.Is(err, errNoCapacity) {
				continue
			}
			return assigned, err
		}
		cands[pick].free--
		assigned++
	}
	if assigned > 0 {
		sc.s.wake()
	}
	return assigned, nil
}

// providerNeeds returns the provider versions a run's plan pins and whether the administrator still allows that
// provider in a pool for the run's project. A run whose provider is disabled or limited to other pools stays queued
// until its queue deadline, like any run no worker can take (ADR 0047).
func (sc *Scheduler) providerNeeds(ctx context.Context, r Run) (map[string]string, func(pool string) bool, error) {
	all := func(string) bool { return true }
	if r.PlanDigest == "" {
		return nil, all, nil
	}
	plan, err := sc.s.plan(ctx, r.PlanDigest)
	if err != nil {
		return nil, nil, err
	}
	var need map[string]string
	switch {
	case plan.Railpack != nil:
		need = map[string]string{"railpack": plan.Railpack.Version}
	case plan.Buildpacks != nil:
		// Workers install nothing for buildpacks; they report the CNB platform API they drive the lifecycle with.
		need = map[string]string{"buildpacks": plan.Buildpacks.PlatformAPI}
	case plan.Recipe != nil && plan.Recipe.Frontend != nil:
		// The web launcher ships with Samuori, so administrators do not enable it per pool (ADR 0055).
		return map[string]string{buildplan.LauncherProvider: plan.Recipe.Frontend.Launcher}, all, nil
	default:
		return nil, all, nil
	}
	if sc.s.providers == nil {
		return need, func(string) bool { return false }, nil
	}
	decided := map[string]bool{}
	return need, func(pool string) bool {
		ok, seen := decided[pool]
		if !seen {
			ok = sc.s.providers.MethodAllowed(ctx, r.OrgID, plan.Method, pool, r.ProjectID) == nil
			decided[pool] = ok
		}
		return ok
	}, nil
}

func (sc *Scheduler) pinsBaseImages(ctx context.Context, r Run) (bool, error) {
	plan, err := sc.s.RunPlan(ctx, r)
	return err == nil && plan.Dockerfile != nil && len(plan.Dockerfile.BaseImagePins) > 0, err
}

// planMinor is the lowest protocol minor version a worker needs to build the run's plan as admitted, or 0.
func (sc *Scheduler) planMinor(ctx context.Context, r Run) (int, error) {
	plan, err := sc.s.RunPlan(ctx, r)
	switch {
	case err != nil:
		return 0, err
	case plan.Image != nil:
		return workers.ImageMetadataMinor, nil
	case plan.Dockerfile != nil && len(plan.Dockerfile.BaseImagePins) > 0:
		return workers.PinMinor, nil
	}
	return 0, nil
}

// secretPools returns the pools every binding of a secret-bearing run allows, or nil when no binding restricts
// pools.
func (sc *Scheduler) secretPools(ctx context.Context, r Run) (map[string]bool, error) {
	if !r.SecretBearing || sc.s.secrets == nil || r.PlanDigest == "" {
		return nil, nil
	}
	plan, err := sc.s.plan(ctx, r.PlanDigest)
	if err != nil {
		return nil, err
	}
	var bindings []string
	for _, s := range plan.Secrets {
		if s.BindingID != "" {
			bindings = append(bindings, s.BindingID)
		}
	}
	restrictions, err := sc.s.secrets.Pools(ctx, r.OrgID, bindings)
	if err != nil {
		return nil, err
	}
	var allowed map[string]bool
	for _, pools := range restrictions {
		if len(pools) == 0 {
			continue
		}
		next := map[string]bool{}
		for _, p := range pools {
			if allowed == nil || allowed[p] {
				next[p] = true
			}
		}
		allowed = next
	}
	return allowed, nil
}

func (sc *Scheduler) candidates(ctx context.Context, orgID string) ([]candidate, error) {
	pools, err := sc.workers.ListPools(ctx, orgID)
	if err != nil {
		return nil, err
	}
	byID := map[string]workers.Pool{}
	for _, p := range pools {
		byID[p.ID] = p
	}
	list, err := sc.workers.List(ctx, orgID, "", 10_000)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, w := range list {
		if !sc.ch.Connected(w.ID) || w.PoolID == "" {
			continue
		}
		var busy int
		if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(workers.BusySlotsQuery), w.ID, w.ID).Scan(&busy); err != nil {
			return nil, err
		}
		out = append(out, candidate{w: w, pool: byID[w.PoolID], free: int(w.Capabilities.DaemonSlots) - busy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].w.ID < out[j].w.ID })
	return out, nil
}

var (
	errLostRace   = errors.New("run changed during assignment")
	errNoCapacity = errors.New("the worker has no free daemon slot")
)

// assign is the compare-and-swap that creates a lease: it increments the fencing generation and records the
// attempt and the outbox message in one transaction.
func (sc *Scheduler) assign(ctx context.Context, r Run, c candidate) error {
	now := sc.s.now().UTC()
	deadline := now.Add(sc.s.LeaseTTL)
	gen := r.Generation + 1
	attemptID := ids.New()
	return sc.s.db.InTx(ctx, func(tx *sql.Tx) error {
		// Controllers dispatch concurrently, so free slots are counted again under a lock on the worker
		// (OD-16: one build per daemon). SQLite transactions already serialise writers.
		if sc.s.db.Dialect == db.Postgres {
			if _, err := tx.ExecContext(ctx, `SELECT id FROM workers WHERE id = $1 FOR UPDATE`, c.w.ID); err != nil {
				return err
			}
		}
		var busy int
		if err := tx.QueryRowContext(ctx, sc.s.db.Rebind(workers.BusySlotsQuery), c.w.ID, c.w.ID).Scan(&busy); err != nil {
			return err
		}
		if busy >= int(c.w.Capabilities.DaemonSlots) {
			return errNoCapacity
		}
		// The run takes the isolation profile of the pool that runs it; the worker refuses any other.
		res, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE build_runs SET state = ?, generation = ?, current_attempt_id = ?, lease_worker_id = ?, lease_deadline = ?, isolation_profile = ?, attempts_used = attempts_used + 1, updated_at = ? WHERE id = ? AND state = ? AND generation = ?`),
			Assigned, gen, attemptID, c.w.ID, deadline, c.pool.IsolationProfile, now, r.ID, Queued, r.Generation)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errLostRace
		}
		if _, err := tx.ExecContext(ctx, sc.s.db.Rebind(`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at) VALUES (?, ?, ?, ?, ?, ?, 'leased', ?, 'pending', ?)`),
			attemptID, r.ID, r.AttemptsUsed+1, gen, c.w.ID, c.w.PoolID, deadline, now); err != nil {
			return err
		}
		if err := sc.s.event(ctx, tx, r.ID, "state", map[string]any{"state": Assigned, "attempt": r.AttemptsUsed + 1, "worker": c.w.ID}); err != nil {
			return err
		}
		return sc.s.enqueue(ctx, tx, SubjectAssign, r.ID, gen)
	})
}

type wakePayload struct {
	RunID      string `json:"runId"`
	Generation int64  `json:"generation"`
}

// Deliver handles an assign or cancel wake-up: it reads the run from the database and acts only if the message's
// generation is still current, so duplicates and stale replays do nothing.
func (sc *Scheduler) Deliver(ctx context.Context, subject string, payload []byte) error {
	var p wakePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	switch subject {
	case SubjectQueued:
		_, err := sc.Dispatch(ctx)
		return err
	case SubjectAssign:
		r, err := sc.s.load(ctx, sc.s.db.SQL, `id = ?`, p.RunID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			// A database this controller cannot reach must not swallow the wake-up: another controller redelivers it.
			return err
		}
		if err != nil || r.State != Assigned || r.Generation != p.Generation {
			return nil
		}
		ctx, span := telemetry.Start(ctx, "scheduler.assign", telemetry.String("samuori.build.id", r.ID), telemetry.String("samuori.attempt.id", r.CurrentAttemptID))
		a := &workerv1.Assignment{
			Lease: &workerv1.LeaseRef{AttemptId: r.CurrentAttemptID, FencingGeneration: uint64(r.Generation)}, BuildRunId: r.ID, PlanDigest: r.PlanDigest,
			IsolationProfile: r.IsolationProfile, SecretBearing: r.SecretBearing, LeaseExpiresAt: timestamppb.New(*r.LeaseDeadline),
			Limits:      &workerv1.ResourceLimits{Vcpus: DefaultLimits.VCPUs, MemoryBytes: DefaultLimits.MemoryBytes, DiskBytes: DefaultLimits.DiskBytes, WallClock: durationpb.New(time.Duration(DefaultLimits.WallClockSecs) * time.Second)},
			TraceParent: telemetry.TraceParent(ctx),
		}
		err = sc.ch.Send(r.LeaseWorkerID, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_Assignment{Assignment: a}})
		if errors.Is(err, workers.ErrNotConnected) {
			err = nil
		}
		span.End(err)
		return err
	case SubjectCancel:
		r, err := sc.s.load(ctx, sc.s.db.SQL, `id = ?`, p.RunID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err != nil || r.LeaseWorkerID == "" {
			return nil
		}
		var attempt string
		_ = sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT id FROM build_attempts WHERE run_id = ? AND generation = ?`), r.ID, p.Generation).Scan(&attempt)
		_ = sc.ch.Send(r.LeaseWorkerID, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_Cancel{Cancel: &workerv1.Cancel{Lease: &workerv1.LeaseRef{AttemptId: attempt, FencingGeneration: uint64(p.Generation)}, Reason: "cancelled"}}})
		return nil
	}
	return nil
}

// Resend wakes delivery again for every run leased to the worker that it has not started, after the worker
// connects; the worker ignores an assignment it already holds.
func (sc *Scheduler) Resend(ctx context.Context, workerID string) error {
	rows, err := sc.s.db.SQL.QueryContext(ctx, sc.s.db.Rebind(`SELECT id, generation FROM build_runs WHERE state = ? AND lease_worker_id = ?`), Assigned, workerID)
	if err != nil {
		return err
	}
	var pending []wakePayload
	for rows.Next() {
		var p wakePayload
		if err := rows.Scan(&p.RunID, &p.Generation); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	rows.Close()
	for _, p := range pending {
		raw, _ := json.Marshal(p)
		if err := sc.Deliver(ctx, SubjectAssign, raw); err != nil {
			return err
		}
	}
	return nil
}

// current loads the run a lease refers to and checks it is the run's current attempt, held by this worker.
func (sc *Scheduler) current(ctx context.Context, q queryer, workerID string, l *workerv1.LeaseRef) (Run, error) {
	if l == nil {
		return Run{}, workers.ErrStaleLease
	}
	r, err := sc.s.load(ctx, q, `current_attempt_id = ?`, l.GetAttemptId())
	if errors.Is(err, ErrNotFound) {
		return Run{}, workers.ErrStaleLease
	}
	if err != nil {
		return Run{}, err
	}
	if uint64(r.Generation) != l.GetFencingGeneration() || r.LeaseWorkerID != workerID || !Active(r.State) {
		return Run{}, workers.ErrStaleLease
	}
	return r, nil
}

// Release returns one build secret to the worker holding the current, unexpired lease of a running attempt, and only
// for a logical ID its plan binds (SEC-SEC-04, SEC-SEC-07, SEC-SCR-03). Every decision is audited without the value.
func (sc *Scheduler) Release(ctx context.Context, workerID string, req *workerv1.ReleaseSecretRequest) (*workerv1.ReleaseSecretResponse, error) {
	id := req.GetLogicalId()
	deny := func(reason workerv1.DenialReason) (*workerv1.ReleaseSecretResponse, error) {
		return &workerv1.ReleaseSecretResponse{Result: &workerv1.ReleaseSecretResponse_Denied{Denied: &workerv1.SecretDenied{LogicalId: id, Reason: reason}}}, nil
	}
	r, err := sc.current(ctx, sc.s.db.SQL, workerID, req.GetLease())
	if errors.Is(err, workers.ErrStaleLease) {
		sc.auditRelease(ctx, workerID, req.GetLease().GetAttemptId(), Run{}, id, 0, "denied", "lease not current")
		return deny(workerv1.DenialReason_DENIAL_REASON_LEASE_INVALID)
	}
	if err != nil {
		return nil, err
	}
	if (r.State != Preparing && r.State != Building) || r.LeaseDeadline == nil || !sc.s.now().Before(*r.LeaseDeadline) {
		sc.auditRelease(ctx, workerID, r.CurrentAttemptID, r, id, 0, "denied", "attempt not running or lease expired")
		return deny(workerv1.DenialReason_DENIAL_REASON_LEASE_INVALID)
	}
	plan, err := sc.s.plan(ctx, r.PlanDigest)
	if err != nil {
		return nil, err
	}
	var ps *PlanSecret
	for i := range plan.Secrets {
		if plan.Secrets[i].ID == id {
			ps = &plan.Secrets[i]
		}
	}
	switch {
	case ps == nil:
		sc.auditRelease(ctx, workerID, r.CurrentAttemptID, r, id, 0, "denied", "not in plan")
		return deny(workerv1.DenialReason_DENIAL_REASON_NOT_IN_PLAN)
	case ps.BindingID == "":
		return deny(workerv1.DenialReason_DENIAL_REASON_OPTIONAL_UNBOUND)
	case sc.s.secrets == nil:
		return deny(workerv1.DenialReason_DENIAL_REASON_POLICY)
	}
	value, err := sc.s.secrets.Value(ctx, r.OrgID, ps.BindingID, ps.BindingVersion, "build")
	if errors.Is(err, secrets.ErrUnavailable) || errors.Is(err, secrets.ErrNotFound) {
		sc.auditRelease(ctx, workerID, r.CurrentAttemptID, r, id, ps.BindingVersion, "denied", "binding disabled or changed")
		return deny(workerv1.DenialReason_DENIAL_REASON_BINDING_DISABLED)
	}
	if err != nil {
		return nil, err
	}
	sc.auditRelease(ctx, workerID, r.CurrentAttemptID, r, id, ps.BindingVersion, "released", "")
	return &workerv1.ReleaseSecretResponse{Result: &workerv1.ReleaseSecretResponse_Secret{Secret: &workerv1.SecretValue{LogicalId: id, Value: value}}}, nil
}

func (sc *Scheduler) auditRelease(ctx context.Context, workerID, attemptID string, r Run, id string, version int, outcome, reason string) {
	actor := authz.Principal{ID: workerID, OrgID: r.OrgID, Kind: authz.Service}
	detail := fmt.Sprintf("attempt %s, secret %s", attemptID, id)
	if version > 0 {
		detail += fmt.Sprintf(" (binding version %d)", version)
	}
	if reason != "" {
		detail += ": " + reason
	}
	err := sc.s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, sc.s.db, tx, audit.Event{Actor: actor, Action: "secret.release", ResourceType: "build", ResourceID: r.ID, Outcome: outcome, Reason: detail})
	})
	if err != nil {
		sc.log.Error("audit secret release", "error", err)
	}
}

// Plan returns the immutable plan only to the worker holding the current lease (SEC-PLN-01).
func (sc *Scheduler) Plan(ctx context.Context, workerID string, l *workerv1.LeaseRef) (string, []byte, error) {
	r, err := sc.current(ctx, sc.s.db.SQL, workerID, l)
	if err != nil {
		return "", nil, err
	}
	var plan string
	if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT plan FROM build_plans WHERE digest = ?`), r.PlanDigest).Scan(&plan); err != nil {
		return "", nil, err
	}
	return r.PlanDigest, []byte(plan), nil
}

var reported = map[workerv1.AttemptState]string{
	workerv1.AttemptState_ATTEMPT_STATE_PREPARING:  Preparing,
	workerv1.AttemptState_ATTEMPT_STATE_BUILDING:   Building,
	workerv1.AttemptState_ATTEMPT_STATE_COMMITTING: Committing,
}

// Report applies a worker's state report under the lease check; reports for superseded attempts are refused and
// recorded, so a late worker never overwrites its successor (CHA-LSE-01).
func (sc *Scheduler) Report(ctx context.Context, workerID string, rep *workerv1.AttemptReport) error {
	r, err := sc.current(ctx, sc.s.db.SQL, workerID, rep.GetLease())
	if errors.Is(err, workers.ErrStaleLease) {
		_, _ = sc.s.db.SQL.ExecContext(ctx, sc.s.db.Rebind(`UPDATE build_attempts SET state = 'superseded' WHERE id = ? AND worker_id = ? AND state IN ('leased', 'preparing', 'building', 'committing', 'abandoned')`), rep.GetLease().GetAttemptId(), workerID)
		return err
	}
	if err != nil {
		return err
	}
	committed := false
	if rep.GetState() == workerv1.AttemptState_ATTEMPT_STATE_COMPLETED {
		ok, err := sc.artifacts.Committed(ctx, r.CurrentAttemptID, artifacts.KindImage, rep.GetArtifactDigest())
		if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrCorrupt) {
			return err
		}
		committed = ok
	}
	now := sc.s.now().UTC()
	cleanup := map[workerv1.CleanupOutcome]string{workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED: "destroyed", workerv1.CleanupOutcome_CLEANUP_OUTCOME_FAILED_QUARANTINED: "failed-quarantined"}[rep.GetCleanup()]
	if cleanup == "" {
		cleanup = "pending"
	}
	err = sc.s.db.InTx(ctx, func(tx *sql.Tx) error {
		cas := func(state string, extra string, args ...any) error {
			res, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE build_runs SET state = ?, updated_at = ?`+extra+` WHERE id = ? AND generation = ?`), append(append([]any{state, now}, args...), r.ID, r.Generation)...)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return workers.ErrStaleLease
			}
			return sc.s.event(ctx, tx, r.ID, "state", map[string]string{"state": state})
		}
		attempt := func(state, class, detail, digest string, ended bool) error {
			q := `UPDATE build_attempts SET state = ?, failure_class = ?, detail = ?, artifact_digest = ?, cleanup = ?`
			args := []any{state, nullable(class), nullable(detail), nullable(digest), cleanup}
			if ended {
				q, args = q+`, ended_at = ?`, append(args, now)
				if report := egressReportFrom(rep); report != nil {
					q, args = q+`, egress_report = ?`, append(args, string(report))
				}
			}
			_, err := tx.ExecContext(ctx, sc.s.db.Rebind(q+` WHERE id = ?`), append(args, r.CurrentAttemptID)...)
			return err
		}
		switch st := rep.GetState(); st {
		case workerv1.AttemptState_ATTEMPT_STATE_PREPARING, workerv1.AttemptState_ATTEMPT_STATE_BUILDING, workerv1.AttemptState_ATTEMPT_STATE_COMMITTING:
			if err := attempt(reported[st], "", "", "", false); err != nil {
				return err
			}
			return cas(reported[st], "")
		case workerv1.AttemptState_ATTEMPT_STATE_COMPLETED:
			if !committed {
				if err := attempt("failed", "infrastructure", "the reported image was not committed to internal storage or does not match", "", true); err != nil {
					return err
				}
				return cas(Failed, `, failure_class = 'infrastructure', lease_deadline = NULL`)
			}
			if err := attempt("completed", "", "", rep.GetArtifactDigest(), true); err != nil {
				return err
			}
			if err := cas(Succeeded, `, artifact_digest = ?, lease_deadline = NULL`, rep.GetArtifactDigest()); err != nil {
				return err
			}
			if _, err := sc.s.assignVersion(ctx, tx, r.ID, r.ProjectID, sc.s.now()); err != nil {
				return err
			}
			if sc.validations == nil {
				return nil
			}
			var plan string
			if err := tx.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT plan FROM build_plans WHERE digest = ?`), r.PlanDigest).Scan(&plan); err != nil {
				return err
			}
			vid, err := sc.validations.CreateForBuild(ctx, tx, validation.BuildInput{OrgID: r.OrgID, ProjectID: r.ProjectID, BuildRunID: r.ID, AttemptID: r.CurrentAttemptID,
				ArtifactDigest: rep.GetArtifactDigest(), Plan: []byte(plan), Checks: checksFrom(rep.GetChecks())})
			if err != nil {
				return err
			}
			return sc.s.event(ctx, tx, r.ID, "validation", map[string]string{"validationId": vid, "state": "pending"})
		case workerv1.AttemptState_ATTEMPT_STATE_FAILED:
			class := rep.GetFailureClass()
			if class != "infrastructure" {
				class = "application"
			}
			if err := attempt("failed", class, clip(rep.GetDetail()), "", true); err != nil {
				return err
			}
			if rep.GetFailureClass() == "timeout" {
				return cas(TimedOut, `, failure_class = 'application', lease_deadline = NULL`)
			}
			if class == "infrastructure" && r.AttemptsUsed < r.MaxAttempts {
				if err := cas(Queued, `, lease_worker_id = NULL, lease_deadline = NULL`); err != nil {
					return err
				}
				return sc.s.enqueue(ctx, tx, SubjectQueued, r.ID, r.Generation)
			}
			return cas(Failed, `, failure_class = ?, lease_deadline = NULL`, class)
		}
		return errors.New("unknown attempt state")
	})
	if err == nil {
		sc.s.wake()
	}
	return err
}

// Renew extends current leases the worker still holds and returns the ones it must abandon.
func (sc *Scheduler) Renew(ctx context.Context, workerID string, leases []*workerv1.LeaseRef) ([]*workerv1.LeaseRef, error) {
	var abandoned []*workerv1.LeaseRef
	deadline := sc.s.now().UTC().Add(sc.s.LeaseTTL)
	for _, l := range leases {
		r, err := sc.current(ctx, sc.s.db.SQL, workerID, l)
		if errors.Is(err, workers.ErrStaleLease) {
			abandoned = append(abandoned, l)
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := sc.s.db.SQL.ExecContext(ctx, sc.s.db.Rebind(`UPDATE build_runs SET lease_deadline = ? WHERE id = ? AND generation = ?`), deadline, r.ID, r.Generation); err != nil {
			return nil, err
		}
		if _, err := sc.s.db.SQL.ExecContext(ctx, sc.s.db.Rebind(`UPDATE build_attempts SET lease_deadline = ? WHERE id = ?`), deadline, r.CurrentAttemptID); err != nil {
			return nil, err
		}
	}
	return abandoned, nil
}

// Rejected requeues a run whose worker refused the assignment; the refusal is an infrastructure outcome.
func (sc *Scheduler) Rejected(ctx context.Context, workerID string, rej *workerv1.AssignmentRejection) error {
	rep := &workerv1.AttemptReport{Lease: rej.GetLease(), State: workerv1.AttemptState_ATTEMPT_STATE_FAILED, FailureClass: "infrastructure", Detail: "worker rejected the assignment: " + rej.GetReason().String() + " " + rej.GetDetail(), Cleanup: workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED}
	err := sc.Report(ctx, workerID, rep)
	if errors.Is(err, workers.ErrStaleLease) {
		return nil
	}
	return err
}

// egressReportFrom keeps the well-formed denials the worker's egress proxy reported (protocol 1.5); nil when there
// are none.
func egressReportFrom(rep *workerv1.AttemptReport) []byte {
	raw := make([]egress.Denial, 0, min(len(rep.GetEgressDenied()), egress.MaxDenials))
	for _, d := range rep.GetEgressDenied() {
		if len(raw) == egress.MaxDenials {
			break
		}
		if d.GetPort() > 65535 || d.GetCount() > math.MaxInt32 || len(d.GetReason()) > 200 {
			continue
		}
		raw = append(raw, egress.Denial{Host: d.GetHost(), Port: int(d.GetPort()), Count: int(d.GetCount()), Reason: d.GetReason()})
	}
	r := storedEgress{Denied: egress.CleanDenials(raw), Unrecorded: int(min(rep.GetEgressUnrecorded(), math.MaxInt32))}
	if len(r.Denied) == 0 && r.Unrecorded == 0 {
		return nil
	}
	b, _ := json.Marshal(r)
	return b
}

func clip(s string) string {
	if len(s) > 1000 {
		return s[:1000]
	}
	return s
}

const sourceChunk = 256 << 10

// Source streams the current attempt's snapshot: the canonical manifest, then each file's verified content. A
// worker can fetch only the source of the attempt it currently leases (SEC-STO-01).
func (sc *Scheduler) Source(ctx context.Context, workerID string, l *workerv1.LeaseRef, send func(*workerv1.SourceChunk) error) error {
	r, err := sc.current(ctx, sc.s.db.SQL, workerID, l)
	if err != nil {
		return err
	}
	snap, err := sc.s.sources.SnapshotByID(ctx, r.SnapshotID)
	if err != nil {
		return err
	}
	m, err := sc.s.sources.Manifest(snap)
	if err != nil {
		return err
	}
	if err := send(&workerv1.SourceChunk{Kind: &workerv1.SourceChunk_ManifestJson{ManifestJson: m.Canonical()}}); err != nil {
		return err
	}
	for _, f := range m.Files {
		if err := send(&workerv1.SourceChunk{Kind: &workerv1.SourceChunk_File{File: &workerv1.SourceFileStart{Path: f.Path, Digest: f.Digest, Size: uint64(f.Size), Executable: f.Executable}}}); err != nil {
			return err
		}
		rc, err := sc.artifacts.Blobs().Open(f.Digest)
		if err != nil {
			return err
		}
		buf := make([]byte, sourceChunk)
		for {
			n, rerr := rc.Read(buf)
			if n > 0 {
				if err := send(&workerv1.SourceChunk{Kind: &workerv1.SourceChunk_Data{Data: append([]byte(nil), buf[:n]...)}}); err != nil {
					rc.Close()
					return err
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				rc.Close()
				return rerr
			}
		}
		rc.Close()
	}
	return nil
}

// Cache streams the newest importable cache of the attempt's own plan scope (SEC-CCH-06). The worker cannot name a
// scope; secret-bearing plans get none while scoped reuse is disabled (SEC-CCH-03).
func (sc *Scheduler) Cache(ctx context.Context, workerID string, l *workerv1.LeaseRef, send func(*workerv1.CacheChunk) error) error {
	r, err := sc.current(ctx, sc.s.db.SQL, workerID, l)
	if err != nil {
		return err
	}
	var raw string
	if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT plan FROM build_plans WHERE digest = ?`), r.PlanDigest).Scan(&raw); err != nil {
		return err
	}
	var plan Plan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return err
	}
	if plan.Cache.Mode != string(artifacts.CacheScoped) || plan.Cache.Scope == "" || (r.SecretBearing && !artifacts.ScopedReuseEnabled) {
		return workers.ErrNoCache
	}
	e, err := sc.artifacts.LatestCache(ctx, r.OrgID, plan.Cache.Scope)
	if errors.Is(err, artifacts.ErrNoCache) {
		return workers.ErrNoCache
	}
	if err != nil {
		return err
	}
	rc, err := sc.artifacts.Blobs().Open(e.Digest)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := send(&workerv1.CacheChunk{Kind: &workerv1.CacheChunk_Start{Start: &workerv1.CacheStart{Digest: e.Digest, SizeBytes: uint64(e.Size)}}}); err != nil {
		return err
	}
	buf := make([]byte, sourceChunk)
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			if err := send(&workerv1.CacheChunk{Kind: &workerv1.CacheChunk_Data{Data: append([]byte(nil), buf[:n]...)}}); err != nil {
				return err
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return sc.artifacts.TouchCache(ctx, e.ID)
}

// Grant issues an upload grant for the current attempt. Cache uploads are allowed only into the plan's assigned
// scope, and never while the plan disables cache (SEC-CCH-03, SEC-CCH-04, SEC-CCH-06).
func (sc *Scheduler) Grant(ctx context.Context, workerID string, req *workerv1.ArtifactUploadRequest) (string, time.Time, error) {
	r, err := sc.current(ctx, sc.s.db.SQL, workerID, req.GetLease())
	if err != nil {
		return "", time.Time{}, err
	}
	g := artifacts.Grant{OrgID: "", RunID: r.ID, AttemptID: r.CurrentAttemptID, Generation: r.Generation, WorkerID: workerID, Kind: req.GetKind(), Digest: req.GetDigest(), Size: int64(req.GetSizeBytes())}
	if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT org_id FROM build_runs WHERE id = ?`), r.ID).Scan(&g.OrgID); err != nil {
		return "", time.Time{}, err
	}
	if g.Kind == artifacts.KindCache {
		var raw string
		if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT plan FROM build_plans WHERE digest = ?`), r.PlanDigest).Scan(&raw); err != nil {
			return "", time.Time{}, err
		}
		var plan Plan
		if err := json.Unmarshal([]byte(raw), &plan); err != nil {
			return "", time.Time{}, err
		}
		if plan.Cache.Mode != string(artifacts.CacheScoped) || plan.Cache.Scope == "" || (r.SecretBearing && !artifacts.ScopedReuseEnabled) {
			return "", time.Time{}, errors.New("this build may not export cache")
		}
		g.Scope = plan.Cache.Scope
	}
	return sc.artifacts.Sign(g)
}

// Upload stores bytes for a grant issued to this worker, re-checking that the grant's lease is still current.
func (sc *Scheduler) Upload(ctx context.Context, workerID, token string, body io.Reader) error {
	g, err := sc.artifacts.Verify(token)
	if err != nil || g.WorkerID != workerID {
		return workers.ErrGrantRefused
	}
	if _, err := sc.current(ctx, sc.s.db.SQL, workerID, &workerv1.LeaseRef{AttemptId: g.AttemptID, FencingGeneration: uint64(g.Generation)}); err != nil {
		return err
	}
	if err := sc.artifacts.Accept(ctx, g, body); err != nil {
		if errors.Is(err, artifacts.ErrMismatch) {
			return workers.ErrUploadMismatch
		}
		return err
	}
	return nil
}

// buildsPins reports whether a worker negotiated a protocol that builds base-image pins; older workers would build
// the moving tag (ADR 0049).
func (sc *Scheduler) buildsPins(ctx context.Context, workerID string) bool {
	return sc.negotiated(ctx, workerID, workers.PinMinor)
}

func (sc *Scheduler) negotiated(ctx context.Context, workerID string, minor int) bool {
	var got int
	err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT protocol_minor FROM workers WHERE id = ?`), workerID).Scan(&got)
	return err == nil && got >= minor
}
