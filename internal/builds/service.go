// Package builds owns BuildRuns and BuildAttempts. Every transition is a database transaction that also writes an
// outbox message; NATS only wakes the dispatcher (docs/architecture/build-lifecycle.md).
package builds

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/admission"
	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/discovery"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/secrets"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
	"samuori.invalid/samuori/internal/tenancy"
	"samuori.invalid/samuori/internal/workers"
)

const (
	Blocked    = "blocked"
	Queued     = "queued"
	Assigned   = "assigned"
	Preparing  = "preparing"
	Building   = "building"
	Committing = "committing"
	Succeeded  = "succeeded"
	Failed     = "failed"
	TimedOut   = "timed_out"
	Cancelled  = "cancelled"
	Expired    = "expired"

	SubjectQueued = "samuori.runs.queued"
	SubjectAssign = "samuori.runs.assign"
	SubjectCancel = "samuori.runs.cancel"
)

var (
	ErrNotFound = errors.New("not found")
	ErrTerminal = errors.New("the build has already finished")
)

func Terminal(state string) bool {
	switch state {
	case Blocked, Succeeded, Failed, TimedOut, Cancelled, Expired:
		return true
	}
	return false
}

// Active states hold a lease.
func Active(state string) bool {
	return state == Assigned || state == Preparing || state == Building || state == Committing
}

type Run struct {
	// Version is <UTC date it succeeded>.<n>, counted per project and day; empty until the build succeeds (ADR 0054).
	Version                            string
	ID, OrgID, ProjectID, TeamID       string
	SpecID                             string
	SpecRevision                       int
	SpecDigest, SnapshotID             string
	SnapshotDigest, PlanDigest         string
	State, FailureClass, BlockedReason string
	Platforms                          []string
	IsolationProfile                   string
	// PreferredLocation is the requester's authorized preference; the scheduler tries it first (D12, D25).
	PreferredLocation               string
	SecretBearing, Untrusted        bool
	Priority                        int
	Generation                      int64
	CurrentAttemptID, LeaseWorkerID string
	LeaseDeadline                   *time.Time
	AttemptsUsed, MaxAttempts       int
	QueueDeadline                   time.Time
	ArtifactDigest                  string
	RequestedBy                     string
	CreatedAt, UpdatedAt            time.Time
}

type Attempt struct {
	ID, RunID               string
	Number                  int
	Generation              int64
	WorkerID, PoolID, State string
	LeaseDeadline           time.Time
	FailureClass, Detail    string
	ArtifactDigest, Cleanup string
	StartedAt               time.Time
	EndedAt                 *time.Time
	// EgressDenied is what the worker's egress proxy denied, advised against the current egress policy.
	EgressDenied     []egress.Advice
	EgressUnrecorded int

	reported storedEgress
}

// storedEgress is the build_attempts.egress_report document.
type storedEgress struct {
	Denied     []egress.Denial `json:"denied"`
	Unrecorded int             `json:"unrecorded,omitempty"`
}

type Event struct {
	Seq  int64
	At   time.Time
	Kind string
	Data json.RawMessage
}

type Service struct {
	db        *db.DB
	tenancy   *tenancy.Store
	specs     *specs.Service
	sources   *sources.Service
	recipes   *recipes.Statuses
	policies  *policy.Store
	admission *admission.Checker
	secrets   Secrets
	providers Providers
	placement Placement
	peers     PeerRoutes
	wake      func()

	LeaseTTL   time.Duration
	QueueTTL   time.Duration
	MaxAttempt int
	now        func() time.Time
	allowances admission.EgressAllowances
}

func NewService(d *db.DB, t *tenancy.Store, sp *specs.Service, src *sources.Service, r *recipes.Statuses, p *policy.Store, a *admission.Checker) *Service {
	return &Service{db: d, tenancy: t, specs: sp, sources: src, recipes: r, policies: p, admission: a, wake: func() {},
		LeaseTTL: 2 * time.Minute, QueueTTL: 24 * time.Hour, MaxAttempt: 3, now: time.Now}
}

// Secrets resolves a project's bindings at admission and releases values to leased attempts.
type Secrets interface {
	ActiveFor(ctx context.Context, orgID, projectID string) (map[string]secrets.Binding, error)
	Value(ctx context.Context, orgID, bindingID string, bindingVersion int, usage string) ([]byte, error)
	Pools(ctx context.Context, orgID string, bindingIDs []string) (map[string][]string, error)
}

