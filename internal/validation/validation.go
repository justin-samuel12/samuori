// Package validation runs ValidationRuns: the checks an image artifact must pass before publication. Worker checks
// (runtime test, build-secret canary) arrive with the build; controller checks (sensitive content, SBOM) run here
// in a restricted subprocess. Verdicts fail closed (docs/architecture/validation.md, ADRs 0003 and 0031).
package validation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/imagescan"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/telemetry"
	"samuori.invalid/samuori/internal/tenancy"
)

const SubjectPending = "samuori.validations.pending"

const (
	Pending   = "pending"
	Running   = "running"
	Completed = "completed"
	Errored   = "errored"
	TimedOut  = "timed_out"

	CheckSensitive = "sensitive-content"
	CheckSBOM      = "sbom"
)

var (
	ErrNotFound  = errors.New("validation not found")
	ErrForbidden = errors.New("forbidden")
)

type Finding struct {
	Rule     string `json:"rule"`
	Class    string `json:"class"`
	Path     string `json:"path"`
	Layer    string `json:"layer,omitempty"`
	Size     int64  `json:"size"`
	SecretID string `json:"secretId,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Expected string `json:"expected,omitempty"`
	// ExceptionID names the approved exception that let a require-exception finding pass.
	ExceptionID string `json:"exceptionId,omitempty"`
}

type Tool struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	// Database describes a scanner's vulnerability database: source, version and age (ADR 0021).
	Database string `json:"database,omitempty"`
}

type Check struct {
	ID              string
	Category        string
	Origin          string
	Required        bool
	Outcome         string
	Reason          string
	Tool            Tool
	Findings        []Finding
	Vulnerabilities []Vulnerability
	OutputDigest    string
}

type Run struct {
	ID, OrgID, ProjectID       string
	ArtifactID, ArtifactDigest string
	BuildRunID, SourceRunID    string
	Trigger, State, Verdict    string
	Reason                     string
	// Classification is "restricted" when any finding passed only through an exception (ADR 0031).
	Classification        string
	CreatedAt             time.Time
	StartedAt, FinishedAt *time.Time
	Checks                []Check
	claim                 string
}

// Scanner inspects an image archive in a restricted subprocess. Degraded reports a scan that ran without the
// Linux sandbox (development hosts only).
type Scanner interface {
	Scan(ctx context.Context, archive string, rules []policy.SensitiveRule) (rep imagescan.Report, degraded bool, err error)
}

// SBOMGenerator produces an SPDX document; ErrNoTool means the generator is not installed.
type SBOMGenerator interface {
	Generate(ctx context.Context, archive string) ([]byte, Tool, error)
}

var ErrNoTool = errors.New("the SBOM generator is not installed on the controller")

type Service struct {
	db       *db.DB
	blobs    *storage.Blobs
	tenancy  *tenancy.Store
	policies *policy.Store
	scanner  Scanner
	sbom     SBOMGenerator
	vulns    VulnerabilityScanner
	log      *slog.Logger
	wake     func()
	onDone   func(ctx context.Context, tx *sql.Tx, r Run) error
	now      func() time.Time
	owner    cluster.Ownership
}

// UseOwnership records which controller runs each validation, so that only runs of controllers that are gone are
// restarted.
func (s *Service) UseOwnership(o cluster.Ownership) { s.owner = o }

// ErrSuperseded means another controller restarted the validation because this one was presumed gone; the late
// result is discarded.
var ErrSuperseded = errors.New("validation: the run was taken over by another controller")

// UseVulnerabilityScanner enables the vulnerability check with an operator-chosen scanner.
func (s *Service) UseVulnerabilityScanner(v VulnerabilityScanner) { s.vulns = v }

func NewService(d *db.DB, blobs *storage.Blobs, t *tenancy.Store, p *policy.Store, sc Scanner, sb SBOMGenerator, log *slog.Logger) *Service {
	return &Service{db: d, blobs: blobs, tenancy: t, policies: p, scanner: sc, sbom: sb, log: log, wake: func() {}, now: time.Now, owner: &cluster.Solo{}}
}

// OnCommit registers the outbox relay wake-up.
func (s *Service) OnCommit(f func()) { s.wake = f }

// OnComplete runs in the completing transaction, for example to record a build event.
func (s *Service) OnComplete(f func(ctx context.Context, tx *sql.Tx, r Run) error) { s.onDone = f }

// BuildInput is the initial validation of an image a build produced.
type BuildInput struct {
	OrgID, ProjectID, BuildRunID, AttemptID, ArtifactDigest string
	Plan                                                    []byte
	Checks                                                  []Check
}

var (
	checkIDs   = map[string]bool{"build-secret-canary": true, "runtime-http-startup": true, "runtime-process-start": true, "runtime-cli-smoke": true, "runtime-static-serve": true}
	categories = map[string]bool{"test": true, "sensitive-content": true}
	outcomes   = map[string]bool{"passed": true, "failed": true, "blocked": true, "skipped": true, "errored": true}
)

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// sanitize accepts worker check reports as untrusted data: known checks only, bounded sizes.
func sanitize(in []Check) []Check {
	var out []Check
	seen := map[string]bool{}
	for _, c := range in {
		if !checkIDs[c.ID] || !categories[c.Category] || !outcomes[c.Outcome] || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		c.Origin, c.Required, c.Reason = "worker", false, clip(c.Reason, 1024)
		if len(c.Findings) > 500 {
			c.Findings = c.Findings[:500]
		}
		for i := range c.Findings {
			f := &c.Findings[i]
			f.Rule, f.Class, f.Path, f.Layer, f.SecretID = clip(f.Rule, 64), clip(f.Class, 64), clip(f.Path, 1024), clip(f.Layer, 80), clip(f.SecretID, 64)
			f.Outcome = ""
			if f.Class == imagescan.ClassBuildSecret {
				f.Outcome = "block"
			}
		}
		out = append(out, c)
	}
	return out
}

// CreateForBuild records the initial ValidationRun for a succeeded build inside the build's transaction.
func (s *Service) CreateForBuild(ctx context.Context, tx *sql.Tx, in BuildInput) (string, error) {
	var artifactID string
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM build_artifacts WHERE attempt_id = ? AND kind = 'image' AND digest = ?`), in.AttemptID, in.ArtifactDigest).Scan(&artifactID); err != nil {
		return "", err
	}
	id, now := ids.New(), s.now().UTC()
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO validation_runs (id, org_id, project_id, artifact_id, artifact_digest, build_run_id, trigger_kind, state, plan, policy_versions, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'build', 'pending', ?, '{}', ?)`), id, in.OrgID, in.ProjectID, artifactID, in.ArtifactDigest, in.BuildRunID, string(in.Plan), now); err != nil {
		return "", err
	}
	for _, c := range sanitize(in.Checks) {
		if err := s.insertCheck(ctx, tx, id, c); err != nil {
			return "", err
		}
	}
	payload, _ := json.Marshal(map[string]string{"validationId": id})
	return id, s.db.EnqueueOutbox(ctx, tx, ids.New(), SubjectPending, payload)
}

func (s *Service) insertCheck(ctx context.Context, tx *sql.Tx, runID string, c Check) error {
	tool, _ := json.Marshal(c.Tool)
	if c.Findings == nil {
		c.Findings = []Finding{}
	}
	findings, _ := json.Marshal(c.Findings)
	if c.Vulnerabilities == nil {
		c.Vulnerabilities = []Vulnerability{}
	}
	vulns, _ := json.Marshal(c.Vulnerabilities)
	var out any
	if c.OutputDigest != "" {
		out = c.OutputDigest
	}
	_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO check_results (id, validation_run_id, check_id, category, origin, required, outcome, reason, tool, findings, output_digest, created_at, vulnerabilities) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		ids.New(), runID, c.ID, c.Category, c.Origin, c.Required, c.Outcome, c.Reason, string(tool), string(findings), out, s.now().UTC(), string(vulns))
	return err
}

// Deliver handles an outbox wake-up; it is safe to run more than once.
func (s *Service) Deliver(ctx context.Context, _ string, payload []byte) error {
	var p struct {
		ValidationID string `json:"validationId"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.ValidationID == "" {
		return nil
	}
	return s.Process(ctx, p.ValidationID)
}

