package kubeconvert

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"samuori.invalid/samuori/internal/convertjob"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/sources"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
	"samuori.invalid/samuori/internal/workers"
)

// Channel is the worker connection conversions are delivered over, shared with builds.
type Channel interface {
	Connected(workerID string) bool
	Send(workerID string, m *workerv1.ControllerMessage) error
}

// Scheduler leases conversion runs to eligible workers with the fencing used for builds and implements the
// conversion calls of the worker protocol (workers.Conversions).
type Scheduler struct {
	s       *Service
	workers *workers.Store
	ch      Channel
	log     *slog.Logger
}

func NewScheduler(s *Service, w *workers.Store, ch Channel, log *slog.Logger) *Scheduler {
	return &Scheduler{s: s, workers: w, ch: ch, log: log}
}

// Limits for one conversion environment; the wall clock adds the daemon's start to the tool's own limit.
var Limits = isolation.Limits{VCPUs: 1, MemoryBytes: 1 << 30, DiskBytes: 2 << 30}

type leased struct {
	id, conversionID, orgID, projectID, provider, version, state, attemptID, workerID, jobDigest, profile string
	generation                                                                                            int64
	attempts, maxAttempts                                                                                 int
	deadline                                                                                              *time.Time
}

const selectLeased = `SELECT r.id, r.conversion_id, r.org_id, c.project_id, c.provider, r.provider_version, r.state, COALESCE(r.current_attempt_id, ''), COALESCE(r.lease_worker_id, ''), r.job_digest,
	COALESCE(r.isolation_profile, ''), r.generation, r.attempts_used, r.max_attempts, r.lease_deadline FROM kube_conversion_runs r JOIN kube_conversions c ON c.id = r.conversion_id`

func scanLeased(row interface{ Scan(...any) error }) (leased, error) {
	var l leased
	var deadline sql.NullTime
	err := row.Scan(&l.id, &l.conversionID, &l.orgID, &l.projectID, &l.provider, &l.version, &l.state, &l.attemptID, &l.workerID, &l.jobDigest, &l.profile, &l.generation, &l.attempts, &l.maxAttempts, &deadline)
	if deadline.Valid {
		l.deadline = &deadline.Time
	}
	return l, err
}

type candidate struct {
	w    workers.Worker
	pool workers.Pool
	free int
}