func (s *Service) UseSecrets(sec Secrets) { s.secrets = sec }

// Providers supplies administrator-approved provider pins at plan time and re-checks them at dispatch (ADR 0047).
type Providers interface {
	Config(ctx context.Context, orgID, name string) (providers.Config, error)
	MethodAllowed(ctx context.Context, orgID, method, pool, project string) error
}

func (s *Service) UseProviders(p Providers) { s.providers = p }

// UseEgressAllowances adds each project's approved egress allowances to its build plans (ADR 0052).
func (s *Service) UseEgressAllowances(a admission.EgressAllowances) { s.allowances = a }

// OnCommit registers the function that wakes the outbox relay after a transition commits.
func (s *Service) OnCommit(f func()) { s.wake = f }

func (s *Service) enqueue(ctx context.Context, tx *sql.Tx, subject, runID string, generation int64) error {
	payload, _ := json.Marshal(map[string]any{"runId": runID, "generation": generation})
	return s.db.EnqueueOutbox(ctx, tx, ids.New(), subject, payload)
}

// RecordEvent appends an event to a run's timeline inside the caller's transaction.
func (s *Service) RecordEvent(ctx context.Context, tx *sql.Tx, runID, kind string, data any) error {
	return s.event(ctx, tx, runID, kind, data)
}

func (s *Service) event(ctx context.Context, tx *sql.Tx, runID, kind string, data any) error {
	raw, _ := json.Marshal(data)
	var seq int64
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COALESCE(MAX(seq), 0) + 1 FROM build_events WHERE run_id = ?`), runID).Scan(&seq); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO build_events (run_id, seq, at, kind, data) VALUES (?, ?, ?, ?, ?)`), runID, seq, s.now().UTC(), kind, string(raw)); err != nil {
		return err
	}
	if m, ok := data.(map[string]string); ok && kind == "state" && terminal(m["state"]) {
		return s.auditFinished(ctx, tx, runID, m["state"])
	}
	return nil
}

func terminal(state string) bool {
	return state == Succeeded || state == Failed || state == TimedOut || state == Expired
}