// Reconcile restarts validations whose controller is gone and runs pending ones whose wake-up was lost. Runs on
// live controllers are left alone, and a superseded controller's late verdict is refused (finish).
func (s *Service) Reconcile(ctx context.Context) error {
	orphaned, args := s.owner.Orphaned("owner_controller")
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE validation_runs SET state = 'pending', started_at = NULL, claim = NULL WHERE state = 'running' AND `+orphaned), args...)
	if err != nil {
		return err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id FROM validation_runs WHERE state = 'pending' ORDER BY created_at LIMIT 20`))
	if err != nil {
		return err
	}
	var pending []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, id)
	}
	rows.Close()
	for _, id := range pending {
		if err := s.Process(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// Process runs the controller checks of one pending ValidationRun and records its verdict.
func (s *Service) Process(ctx context.Context, id string) error {
	ctx, span := telemetry.Start(ctx, "validation.process", telemetry.String("samuori.validation.id", id))
	err := s.process(ctx, id)
	if errors.Is(err, ErrSuperseded) {
		s.log.Warn("validation result discarded: another controller took the run over", "validation", id)
		err = nil
	}
	span.End(err)
	return err
}

func (s *Service) process(ctx context.Context, id string) error {
	now := s.now().UTC()
	claim := ids.New()
	res, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE validation_runs SET state = 'running', started_at = ?, owner_controller = ?, claim = ? WHERE id = ? AND state = 'pending'`), now, s.owner.ControllerID(), claim, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	r, rawPlan, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	r.claim = claim
	plan, err := buildplan.Decode(rawPlan)
	if err != nil {
		return s.finish(ctx, r, Errored, "", "the build plan could not be read", nil, map[string]string{})
	}
	eff, err := s.policies.Effective(ctx, r.OrgID)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	added := []Check{s.sensitive(cctx, r, plan, eff), s.sbomCheck(cctx, r), s.vulnerabilityCheck(cctx, r, eff)}
	if cctx.Err() != nil && ctx.Err() == nil {
		return s.finish(ctx, r, TimedOut, "", "validation exceeded its time limit", nil, eff.Versions)
	}
	return s.finish(ctx, r, Completed, "", "", added, eff.Versions)
}

// sensitive runs the sensitive-content check: built-in and policy path rules, content detectors and metadata.
func (s *Service) sensitive(ctx context.Context, r Run, plan buildplan.Plan, eff policy.Effective) Check {
	c := Check{ID: CheckSensitive, Category: "sensitive-content", Origin: "controller", Outcome: "errored", Tool: Tool{Name: "samuori-imagescan", Version: imagescan.RulesVersion}}
	if s.scanner == nil {
		c.Reason = "no image scanner is configured"
		return c
	}
	archive, err := s.blobs.File(r.ArtifactDigest)
	if err != nil {
		c.Reason = "the image is not in internal storage"
		return c
	}
	name := archive.Name()
	archive.Close()
	rep, degraded, err := s.scanner.Scan(ctx, name, eff.Sensitive.ExtraRules)
	if err != nil {
		c.Reason = "the scan did not complete: " + clip(err.Error(), 300)
		return c
	}
	if !rep.Complete {
		c.Reason = "the scan did not complete: " + clip(rep.Reason, 300)
		return c
	}
	outcomeOf := func(class string) string {
		if o := eff.Sensitive.Outcomes[class]; o != "" {
			return o
		}
		if o := imagescan.DefaultOutcomes[class]; o != "" {
			return o
		}
		return "block"
	}
	excs, err := s.exceptions(ctx, `project_id = ? AND state = 'approved'`, r.ProjectID)
	if err != nil {
		c.Reason = "exceptions could not be read"
		return c
	}
	now := s.now()
	counts := map[string]int{}
	used := map[string]bool{}
	for _, f := range rep.Findings {
		ff := Finding{Rule: f.Rule, Class: f.Class, Path: f.Path, Layer: f.Layer, Size: f.Size, SecretID: f.SecretID, Outcome: outcomeOf(f.Class)}
		if ff.Outcome == "require-exception" {
			for _, e := range excs {
				if e.covers(ff, now) {
					ff.Outcome, ff.ExceptionID = "excepted", e.ID
					used[e.ID] = true
					break
				}
			}
		}
		for _, e := range plan.Validation.Expected {
			if under(ff.Path, e.Path) {
				ff.Expected = clip(e.Reason, 300)
			}
		}
		counts[ff.Outcome]++
		c.Findings = append(c.Findings, ff)
	}
	switch {
	case counts["block"] > 0:
		c.Outcome, c.Reason = "blocked", fmt.Sprintf("%d finding(s) block publication", counts["block"])
	case counts["require-exception"] > 0:
		c.Outcome, c.Reason = "blocked", fmt.Sprintf("%d finding(s) need an approved exception", counts["require-exception"])
	case counts["excepted"] > 0:
		c.Outcome, c.Reason = "passed", fmt.Sprintf("%d finding(s) allowed by %d approved exception(s); the image is restricted", counts["excepted"], len(used))
	case counts["warn"] > 0:
		c.Outcome, c.Reason = "passed", fmt.Sprintf("%d warning(s); no blocking findings in %d layer(s)", counts["warn"], rep.Layers)
	default:
		c.Outcome, c.Reason = "passed", fmt.Sprintf("no sensitive content found in %d layer(s) and %d file(s)", rep.Layers, rep.Entries)
	}
	if rep.Truncated > 0 {
		c.Reason += fmt.Sprintf(" (%d more findings not listed)", rep.Truncated)
	}
	if degraded {
		c.Reason += "; scanned without the Linux sandbox (development only)"
	}
	return c
}

// under reports whether name lies at or below the declared path.
func under(name, declared string) bool {
	d := strings.Trim(path.Clean("/"+declared), "/")
	return d != "" && (name == d || strings.HasPrefix(name, d+"/"))
}

func (s *Service) sbomCheck(ctx context.Context, r Run) Check {
	c := Check{ID: CheckSBOM, Category: "sbom", Origin: "controller", Outcome: "errored"}
	if s.sbom == nil {
		c.Outcome, c.Reason = "skipped", ErrNoTool.Error()
		return c
	}
	archive, err := s.blobs.File(r.ArtifactDigest)
	if err != nil {
		c.Reason = "the image is not in internal storage"
		return c
	}
	name := archive.Name()
	archive.Close()
	doc, tool, err := s.sbom.Generate(ctx, name)
	c.Tool = tool
	switch {
	case errors.Is(err, ErrNoTool):
		c.Outcome, c.Reason = "skipped", err.Error()
		return c
	case err != nil:
		c.Reason = "SBOM generation failed: " + clip(err.Error(), 300)
		return c
	}
	digest, _, err := s.blobs.PutClass(storage.ClassReport, strings.NewReader(string(doc)), 256<<20)
	if err != nil {
		c.Reason = "the SBOM could not be stored"
		return c
	}
	c.Outcome, c.Reason, c.OutputDigest = "passed", "SPDX document generated", digest
	return c
}

// required decides which checks must pass for the run's verdict.
func required(c Check, plan buildplan.Plan, eff policy.Effective) bool {
	switch {
	case c.ID == CheckSensitive:
		return true
	case c.ID == "build-secret-canary":
		return true
	case strings.HasPrefix(c.ID, "runtime-"):
		return eff.Validation.RequireRuntimeTest && plan.Validation.Profile != "" && plan.Validation.Profile != buildplan.ProfileNone
	case c.ID == CheckSBOM:
		return eff.Validation.RequireSBOM || plan.Validation.SBOM
	case c.ID == CheckVulnerabilities:
		return eff.Validation.RequireVulnerabilityScan
	}
	return false
}

// expected lists the worker checks the plan implies; a missing one is recorded as errored.
func expected(plan buildplan.Plan) []Check {
	var out []Check
	for _, s := range plan.Secrets {
		if s.BindingID != "" {
			out = append(out, Check{ID: "build-secret-canary", Category: "sensitive-content"})
			break
		}
	}
	if p := plan.Validation.Profile; p != "" && p != buildplan.ProfileNone {
		out = append(out, Check{ID: "runtime-" + p, Category: "test"})
	}
	return out
}

// Verdict applies the lifecycle rules: blocked before failed before incomplete; passed only when every required
// check passed.
func Verdict(checks []Check) string {
	v := "passed"
	rank := map[string]int{"passed": 0, "incomplete": 1, "failed": 2, "blocked": 3}
	for _, c := range checks {
		if !c.Required {
			continue
		}
		next := map[string]string{"passed": "passed", "failed": "failed", "blocked": "blocked", "skipped": "incomplete", "errored": "incomplete"}[c.Outcome]
		if next == "" {
			next = "incomplete"
		}
		if rank[next] > rank[v] {
			v = next
		}
	}
	return v
}

func (s *Service) finish(ctx context.Context, r Run, state, verdict, reason string, added []Check, versions map[string]string) error {
	_, rawPlan, err := s.load(ctx, r.ID)
	if err != nil {
		return err
	}
	plan, _ := buildplan.Decode(rawPlan)
	eff, err := s.policies.Effective(ctx, r.OrgID)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range r.Checks {
		have[c.ID] = true
	}
	for _, c := range added {
		have[c.ID] = true
	}
	for _, e := range expected(plan) {
		if !have[e.ID] {
			added = append(added, Check{ID: e.ID, Category: e.Category, Origin: "worker", Outcome: "errored", Reason: "the worker did not report this check"})
		}
	}
	all := append(append([]Check{}, r.Checks...), added...)
	for i := range all {
		all[i].Required = required(all[i], plan, eff)
	}
	if state == Completed {
		verdict = Verdict(all)
	}
	r.Classification = "standard"
	for _, c := range all {
		for _, f := range c.Findings {
			if f.Outcome == "excepted" {
				r.Classification = "restricted"
			}
		}
	}
	rawVersions, _ := json.Marshal(versions)
	now := s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE validation_runs SET state = ?, verdict = ?, reason = ?, policy_versions = ?, finished_at = ?, classification = ? WHERE id = ? AND state = 'running' AND claim = ?`),
			state, nullable(verdict), nullable(reason), string(rawVersions), now, r.Classification, r.ID, r.claim)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrSuperseded
		}
		for _, c := range added {
			if err := s.insertCheck(ctx, tx, r.ID, c); err != nil {
				return err
			}
		}
		for _, c := range all {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE check_results SET required = ? WHERE validation_run_id = ? AND check_id = ?`), c.Required, r.ID, c.ID); err != nil {
				return err
			}
		}
		r.State, r.Verdict = state, verdict
		if s.onDone != nil {
			if err := s.onDone(ctx, tx, r); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{OrgID: r.OrgID, Kind: authz.System}, Action: "validation.complete", ResourceType: "validation", ResourceID: r.ID,
			Outcome: state, Reason: strings.TrimSpace(verdict + " " + reason)})
	})
	if err == nil {
		s.wake()
	}
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const selectRun = `SELECT id, org_id, project_id, artifact_id, artifact_digest, COALESCE(build_run_id, ''), COALESCE(source_run_id, ''), trigger_kind, state, COALESCE(verdict, ''), COALESCE(reason, ''), plan, created_at, started_at, finished_at, classification FROM validation_runs`

func (s *Service) load(ctx context.Context, id string) (Run, []byte, error) {
	var r Run
	var plan string
	var started, finished sql.NullTime
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectRun+` WHERE id = ?`), id).Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.ArtifactID, &r.ArtifactDigest, &r.BuildRunID, &r.SourceRunID,
		&r.Trigger, &r.State, &r.Verdict, &r.Reason, &plan, &r.CreatedAt, &started, &finished, &r.Classification)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, nil, ErrNotFound
	}
	if err != nil {
		return Run{}, nil, err
	}
	if started.Valid {
		r.StartedAt = &started.Time
	}
	if finished.Valid {
		r.FinishedAt = &finished.Time
	}
	if r.Checks, err = s.checks(ctx, id); err != nil {
		return Run{}, nil, err
	}
	return r, []byte(plan), nil
}

func (s *Service) checks(ctx context.Context, runID string) ([]Check, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT check_id, category, origin, required, outcome, reason, tool, findings, COALESCE(output_digest, ''), vulnerabilities FROM check_results WHERE validation_run_id = ? ORDER BY category, check_id`), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Check{}
	for rows.Next() {
		var c Check
		var tool, findings, vulns string
		if err := rows.Scan(&c.ID, &c.Category, &c.Origin, &c.Required, &c.Outcome, &c.Reason, &tool, &findings, &c.OutputDigest, &vulns); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tool), &c.Tool)
		_ = json.Unmarshal([]byte(findings), &c.Findings)
		_ = json.Unmarshal([]byte(vulns), &c.Vulnerabilities)
		out = append(out, c)
	}
	return out, rows.Err()
}