// Dispatch expires lapsed leases and assigns queued runs to workers that report the approved provider version, in
// pools and projects the provider is enabled for, whose isolation is verified for untrusted input.
func (sc *Scheduler) Dispatch(ctx context.Context) (int, error) {
	if err := sc.expire(ctx); err != nil {
		return 0, err
	}
	rows, err := sc.s.db.SQL.QueryContext(ctx, sc.s.db.Rebind(selectLeased+` WHERE r.state = ? ORDER BY r.created_at LIMIT 200`), StateQueued)
	if err != nil {
		return 0, err
	}
	var queued []leased
	for rows.Next() {
		l, err := scanLeased(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		queued = append(queued, l)
	}
	rows.Close()
	byOrg := map[string][]candidate{}
	assigned := 0
	for _, run := range queued {
		cands, ok := byOrg[run.orgID]
		if !ok {
			if cands, err = sc.candidates(ctx, run.orgID); err != nil {
				return assigned, err
			}
			byOrg[run.orgID] = cands
		}
		cfg, err := sc.s.providers.Config(ctx, run.orgID, run.provider)
		if err != nil {
			return assigned, err
		}
		if !cfg.Allows("", run.projectID) || cfg.Version != run.version {
			if err := sc.fail(ctx, run, "policy", fmt.Sprintf("%s %s is no longer approved for this project", run.provider, run.version)); err != nil {
				return assigned, err
			}
			continue
		}
		pick := -1
		for i, c := range cands {
			if c.free > 0 && Eligible(cfg, run.provider, run.projectID, c.pool, c.w, sc.s.now()) == nil && (pick < 0 || c.free > cands[pick].free) {
				pick = i
			}
		}
		if pick < 0 {
			continue
		}
		if err := sc.assign(ctx, run, cands[pick]); err != nil {
			if errors.Is(err, errNoCapacity) {
				cands[pick].free = 0
				continue
			}
			if errors.Is(err, errLostRace) {
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

// Eligible decides whether a worker may run a provider for a project. The input is always untrusted, so the worker
// must pass the same isolation checks as an untrusted build (ADR 0027), report the approved provider version, and
// have an approved checksum for its architecture.
func Eligible(cfg providers.Config, provider, projectID string, pool workers.Pool, w workers.Worker, now time.Time) error {
	switch {
	case !cfg.Allows(pool.ID, projectID):
		return errors.New("provider-not-enabled-for-pool")
	case w.Capabilities.Providers[provider] == "" || w.Capabilities.Providers[provider] != cfg.Version:
		return errors.New("provider-version-missing")
	}
	arch := ""
	if len(w.Capabilities.Architectures) > 0 {
		arch = w.Capabilities.Architectures[0]
	}
	if cfg.Artifacts["binary-"+arch] == "" {
		return errors.New("no-approved-checksum-for-architecture")
	}
	ok, why := isolation.Eligible(pool.State(), w.State(now), isolation.Requirement{Untrusted: true, Platforms: []string{"linux/" + arch}}, now)
	if !ok {
		return errors.New(strings.Join(why, ","))
	}
	return nil
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
		if w.PoolID == "" || !sc.ch.Connected(w.ID) {
			continue
		}
		var minor, busy int
		if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT protocol_minor FROM workers WHERE id = ?`), w.ID).Scan(&minor); err != nil {
			return nil, err
		}
		if minor < workers.ConversionMinor {
			continue
		}
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

// assign creates a lease with a compare-and-swap on the run's generation; slots are counted again under a lock on
// the worker, together with its builds (one build or conversion per daemon, OD-16).
func (sc *Scheduler) assign(ctx context.Context, run leased, c candidate) error {
	now := sc.s.now().UTC()
	deadline := now.Add(sc.s.LeaseTTL)
	gen := run.generation + 1
	attempt := ids.New()
	return sc.s.db.InTx(ctx, func(tx *sql.Tx) error {
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
		res, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE kube_conversion_runs SET state = ?, generation = ?, current_attempt_id = ?, lease_worker_id = ?, lease_pool_id = ?, lease_deadline = ?, isolation_profile = ?, attempts_used = attempts_used + 1, updated_at = ?
			WHERE id = ? AND state = ? AND generation = ?`), StateAssigned, gen, attempt, c.w.ID, c.pool.ID, deadline, c.pool.IsolationProfile, now, run.id, StateQueued, run.generation)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errLostRace
		}
		if _, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE kube_conversions SET state = ?, updated_at = ? WHERE id = ?`), StateAssigned, now, run.conversionID); err != nil {
			return err
		}
		return sc.s.db.EnqueueOutbox(ctx, tx, ids.New(), SubjectAssign, wake(run.id, gen))
	})
}

func wake(runID string, gen int64) []byte {
	b, _ := json.Marshal(wakePayload{RunID: runID, Generation: gen})
	return b
}

type wakePayload struct {
	RunID      string `json:"runId"`
	Generation int64  `json:"generation"`
}

// Deliver handles the conversion subjects; it acts only on the generation the message names.
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
		run, err := scanLeased(sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(selectLeased+` WHERE r.id = ?`), p.RunID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if run.state != StateAssigned || run.generation != p.Generation || run.deadline == nil {
			return nil
		}
		wall := time.Duration(sc.timeout(ctx, run)+120) * time.Second
		m := &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_Conversion{Conversion: &workerv1.ConversionAssignment{
			Lease: &workerv1.LeaseRef{AttemptId: run.attemptID, FencingGeneration: uint64(run.generation)}, ConversionId: run.conversionID, JobDigest: run.jobDigest,
			IsolationProfile: run.profile, LeaseExpiresAt: timestamppb.New(*run.deadline),
			Limits: &workerv1.ResourceLimits{Vcpus: Limits.VCPUs, MemoryBytes: Limits.MemoryBytes, DiskBytes: Limits.DiskBytes, WallClock: durationpb.New(wall)},
		}}}
		err = sc.ch.Send(run.workerID, m)
		if errors.Is(err, workers.ErrNotConnected) {
			return nil
		}
		return err
	case SubjectCancel:
		run, err := scanLeased(sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(selectLeased+` WHERE r.id = ?`), p.RunID))
		if err != nil || run.workerID == "" {
			return nil
		}
		_ = sc.ch.Send(run.workerID, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_Cancel{Cancel: &workerv1.Cancel{Lease: &workerv1.LeaseRef{AttemptId: run.attemptID, FencingGeneration: uint64(p.Generation)}, Reason: "cancelled"}}})
	}
	return nil
}

func (sc *Scheduler) timeout(ctx context.Context, run leased) int {
	var raw string
	if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT job FROM kube_conversion_runs WHERE id = ?`), run.id).Scan(&raw); err != nil {
		return convertjob.DefaultTimeout
	}
	var j convertjob.Job
	_ = json.Unmarshal([]byte(raw), &j)
	return max(j.TimeoutSeconds, 1)
}