// auditFinished records a run reaching a terminal state; notifications are routed from it.
func (s *Service) auditFinished(ctx context.Context, tx *sql.Tx, runID, state string) error {
	var orgID, class string
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT org_id, COALESCE(failure_class, '') FROM build_runs WHERE id = ?`), runID).Scan(&orgID, &class); err != nil {
		return err
	}
	return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{OrgID: orgID, Kind: authz.System}, Action: "build.finished", ResourceType: "build", ResourceID: runID, Outcome: state, Reason: class})
}

// Create persists a run before anything else. Admission runs first; an inadmissible run is stored as blocked with
// the reason, never queued. Repeating a request with the same idempotency key returns the original run.
// CreateOptions are the requester's choices for one build.
type CreateOptions struct {
	CleanBuild bool
	// Location, when set, must be a location of the organization the project may build in.
	Location string
	// SourceRevision is the commit the snapshot was fetched from, set only by callers that fetched it; it becomes the
	// image's org.opencontainers.image.revision.
	SourceRevision string
}

// Placement answers where a project's builds may run.
type Placement interface {
	ProjectLocations(ctx context.Context, projectID string) ([]string, error)
	Locations(ctx context.Context, orgID string) ([]workers.Location, error)
}

// UsePlacement enables location restrictions and preferences.
func (s *Service) UsePlacement(pl Placement) { s.placement = pl }

var ErrLocation = errors.New("the location does not exist or is not allowed for this project")

func (s *Service) Create(ctx context.Context, p authz.Principal, roles []authz.Role, specID string, revision int, snapshotID string, opts CreateOptions, key, requestID string) (Run, error) {
	cleanBuild := opts.CleanBuild
	if r, err := s.byKey(ctx, p.ID, key); err == nil {
		return r, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Run{}, err
	}
	sp, rev, err := s.specs.Revision(ctx, p, roles, specID, revision)
	if err != nil {
		return Run{}, err
	}
	project, err := s.tenancy.GetProject(ctx, p, roles, sp.ProjectID)
	if err != nil {
		return Run{}, err
	}
	snap, err := s.sources.GetSnapshot(ctx, p, roles, snapshotID)
	if err != nil {
		return Run{}, err
	}
	if snap.DeletedAt != nil {
		return Run{}, sources.ErrDeleted
	}
	if opts.Location != "" {
		if err := s.checkLocation(ctx, p.OrgID, sp.ProjectID, opts.Location); err != nil {
			return Run{}, err
		}
	}
	federated := false
	if opts.Location != "" && s.peers != nil {
		if federated, err = s.peers.Federated(ctx, p.OrgID, opts.Location); err != nil {
			return Run{}, err
		}
	}
	check := s.admission.Check
	if federated {
		check = s.admission.CheckForPeer
	}
	report, err := check(ctx, p, roles, specID, revision, snapshotID)
	if err != nil {
		return Run{}, err
	}
	if federated && report.SecretBearing {
		report.Admissible = false
		report.PlatformIssues = append(report.PlatformIssues, discovery.Warning{Code: "secrets-stay-local", Message: "Builds that use secrets run only on this installation's own workers, never at a federation peer."})
	}
	var doc buildspec.Document
	if err := json.Unmarshal(rev.Document, &doc); err != nil {
		return Run{}, err
	}
	now := s.now().UTC()
	r := Run{ID: ids.New(), ProjectID: sp.ProjectID, TeamID: project.TeamID, SpecID: specID, SpecRevision: revision, SpecDigest: rev.Digest,
		SnapshotID: snap.ID, SnapshotDigest: snap.Digest, State: Queued, Platforms: []string{"linux/amd64"}, IsolationProfile: "vm-per-job",
		SecretBearing: report.SecretBearing, Untrusted: true, MaxAttempts: s.MaxAttempt, QueueDeadline: now.Add(s.QueueTTL), RequestedBy: p.ID, CreatedAt: now, UpdatedAt: now, PreferredLocation: opts.Location}
	if doc.Spec.Target != nil && len(doc.Spec.Target.Platforms) > 0 {
		r.Platforms = doc.Spec.Target.Platforms
	}
	var planRaw []byte
	if !report.Admissible {
		r.State, r.BlockedReason = Blocked, blockedReason(report)
	} else {
		eff, err := s.policies.Effective(ctx, p.OrgID)
		if err != nil {
			return Run{}, err
		}
		if err := admission.ProjectEgress(ctx, s.allowances, &eff, p.OrgID, sp.ProjectID, doc.Spec.Build.Method); err != nil {
			return Run{}, err
		}
		manifest, err := s.sources.Manifest(snap)
		if err != nil {
			return Run{}, err
		}
		bound := map[string]secrets.Binding{}
		if s.secrets != nil && len(doc.Spec.Secrets) > 0 {
			if bound, err = s.secrets.ActiveFor(ctx, p.OrgID, sp.ProjectID); err != nil {
				return Run{}, err
			}
		}
		plan, err := resolvePlan(ctx, &doc, rev.Digest, snap, manifest, s.recipes, p.OrgID, eff, bound)
		if err == nil && plan.Dockerfile != nil {
			plan.Dockerfile.BaseImagePins = report.BaseImagePins
		}
		if err == nil && plan.Method == providers.MethodRailpack {
			err = s.railpackPlan(ctx, &plan, &doc, manifest, p.OrgID, eff)
		} else if err == nil && plan.Method == providers.MethodBuildpacks {
			err = s.buildpacksPlan(ctx, &plan, &doc, manifest, p.OrgID, eff)
		}
		if err != nil {
			r.State, r.BlockedReason = Blocked, "the build plan could not be resolved: "+err.Error()
		} else {
			scope := artifacts.CacheScope{OrgID: p.OrgID, TrustDomain: DefaultTrustDomain, ProjectID: r.ProjectID, PolicyVersion: policyVersion(eff)}
			mode, id := artifacts.CachePolicy(report.SecretBearing, cleanBuild, scope)
			plan.Cache = PlanCache{Mode: string(mode), Scope: id}
			plan.Image = imageMetadata(&doc, plan.Method, now, opts.SourceRevision)
			if plan.Railpack != nil {
				plan.Railpack.CacheKey = scope.ID()
			}
			planRaw, r.PlanDigest = plan.Encode()
		}
	}
	platforms, _ := json.Marshal(r.Platforms)
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tenancy.RequireActive(ctx, s.db, tx, r.ProjectID); err != nil {
			return err
		}
		if planRaw != nil {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO build_plans (digest, org_id, plan, created_at) SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM build_plans WHERE digest = ?)`),
				r.PlanDigest, p.OrgID, string(planRaw), now, r.PlanDigest); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO build_runs (id, org_id, project_id, team_id, spec_id, spec_revision, spec_digest, snapshot_id, snapshot_digest, plan_digest, state, blocked_reason, platforms, isolation_profile, secret_bearing, untrusted, priority, generation, attempts_used, max_attempts, queue_deadline, requested_by, idempotency_key, created_at, updated_at, preferred_location)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0, ?, ?, ?, ?, ?, ?, ?)`),
			r.ID, p.OrgID, r.ProjectID, r.TeamID, specID, revision, r.SpecDigest, r.SnapshotID, r.SnapshotDigest, nullable(r.PlanDigest), r.State, nullable(r.BlockedReason), string(platforms),
			r.IsolationProfile, r.SecretBearing, true, r.MaxAttempts, r.QueueDeadline, p.ID, key, now, now, r.PreferredLocation); err != nil {
			return err
		}
		if err := s.event(ctx, tx, r.ID, "state", map[string]string{"state": r.State, "reason": r.BlockedReason}); err != nil {
			return err
		}
		if r.State == Queued {
			if err := s.enqueue(ctx, tx, SubjectQueued, r.ID, 0); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "build.create", ResourceType: "build", ResourceID: r.ID, Outcome: r.State, Reason: r.BlockedReason, RequestID: requestID})
	})
	if err != nil {
		if isUnique(err) {
			return s.byKey(ctx, p.ID, key)
		}
		return Run{}, err
	}
	s.wake()
	return r, nil
}

func (s *Service) checkLocation(ctx context.Context, orgID, projectID, location string) error {
	if s.placement == nil {
		return ErrLocation
	}
	known, err := s.placement.Locations(ctx, orgID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(known, func(l workers.Location) bool { return l.Name == location }) {
		return ErrLocation
	}
	allowed, err := s.placement.ProjectLocations(ctx, projectID)
	if err != nil {
		return err
	}
	if len(allowed) > 0 && !slices.Contains(allowed, location) {
		return ErrLocation
	}
	return nil
}

// DefaultTrustDomain applies until projects are mapped to trust domains with locations and pools (WP24).
const DefaultTrustDomain = "default"

func policyVersion(eff policy.Effective) string {
	return "frontends:" + eff.Versions[policy.KindFrontends] + ",base-images:" + eff.Versions[policy.KindBaseImages] + ",build-args:" + eff.Versions[policy.KindBuildArgs] + ",egress:" + eff.Versions[policy.KindEgress]
}

func blockedReason(r admission.Report) string {
	var parts []string
	for _, e := range append(r.SchemaErrors, r.SemanticErrors...) {
		parts = append(parts, e.Message)
	}
	for _, d := range r.Decisions {
		if d.Outcome == "block" {
			parts = append(parts, d.Reason)
		}
	}
	for _, w := range r.PlatformIssues {
		parts = append(parts, w.Message)
	}
	for _, sec := range r.Secrets {
		if sec.Required && sec.Status != "ready" {
			parts = append(parts, fmt.Sprintf("secret %s: %s", sec.ID, sec.Reason))
		}
	}
	out := strings.Join(parts, " ")
	if len(out) > 1000 {
		out = out[:1000] + "…"
	}
	return out
}

// Get returns a run in a project p can read with roles; others are not found.
func (s *Service) Get(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (Run, error) {
	r, err := s.load(ctx, s.db.SQL, `id = ? AND org_id = ?`, id, p.OrgID)
	if err != nil {
		return Run{}, err
	}
	if _, err := s.tenancy.GetProject(ctx, p, roles, r.ProjectID); err != nil {
		return Run{}, ErrNotFound
	}
	return r, nil
}

func (s *Service) List(ctx context.Context, p authz.Principal, roles []authz.Role, projectID, state, cursor string, limit int) ([]Run, error) {
	where, args := `org_id = ? AND id < ?`, []any{p.OrgID, cursorOrMax(cursor)}
	if projectID != "" {
		if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
			return nil, err
		}
		where, args = where+` AND project_id = ?`, append(args, projectID)
	}
	if state != "" {
		where, args = where+` AND state = ?`, append(args, state)
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectRun+` WHERE `+where+` ORDER BY id DESC LIMIT ?`), append(args, limit*4)...)
	if err != nil {
		return nil, err
	}
	var candidates []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []Run
	visible := map[string]bool{}
	for _, r := range candidates {
		if len(out) >= limit {
			break
		}
		ok, seen := visible[r.ProjectID]
		if !seen {
			_, gerr := s.tenancy.GetProject(ctx, p, roles, r.ProjectID)
			ok = gerr == nil
			visible[r.ProjectID] = ok
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Service) Attempts(ctx context.Context, p authz.Principal, roles []authz.Role, runID string) ([]Attempt, error) {
	run, err := s.Get(ctx, p, roles, runID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectAttempt+` WHERE run_id = ? ORDER BY number DESC`), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.adviseEgress(ctx, run, out)
}

// adviseEgress explains each attempt's denied destinations from the preset catalog and the organization's current
// egress policy, for the run's build method.
func (s *Service) adviseEgress(ctx context.Context, run Run, attempts []Attempt) error {
	var method string
	var eff policy.Effective
	var cat *egress.Catalog
	for i := range attempts {
		if len(attempts[i].reported.Denied) == 0 {
			continue
		}
		if cat == nil {
			var err error
			if cat, err = egress.Builtin(); err != nil {
				return err
			}
			if eff, err = s.policies.Effective(ctx, run.OrgID); err != nil {
				return err
			}
			if method, err = s.planMethod(ctx, run.PlanDigest); err != nil {
				return err
			}
			if err := admission.ProjectEgress(ctx, s.allowances, &eff, run.OrgID, run.ProjectID, method); err != nil {
				return err
			}
		}
		for _, d := range attempts[i].reported.Denied {
			attempts[i].EgressDenied = append(attempts[i].EgressDenied, cat.Advise(d, method, eff.Egress))
		}
	}
	return nil
}

func (s *Service) planMethod(ctx context.Context, digest string) (string, error) {
	var plan string
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT plan FROM build_plans WHERE digest = ?`), digest).Scan(&plan); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var p struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal([]byte(plan), &p)
	return p.Method, nil
}