// visible checks the caller may read the project; other projects' validations are reported as not found.
func (s *Service) visible(ctx context.Context, p authz.Principal, roles []authz.Role, orgID, projectID string) error {
	if orgID != p.OrgID {
		return ErrNotFound
	}
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		if errors.Is(err, tenancy.ErrForbidden) || errors.Is(err, tenancy.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (s *Service) Get(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (Run, error) {
	r, _, err := s.load(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if err := s.visible(ctx, p, roles, r.OrgID, r.ProjectID); err != nil {
		return Run{}, err
	}
	return r, nil
}

type Artifact struct {
	ID, Digest, BuildRunID, ProjectID, OrgID string
	Size                                     int64
	CreatedAt                                time.Time
}

func (s *Service) Artifact(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (Artifact, error) {
	var a Artifact
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT a.id, a.digest, a.run_id, r.project_id, a.org_id, a.size, a.created_at FROM build_artifacts a JOIN build_runs r ON r.id = a.run_id WHERE a.id = ? AND a.kind = 'image'
		AND a.run_id NOT IN (SELECT run_id FROM build_run_deletions)`), id).
		Scan(&a.ID, &a.Digest, &a.BuildRunID, &a.ProjectID, &a.OrgID, &a.Size, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	if err := s.visible(ctx, p, roles, a.OrgID, a.ProjectID); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

func (s *Service) List(ctx context.Context, p authz.Principal, roles []authz.Role, artifactID string) ([]Run, error) {
	if _, err := s.Artifact(ctx, p, roles, artifactID); err != nil {
		return nil, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id FROM validation_runs WHERE artifact_id = ? ORDER BY created_at DESC LIMIT 50`), artifactID)
	if err != nil {
		return nil, err
	}
	var list []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, id)
	}
	rows.Close()
	out := []Run{}
	for _, id := range list {
		r, _, err := s.load(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Latest returns the newest validation of the artifact a build produced, if any.
func (s *Service) Latest(ctx context.Context, buildRunID string) (Run, bool, error) {
	var id string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT v.id FROM validation_runs v JOIN build_artifacts a ON a.id = v.artifact_id WHERE a.run_id = ? ORDER BY v.created_at DESC LIMIT 1`), buildRunID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	r, _, err := s.load(ctx, id)
	return r, err == nil, err
}

var rescanRoles = []authz.Role{authz.Maintainer, authz.SecurityReviewer, authz.Administrator}

// Rescan starts a new ValidationRun for an artifact. Worker checks cannot be repeated without the build
// environment, so they are carried over from the build-time validation and labeled as such.
func (s *Service) Rescan(ctx context.Context, p authz.Principal, roles []authz.Role, artifactID, key string) (Run, error) {
	a, err := s.Artifact(ctx, p, roles, artifactID)
	if err != nil {
		return Run{}, err
	}
	var team string
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), a.ProjectID).Scan(&team); err != nil {
		return Run{}, err
	}
	if !authz.Can(p, rescanRoles, authz.Resource{OrgID: a.OrgID, TeamID: team, ProjectID: a.ProjectID}) {
		return Run{}, ErrForbidden
	}
	return s.revalidate(ctx, p, roles, a, "rescan", key)
}

// Revalidate starts a promotion revalidation of an image the caller can see: the controller checks run again
// against current policy and exceptions, with the build-time worker results carried over (ADR 0003).
// ByID returns a validation run with its checks, for controller-internal decisions such as promotion gates.
func (s *Service) ByID(ctx context.Context, id string) (Run, error) {
	r, _, err := s.load(ctx, id)
	if err != nil {
		return Run{}, err
	}
	r.Checks, err = s.checks(ctx, id)
	return r, err
}

func (s *Service) Revalidate(ctx context.Context, p authz.Principal, roles []authz.Role, artifactID, key string) (Run, error) {
	a, err := s.Artifact(ctx, p, roles, artifactID)
	if err != nil {
		return Run{}, err
	}
	return s.revalidate(ctx, p, roles, a, "promotion", key)
}

func (s *Service) revalidate(ctx context.Context, p authz.Principal, roles []authz.Role, a Artifact, trigger, key string) (Run, error) {
	artifactID := a.ID
	var existing string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM validation_runs WHERE requested_by = ? AND idempotency_key = ?`), p.ID, key).Scan(&existing)
	if err == nil {
		return s.Get(ctx, p, roles, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Run{}, err
	}
	var source, plan string
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id, plan FROM validation_runs WHERE artifact_id = ? AND trigger_kind = 'build' ORDER BY created_at LIMIT 1`), artifactID).Scan(&source, &plan); err != nil {
		return Run{}, err
	}
	carried, err := s.checks(ctx, source)
	if err != nil {
		return Run{}, err
	}
	id, now := ids.New(), s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tenancy.RequireActive(ctx, s.db, tx, a.ProjectID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO validation_runs (id, org_id, project_id, artifact_id, artifact_digest, build_run_id, source_run_id, trigger_kind, state, plan, policy_versions, requested_by, idempotency_key, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, '{}', ?, ?, ?)`), id, a.OrgID, a.ProjectID, a.ID, a.Digest, a.BuildRunID, source, trigger, plan, p.ID, key, now); err != nil {
			return err
		}
		for _, c := range carried {
			if c.Origin != "worker" {
				continue
			}
			c.Reason = clip("From the build-time validation: "+c.Reason, 1024)
			if err := s.insertCheck(ctx, tx, id, c); err != nil {
				return err
			}
		}
		payload, _ := json.Marshal(map[string]string{"validationId": id})
		if err := s.db.EnqueueOutbox(ctx, tx, ids.New(), SubjectPending, payload); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "validation." + trigger, ResourceType: "artifact", ResourceID: a.ID, Outcome: "accepted"})
	})
	if err != nil {
		return Run{}, err
	}
	s.wake()
	return s.Get(ctx, p, roles, id)
}