// Resend wakes delivery again for every run leased to a worker that (re)connected; the worker ignores duplicates.
func (sc *Scheduler) Resend(ctx context.Context, workerID string) error {
	rows, err := sc.s.db.SQL.QueryContext(ctx, sc.s.db.Rebind(`SELECT id, generation FROM kube_conversion_runs WHERE state = ? AND lease_worker_id = ?`), StateAssigned, workerID)
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

// current loads the run a lease refers to and checks that it is current and held by this worker.
func (sc *Scheduler) current(ctx context.Context, workerID string, l *workerv1.LeaseRef) (leased, error) {
	if l == nil || l.GetAttemptId() == "" {
		return leased{}, workers.ErrStaleLease
	}
	run, err := scanLeased(sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(selectLeased+` WHERE r.current_attempt_id = ?`), l.GetAttemptId()))
	if errors.Is(err, sql.ErrNoRows) {
		return leased{}, workers.ErrStaleLease
	}
	if err != nil {
		return leased{}, err
	}
	if uint64(run.generation) != l.GetFencingGeneration() || run.workerID != workerID || (run.state != StateAssigned && run.state != StateRunning) {
		return leased{}, workers.ErrStaleLease
	}
	return run, nil
}

func (sc *Scheduler) Renew(ctx context.Context, workerID string, leases []*workerv1.LeaseRef) ([]*workerv1.LeaseRef, error) {
	var abandoned []*workerv1.LeaseRef
	deadline := sc.s.now().UTC().Add(sc.s.LeaseTTL)
	for _, l := range leases {
		run, err := sc.current(ctx, workerID, l)
		if errors.Is(err, workers.ErrStaleLease) {
			abandoned = append(abandoned, l)
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := sc.s.db.SQL.ExecContext(ctx, sc.s.db.Rebind(`UPDATE kube_conversion_runs SET lease_deadline = ? WHERE id = ? AND generation = ?`), deadline, run.id, run.generation); err != nil {
			return nil, err
		}
	}
	return abandoned, nil
}

// Job returns the job only to the worker holding the current lease.
func (sc *Scheduler) Job(ctx context.Context, workerID string, l *workerv1.LeaseRef) (string, []byte, error) {
	run, err := sc.current(ctx, workerID, l)
	if err != nil {
		return "", nil, err
	}
	var raw string
	if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT job FROM kube_conversion_runs WHERE id = ?`), run.id).Scan(&raw); err != nil {
		return "", nil, err
	}
	return run.jobDigest, []byte(raw), nil
}

const inputChunk = 256 << 10

// Input streams the run's input to the worker holding its lease: the canonical manifest, then each file. The
// worker checks the manifest against the job's input digest and every file against the manifest.
func (sc *Scheduler) Input(ctx context.Context, workerID string, l *workerv1.LeaseRef, send func(*workerv1.SourceChunk) error) error {
	run, err := sc.current(ctx, workerID, l)
	if err != nil {
		return err
	}
	var rawInput, rawInline string
	if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT c.input, r.inline_files FROM kube_conversion_runs r JOIN kube_conversions c ON c.id = r.conversion_id WHERE r.id = ?`), run.id).Scan(&rawInput, &rawInline); err != nil {
		return err
	}
	var in Input
	inline := map[string]string{}
	_ = json.Unmarshal([]byte(rawInput), &in)
	_ = json.Unmarshal([]byte(rawInline), &inline)
	var m sources.Manifest
	if len(inline) > 0 {
		m = inlineManifest(inline)
	} else {
		snap, err := sc.s.sources.SnapshotByID(ctx, in.SnapshotID)
		if err != nil {
			return err
		}
		if m, err = sc.s.sources.Manifest(snap); err != nil {
			return err
		}
	}
	if err := send(&workerv1.SourceChunk{Kind: &workerv1.SourceChunk_ManifestJson{ManifestJson: m.Canonical()}}); err != nil {
		return err
	}
	for _, f := range m.Files {
		if err := send(&workerv1.SourceChunk{Kind: &workerv1.SourceChunk_File{File: &workerv1.SourceFileStart{Path: f.Path, Digest: f.Digest, Size: uint64(f.Size), Executable: f.Executable}}}); err != nil {
			return err
		}
		if c, ok := inline[f.Path]; ok {
			for b := []byte(c); len(b) > 0; b = b[min(len(b), inputChunk):] {
				if err := send(&workerv1.SourceChunk{Kind: &workerv1.SourceChunk_Data{Data: b[:min(len(b), inputChunk)]}}); err != nil {
					return err
				}
			}
			continue
		}
		rc, err := sc.s.blobs.Open(f.Digest)
		if err != nil {
			return err
		}
		buf := make([]byte, inputChunk)
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

var cleanups = map[workerv1.CleanupOutcome]string{workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED: "destroyed", workerv1.CleanupOutcome_CLEANUP_OUTCOME_FAILED_QUARANTINED: "failed-quarantined"}

// Report applies a worker's report under the lease check. A completed run's files are untrusted: they are checked,
// interpreted by the provider and post-processed before a revision is stored.
func (sc *Scheduler) Report(ctx context.Context, workerID string, rep *workerv1.ConversionReport) error {
	run, err := sc.current(ctx, workerID, rep.GetLease())
	if err != nil {
		return err
	}
	cleanup := cleanups[rep.GetCleanup()]
	if cleanup == "" {
		cleanup = "pending"
	}
	switch rep.GetState() {
	case workerv1.ConversionState_CONVERSION_STATE_RUNNING:
		return sc.transition(ctx, run, StateRunning, "", "", cleanup, false)
	case workerv1.ConversionState_CONVERSION_STATE_FAILED:
		class := rep.GetFailureClass()
		if class == "infrastructure" && run.attempts < run.maxAttempts {
			return sc.requeue(ctx, run, clip(rep.GetDetail()), cleanup)
		}
		state := StateFailed
		if class == "timeout" {
			state = StateTimedOut
		}
		return sc.transition(ctx, run, state, or(class, "application"), clip(rep.GetDetail()), cleanup, true)
	case workerv1.ConversionState_CONVERSION_STATE_COMPLETED:
		return sc.complete(ctx, run, workerID, rep, cleanup)
	}
	return errors.New("unknown conversion state")
}

func clip(s string) string {
	if len(s) > 1000 {
		return s[:1000]
	}
	return s
}

func (sc *Scheduler) transition(ctx context.Context, run leased, state, class, detail, cleanup string, ended bool) error {
	now := sc.s.now().UTC()
	return sc.s.db.InTx(ctx, func(tx *sql.Tx) error {
		q, args := `UPDATE kube_conversion_runs SET state = ?, failure_class = ?, detail = ?, cleanup = ?, updated_at = ?`, []any{state, nullable(class), nullable(detail), cleanup, now}
		if ended {
			q, args = q+`, ended_at = ?, lease_deadline = NULL`, append(args, now)
		}
		res, err := tx.ExecContext(ctx, sc.s.db.Rebind(q+` WHERE id = ? AND generation = ?`), append(args, run.id, run.generation)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return workers.ErrStaleLease
		}
		_, err = tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE kube_conversions SET state = ?, updated_at = ? WHERE id = ?`), state, now, run.conversionID)
		return err
	})
}

func (sc *Scheduler) requeue(ctx context.Context, run leased, detail, cleanup string) error {
	now := sc.s.now().UTC()
	err := sc.s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE kube_conversion_runs SET state = ?, detail = ?, cleanup = ?, lease_worker_id = NULL, lease_deadline = NULL, updated_at = ? WHERE id = ? AND generation = ?`),
			StateQueued, nullable(detail), cleanup, now, run.id, run.generation)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return workers.ErrStaleLease
		}
		if _, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE kube_conversions SET state = ?, updated_at = ? WHERE id = ?`), StateQueued, now, run.conversionID); err != nil {
			return err
		}
		return sc.s.db.EnqueueOutbox(ctx, tx, ids.New(), SubjectQueued, wake(run.id, run.generation))
	})
	if err == nil {
		sc.s.wake()
	}
	return err
}

func (sc *Scheduler) fail(ctx context.Context, run leased, class, detail string) error {
	now := sc.s.now().UTC()
	return sc.s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE kube_conversion_runs SET state = ?, failure_class = ?, detail = ?, ended_at = ?, lease_deadline = NULL, updated_at = ? WHERE id = ? AND generation = ?`),
			StateFailed, class, detail, now, now, run.id, run.generation); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE kube_conversions SET state = ?, updated_at = ? WHERE id = ?`), StateFailed, now, run.conversionID)
		return err
	})
}

// expire returns runs whose worker stopped renewing to the queue, or fails them after the last attempt.
func (sc *Scheduler) expire(ctx context.Context) error {
	rows, err := sc.s.db.SQL.QueryContext(ctx, sc.s.db.Rebind(selectLeased+` WHERE r.state IN (?, ?) AND r.lease_deadline < ?`), StateAssigned, StateRunning, sc.s.now().UTC())
	if err != nil {
		return err
	}
	var lapsed []leased
	for rows.Next() {
		l, err := scanLeased(rows)
		if err != nil {
			rows.Close()
			return err
		}
		lapsed = append(lapsed, l)
	}
	rows.Close()
	for _, run := range lapsed {
		var err error
		if run.attempts < run.maxAttempts {
			err = sc.requeue(ctx, run, "the worker stopped renewing its lease", "pending")
		} else {
			err = sc.transition(ctx, run, StateFailed, "infrastructure", "the worker stopped renewing its lease on the last attempt", "pending", true)
		}
		if err != nil && !errors.Is(err, workers.ErrStaleLease) {
			return err
		}
	}
	return nil
}

// checkFiles bounds the reported files and their paths before anything parses them.
func checkFiles(files []*workerv1.ConversionFile, outputDir string) (map[string][]byte, error) {
	if len(files) > convertjob.MaxOutputFiles {
		return nil, fmt.Errorf("the tool wrote %d files, more than %d", len(files), convertjob.MaxOutputFiles)
	}
	out := map[string][]byte{}
	total := 0
	for _, f := range files {
		p := f.GetPath()
		if !cleanBundlePath(p) || !strings.HasPrefix(p, outputDir+"/") {
			return nil, fmt.Errorf("the tool wrote %q outside its output directory", p)
		}
		if len(f.GetContent()) > convertjob.MaxFileBytes {
			return nil, fmt.Errorf("%s is larger than %d KiB", p, convertjob.MaxFileBytes>>10)
		}
		if total += len(f.GetContent()); total > convertjob.MaxOutputBytes {
			return nil, fmt.Errorf("the output is larger than %d MiB", convertjob.MaxOutputBytes>>20)
		}
		rel := strings.TrimPrefix(p, outputDir+"/")
		if _, dup := out[rel]; dup {
			return nil, fmt.Errorf("%s was reported twice", p)
		}
		out[rel] = f.GetContent()
	}
	return out, nil
}

func (sc *Scheduler) complete(ctx context.Context, run leased, workerID string, rep *workerv1.ConversionReport, cleanup string) error {
	var rawJob, rawCtx, rawOpts, rawAnswers, name, actor string
	var input string
	if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT r.job, r.context, r.options, r.answers, c.name, c.input, r.created_by FROM kube_conversion_runs r JOIN kube_conversions c ON c.id = r.conversion_id WHERE r.id = ?`), run.id).
		Scan(&rawJob, &rawCtx, &rawOpts, &rawAnswers, &name, &input, &actor); err != nil {
		return err
	}
	var job convertjob.Job
	var rc runContext
	var opts Options
	var in Input
	answers := map[string]string{}
	_ = json.Unmarshal([]byte(rawJob), &job)
	_ = json.Unmarshal([]byte(rawCtx), &rc)
	_ = json.Unmarshal([]byte(rawOpts), &opts)
	_ = json.Unmarshal([]byte(rawAnswers), &answers)
	_ = json.Unmarshal([]byte(input), &in)
	arch := ""
	for a, sum := range job.ToolSHA256 {
		if sum == rep.GetToolDigest() {
			arch = a
		}
	}
	if arch == "" {
		return sc.transition(ctx, run, StateFailed, "policy", "the worker reported a tool that is not the approved one", cleanup, true)
	}
	prov, ok := Lookup(run.provider)
	if !ok {
		return sc.transition(ctx, run, StateFailed, "infrastructure", "this controller cannot interpret "+run.provider, cleanup, true)
	}
	files, err := checkFiles(rep.GetFiles(), rc.Plan.OutputDir)
	if err != nil {
		return sc.transition(ctx, run, StateFailed, "application", err.Error(), cleanup, true)
	}
	pin := PlanInput{Name: name, Input: in, Options: opts, Answers: answers, Questions: rc.Questions, ComposeFile: rc.ComposeFile}
	interp, err := prov.Interpret(pin, rc.Plan, ToolOutput{ExitCode: int(rep.GetExitCode()), TimedOut: rep.GetTimedOut(), Stdout: string(rep.GetToolStdout()), Stderr: string(rep.GetToolStderr()), Files: files})
	if err != nil {
		detail := err.Error()
		var notes []string
		for _, f := range interp.Findings {
			notes = append(notes, f.Message)
		}
		if len(notes) > 0 {
			detail += " (" + strings.Join(notes, "; ") + ")"
		}
		state := StateFailed
		if rep.GetTimedOut() {
			state = StateTimedOut
		}
		return sc.transition(ctx, run, state, "application", clip(detail), cleanup, true)
	}
	out, findings := Process(PostContext{Name: name, Options: opts, Image: rc.Image}, interp.Files)
	findings = append(append(append([]Finding{}, rc.Notes...), interp.Findings...), findings...)
	lin := rc.Lineage
	lin.ToolDigest, lin.WorkerID, lin.IsolationProfile = rep.GetToolDigest(), workerID, run.profile
	questions, _ := json.Marshal(interp.Questions)
	state := StateSucceeded
	if len(unanswered(interp.Questions, answers)) > 0 {
		state = StateNeedsAnswers
	}
	now := sc.s.now().UTC()
	err = sc.s.db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT COALESCE(MAX(number), 0) + 1 FROM kube_conversion_revisions WHERE conversion_id = ?`), run.conversionID).Scan(&n); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE kube_conversion_runs SET state = ?, cleanup = ?, tool_digest = ?, questions = ?, revision = ?, ended_at = ?, lease_deadline = NULL, updated_at = ? WHERE id = ? AND generation = ?`),
			state, cleanup, rep.GetToolDigest(), string(questions), n, now, now, run.id, run.generation)
		if err != nil {
			return err
		}
		if c, _ := res.RowsAffected(); c != 1 {
			return workers.ErrStaleLease
		}
		rev := Revision{Number: n, RunID: run.id, Origin: OriginProvider, Findings: findings, Lineage: lin, Errors: countErrors(findings), CreatedBy: actor, CreatedAt: now, Files: out}
		if err := sc.s.insertRevision(ctx, tx, run.conversionID, rev); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE kube_conversions SET state = ?, current_revision = ?, updated_at = ? WHERE id = ?`), state, n, now, run.conversionID)
		return err
	})
	return err
}