// EgressEvidence returns a run the caller may read, its plan's build method, and every destination its attempts'
// egress proxies denied, advised against the current policy and the project's allowances. It is the stored evidence
// for an egress access request (ADR 0052).
func (s *Service) EgressEvidence(ctx context.Context, p authz.Principal, roles []authz.Role, runID string) (Run, string, []egress.Advice, error) {
	run, err := s.Get(ctx, p, roles, runID)
	if err != nil {
		return Run{}, "", nil, err
	}
	attempts, err := s.Attempts(ctx, p, roles, runID)
	if err != nil {
		return Run{}, "", nil, err
	}
	method, err := s.planMethod(ctx, run.PlanDigest)
	if err != nil {
		return Run{}, "", nil, err
	}
	var out []egress.Advice
	seen := map[string]bool{}
	for _, a := range attempts {
		for _, d := range a.EgressDenied {
			k := net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
			if !seen[k] {
				seen[k] = true
				out = append(out, d)
			}
		}
	}
	return run, method, out, nil
}

func (s *Service) Events(ctx context.Context, p authz.Principal, roles []authz.Role, runID string, after int64) ([]Event, error) {
	if _, err := s.Get(ctx, p, roles, runID); err != nil {
		return nil, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT seq, at, kind, data FROM build_events WHERE run_id = ? AND seq > ? ORDER BY seq LIMIT 500`), runID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var data string
		if err := rows.Scan(&e.Seq, &e.At, &e.Kind, &data); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Cancel ends a run that has not finished; an active attempt's worker is told to stop and destroy its environment.
func (s *Service) Cancel(ctx context.Context, p authz.Principal, roles []authz.Role, id, requestID string) (Run, error) {
	r, err := s.Get(ctx, p, roles, id)
	if err != nil {
		return Run{}, err
	}
	if Terminal(r.State) {
		return r, ErrTerminal
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE build_runs SET state = ?, updated_at = ? WHERE id = ? AND state = ? AND generation = ?`), Cancelled, s.now().UTC(), id, r.State, r.Generation)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrTerminal
		}
		if Active(r.State) {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE build_attempts SET state = 'abandoned', detail = 'cancelled', ended_at = ? WHERE id = ?`), s.now().UTC(), r.CurrentAttemptID); err != nil {
				return err
			}
			if err := s.enqueue(ctx, tx, SubjectCancel, id, r.Generation); err != nil {
				return err
			}
		}
		if err := s.event(ctx, tx, id, "state", map[string]string{"state": Cancelled, "by": p.ID}); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "build.cancel", ResourceType: "build", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
	if err != nil {
		return Run{}, err
	}
	s.wake()
	return s.Get(ctx, p, roles, id)
}

func (s *Service) byKey(ctx context.Context, principal, key string) (Run, error) {
	return s.load(ctx, s.db.SQL, `requested_by = ? AND idempotency_key = ?`, principal, key)
}

const selectRun = `SELECT id, org_id, project_id, team_id, spec_id, spec_revision, spec_digest, snapshot_id, snapshot_digest, plan_digest, state, failure_class, blocked_reason, platforms, isolation_profile, secret_bearing, untrusted, priority, generation, current_attempt_id, lease_worker_id, lease_deadline, attempts_used, max_attempts, queue_deadline, artifact_digest, requested_by, created_at, updated_at, preferred_location, COALESCE(version, '') FROM build_runs`

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// BaseImagePins returns the tags a run's plan pinned at admission (ADR 0049). The caller has authorized the run.
func (s *Service) BaseImagePins(ctx context.Context, r Run) ([]buildplan.BaseImagePin, error) {
	plan, err := s.RunPlan(ctx, r)
	if err != nil || plan.Dockerfile == nil {
		return nil, err
	}
	return plan.Dockerfile.BaseImagePins, nil
}

// RunPlan returns the plan a run was admitted with, or a zero plan for a blocked run. The caller has authorized
// the run.
func (s *Service) RunPlan(ctx context.Context, r Run) (Plan, error) {
	if r.PlanDigest == "" {
		return Plan{}, nil
	}
	return s.plan(ctx, r.PlanDigest)
}

func (s *Service) plan(ctx context.Context, digest string) (Plan, error) {
	var raw string
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT plan FROM build_plans WHERE digest = ?`), digest).Scan(&raw); err != nil {
		return Plan{}, err
	}
	return buildplan.Decode([]byte(raw))
}

func (s *Service) load(ctx context.Context, q queryer, where string, args ...any) (Run, error) {
	r, err := scanRun(q.QueryRowContext(ctx, s.db.Rebind(selectRun+` WHERE `+where), args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var plan, fc, br, attempt, worker, artifact sql.NullString
	var lease sql.NullTime
	var platforms string
	err := row.Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.TeamID, &r.SpecID, &r.SpecRevision, &r.SpecDigest, &r.SnapshotID, &r.SnapshotDigest, &plan, &r.State, &fc, &br, &platforms,
		&r.IsolationProfile, &r.SecretBearing, &r.Untrusted, &r.Priority, &r.Generation, &attempt, &worker, &lease, &r.AttemptsUsed, &r.MaxAttempts, &r.QueueDeadline, &artifact, &r.RequestedBy, &r.CreatedAt, &r.UpdatedAt, &r.PreferredLocation, &r.Version)
	if err != nil {
		return Run{}, err
	}
	r.PlanDigest, r.FailureClass, r.BlockedReason, r.CurrentAttemptID, r.LeaseWorkerID, r.ArtifactDigest = plan.String, fc.String, br.String, attempt.String, worker.String, artifact.String
	if lease.Valid {
		r.LeaseDeadline = &lease.Time
	}
	_ = json.Unmarshal([]byte(platforms), &r.Platforms)
	return r, nil
}

const selectAttempt = `SELECT id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, failure_class, detail, artifact_digest, cleanup, started_at, ended_at, egress_report FROM build_attempts`

func scanAttempt(row interface{ Scan(...any) error }) (Attempt, error) {
	var a Attempt
	var fc, detail, artifact, eg sql.NullString
	var ended sql.NullTime
	err := row.Scan(&a.ID, &a.RunID, &a.Number, &a.Generation, &a.WorkerID, &a.PoolID, &a.State, &a.LeaseDeadline, &fc, &detail, &artifact, &a.Cleanup, &a.StartedAt, &ended, &eg)
	a.FailureClass, a.Detail, a.ArtifactDigest = fc.String, detail.String, artifact.String
	if eg.Valid {
		_ = json.Unmarshal([]byte(eg.String), &a.reported)
		a.reported.Denied = egress.CleanDenials(a.reported.Denied)
		a.EgressUnrecorded = a.reported.Unrecorded
	}
	if ended.Valid {
		a.EndedAt = &ended.Time
	}
	return a, err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUnique(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}

func cursorOrMax(c string) string {
	if c == "" {
		return "\U0010FFFF"
	}
	return c
}
