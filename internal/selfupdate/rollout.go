package selfupdate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
	"samuori.invalid/samuori/internal/workers"
)

const (
	controllerTimeout = 15 * time.Minute
	drainTimeout      = 2 * time.Hour
	workerTimeout     = 20 * time.Minute
	rollbackMinor     = 3
)

type Step struct {
	Seq                    int
	Kind, Target           string
	Name, State            string
	Detail                 string
	Wave                   int
	PoolID, Pool, Location string
	Arch                   string
	// LocalWorker is the id of the worker on a controller's host that this controller step drains, restarts and
	// self-tests after the controller (ADR 0046, OD-28); empty otherwise.
	LocalWorker string
	UpdatedAt   time.Time
}

func (st Step) done() bool { return st.State == "succeeded" || st.State == "skipped" }

func (st Step) active() bool { return !st.done() && st.State != "pending" && st.State != "failed" }

type Rollout struct {
	ID, Version, State string
	Reason, Detail     string
	CreatedBy          string
	CreatedAt          time.Time
	FinishedAt         *time.Time
	Steps              []Step
	WavePercent        int
	// PauseAfter is the wave after which the rollout pauses for an administrator to check it.
	PauseAfter  *int
	Paused      bool
	PauseReason string
}

// StartRequest is an administrator's Update.
type StartRequest struct {
	Version, Reason     string
	PauseAfterFirstWave bool
	WavePercent         int
	AllowDowngrade      bool
}

// Start runs the checks before a rollout and, if none fails, plans it (planSteps).
func (s *Service) Start(ctx context.Context, actor authz.Principal, req StartRequest, requestID string) (Rollout, error) {
	if !actor.IsOrgAdmin() {
		return Rollout{}, ErrForbidden
	}
	if req.WavePercent == 0 {
		req.WavePercent = DefaultWavePercent
	}
	if req.WavePercent < 1 || req.WavePercent > 100 {
		return Rollout{}, &Invalid{"the wave size is a percentage of each pool, from 1 to 100"}
	}
	pf, err := s.preflight(ctx, req.Version, req.AllowDowngrade, req.WavePercent)
	if errors.Is(err, ErrNotFound) {
		return Rollout{}, &Invalid{"upload release " + req.Version + " first"}
	}
	if err != nil {
		return Rollout{}, err
	}
	if !pf.Ready {
		var failed []string
		for _, c := range pf.Checks {
			if c.State == CheckFailed {
				failed = append(failed, c.Name+": "+c.Detail)
			}
		}
		return Rollout{}, &Invalid{"the checks before updating failed. " + strings.Join(failed, " ")}
	}
	now := s.now().UTC()
	r := Rollout{ID: ids.New(), Version: req.Version, State: "running", Reason: strings.TrimSpace(req.Reason), CreatedBy: actor.ID, CreatedAt: now, Steps: pf.Steps, WavePercent: req.WavePercent}
	if len(pf.Left) > 0 {
		r.Detail = "Not included, because they are not active, unassigned or their pool's isolation profile cannot run self-tests: " + strings.Join(pf.Left, ", ")
	}
	if req.PauseAfterFirstWave && req.WavePercent >= 100 {
		if n := len(r.Steps); n > 0 && r.Steps[n-1].Wave > 1 {
			first := 1
			r.PauseAfter = &first
		}
	} else if req.PauseAfterFirstWave {
		first, last := 0, 0
		for _, st := range r.Steps {
			if st.Kind == "worker" {
				if first == 0 {
					first = st.Wave
				}
				last = st.Wave
			}
		}
		if first > 0 && last > first {
			r.PauseAfter = &first
		}
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rollouts WHERE state = 'running'`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrRunning
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO rollouts (id, version, state, reason, detail, created_by, created_at, pause_after, wave_percent) VALUES (?, ?, 'running', ?, ?, ?, ?, ?, ?)`),
			r.ID, r.Version, r.Reason, clip(r.Detail), actor.ID, now, r.PauseAfter, r.WavePercent); err != nil {
			return err
		}
		for _, st := range r.Steps {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO rollout_steps (rollout_id, seq, kind, target, name, state, updated_at, wave, pool_id, pool, location, arch, local_worker_id) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?)`),
				r.ID, st.Seq, st.Kind, st.Target, st.Name, now, st.Wave, st.PoolID, st.Pool, st.Location, st.Arch, st.LocalWorker); err != nil {
				return err
			}
		}
		reason := r.Version
		if r.Reason != "" {
			reason += ": " + r.Reason
		}
		if req.AllowDowngrade {
			reason += " (downgrade allowed)"
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "rollout.start", ResourceType: "rollout", ResourceID: r.ID, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	return r, err
}

// Pause stops new hosts from starting; hosts already updating finish.
func (s *Service) Pause(ctx context.Context, actor authz.Principal, id, requestID string) (Rollout, error) {
	return s.control(ctx, actor, id, "rollout.pause", requestID, func(tx *sql.Tx, r Rollout) error {
		if r.Paused {
			return &Invalid{"the rollout is already paused"}
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE rollouts SET paused = ?, pause_reason = ? WHERE id = ?`), true, "paused by "+actor.ID, id)
		return err
	})
}

// Resume continues a paused rollout and retries its failed hosts, which went back to their previous release.
func (s *Service) Resume(ctx context.Context, actor authz.Principal, id, requestID string) (Rollout, error) {
	return s.control(ctx, actor, id, "rollout.resume", requestID, func(tx *sql.Tx, r Rollout) error {
		if !r.Paused {
			return &Invalid{"the rollout is not paused"}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE rollouts SET paused = ?, pause_reason = '', pause_after = NULL WHERE id = ?`), false, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE rollout_steps SET state = 'pending', detail = 'retrying after '||detail, updated_at = ? WHERE rollout_id = ? AND state = 'failed'`), s.now().UTC(), id)
		return err
	})
}

// Cancel stops a running rollout; a worker still draining goes back to active, and a host already installing
// PurgeHistory deletes every finished rollout and its steps; a running rollout stays. The audit log keeps its
// rollout.start records.
func (s *Service) PurgeHistory(ctx context.Context, actor authz.Principal, requestID string) (int, error) {
	if !actor.IsOrgAdmin() {
		return 0, ErrForbidden
	}
	var removed int
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rollout_steps WHERE rollout_id IN (SELECT id FROM rollouts WHERE state <> 'running')`); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM rollouts WHERE state <> 'running'`)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		removed = int(n)
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "rollout.purge", ResourceType: "rollout", Outcome: "succeeded", Reason: fmt.Sprintf("%d finished rollouts deleted", removed), RequestID: requestID})
	})
	return removed, err
}

// finishes on its own.
func (s *Service) Cancel(ctx context.Context, actor authz.Principal, id, requestID string) (Rollout, error) {
	r, err := s.control(ctx, actor, id, "rollout.cancel", requestID, func(tx *sql.Tx, r Rollout) error {
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE rollouts SET state = 'cancelled', paused = ?, finished_at = ? WHERE id = ? AND state = 'running'`), false, s.now().UTC(), id)
		return err
	})
	if err != nil {
		return Rollout{}, err
	}
	for _, st := range r.Steps {
		if st.Kind == "worker" && st.State == "draining" {
			s.reactivateDrained(ctx, st.Target)
		}
	}
	return r, nil
}

func (s *Service) control(ctx context.Context, actor authz.Principal, id, action, requestID string, fn func(*sql.Tx, Rollout) error) (Rollout, error) {
	if !actor.IsOrgAdmin() {
		return Rollout{}, ErrForbidden
	}
	r, err := s.rollout(ctx, `id = ?`, id)
	if err != nil {
		return Rollout{}, err
	}
	if r.State != "running" {
		return Rollout{}, &Invalid{"the rollout is " + r.State}
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := fn(tx, r); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: action, ResourceType: "rollout", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
	if err != nil {
		return Rollout{}, err
	}
	return s.rollout(ctx, `id = ?`, id)
}

// List returns the newest rollouts first, without their steps.
func (s *Service) List(ctx context.Context, actor authz.Principal, before string, limit int) ([]Rollout, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	q, args := `SELECT id FROM rollouts`, []any{}
	if before != "" {
		q, args = q+` WHERE `+db.NewestBefore("rollouts", "rollouts"), append(args, before, before, before)
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(q+` ORDER BY created_at DESC, id DESC LIMIT ?`), append(args, limit)...)
	if err != nil {
		return nil, err
	}
	var idList []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		idList = append(idList, id)
	}
	rows.Close()
	out := []Rollout{}
	for _, id := range idList {
		r, err := s.rollout(ctx, `id = ?`, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, actor authz.Principal, id string) (Rollout, error) {
	if !actor.IsOrgAdmin() {
		return Rollout{}, ErrForbidden
	}
	return s.rollout(ctx, `id = ?`, id)
}

func (s *Service) rollout(ctx context.Context, where string, args ...any) (Rollout, error) {
	var r Rollout
	var finished sql.NullTime
	var pause sql.NullInt64
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id, version, state, reason, detail, created_by, created_at, finished_at, pause_after, paused, pause_reason, wave_percent FROM rollouts WHERE `+where), args...).
		Scan(&r.ID, &r.Version, &r.State, &r.Reason, &r.Detail, &r.CreatedBy, &r.CreatedAt, &finished, &pause, &r.Paused, &r.PauseReason, &r.WavePercent)
	if errors.Is(err, sql.ErrNoRows) {
		return Rollout{}, ErrNotFound
	}
	if err != nil {
		return Rollout{}, err
	}
	if finished.Valid {
		r.FinishedAt = &finished.Time
	}
	if pause.Valid {
		p := int(pause.Int64)
		r.PauseAfter = &p
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT seq, kind, target, name, state, detail, updated_at, wave, pool_id, pool, location, arch, local_worker_id FROM rollout_steps WHERE rollout_id = ? ORDER BY seq`), r.ID)
	if err != nil {
		return Rollout{}, err
	}
	defer rows.Close()
	r.Steps = []Step{}
	for rows.Next() {
		var st Step
		if err := rows.Scan(&st.Seq, &st.Kind, &st.Target, &st.Name, &st.State, &st.Detail, &st.UpdatedAt, &st.Wave, &st.PoolID, &st.Pool, &st.Location, &st.Arch, &st.LocalWorker); err != nil {
			return Rollout{}, err
		}
		r.Steps = append(r.Steps, st)
	}
	return r, rows.Err()
}

func (s *Service) setStep(ctx context.Context, rolloutID string, seq int, state, detail string) error {
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE rollout_steps SET state = ?, detail = ?, updated_at = ? WHERE rollout_id = ? AND seq = ?`),
		state, clip(detail), s.now().UTC(), rolloutID, seq)
	return err
}

func (s *Service) pause(ctx context.Context, id, reason string) error {
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE rollouts SET paused = ?, pause_reason = ?, pause_after = NULL WHERE id = ? AND state = 'running'`), true, clip(reason), id)
	if err == nil {
		s.log.Warn("rollout paused", "rollout", id, "reason", reason)
	}
	return err
}

func (s *Service) finish(ctx context.Context, id, state, detail string) error {
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE rollouts SET state = ?, detail = ?, finished_at = ? WHERE id = ? AND state = 'running'`), state, clip(detail), s.now().UTC(), id)
	if err == nil {
		s.log.Info("rollout finished", "rollout", id, "state", state, "detail", detail)
	}
	return err
}

// advance moves the running rollout on. Hosts already updating always continue; new hosts start only while the
// rollout is not paused: one controller at a time with the leader last, then, once every controller is done, the
// lowest unfinished wave of each pool. A failed host pauses the rollout.
func (s *Service) advance(ctx context.Context) error {
	r, err := s.rollout(ctx, `state = 'running' ORDER BY created_at LIMIT 1`)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range r.Steps {
		if r.Steps[i].active() {
			if err := s.advanceStep(ctx, r, &r.Steps[i]); err != nil {
				return err
			}
		}
	}
	if r, err = s.rollout(ctx, `id = ?`, r.ID); err != nil {
		return err
	}
	allDone := true
	for _, st := range r.Steps {
		if !st.done() {
			allDone = false
		}
	}
	if allDone {
		return s.finish(ctx, r.ID, "succeeded", r.Detail)
	}
	for _, st := range r.Steps {
		if st.State == "failed" {
			if st.Kind == "worker" {
				s.reactivateDrained(ctx, st.Target)
			} else if st.LocalWorker != "" {
				s.reactivateDrained(ctx, st.LocalWorker)
			}
			if !r.Paused {
				return s.pause(ctx, r.ID, st.Name+" failed and is on its previous release: "+st.Detail)
			}
		}
	}
	if r.Paused {
		return nil
	}
	if r.PauseAfter != nil && wavesDone(r.Steps, *r.PauseAfter) {
		if r.WavePercent >= 100 {
			return s.pause(ctx, r.ID, "the first controller is updated; check it, then resume to update everything else at once")
		}
		return s.pause(ctx, r.ID, "the first wave of workers is updated and passed its self-tests; check them, then resume")
	}
	if r.WavePercent >= 100 {
		if moved, err := s.handOver(ctx, r); moved || err != nil {
			return err
		}
	}
	for _, i := range s.startable(r) {
		if err := s.advanceStep(ctx, r, &r.Steps[i]); err != nil {
			return err
		}
	}
	return nil
}

func wavesDone(steps []Step, through int) bool {
	for _, st := range steps {
		if st.Wave <= through && !st.done() {
			return false
		}
	}
	return true
}

// startable picks the pending steps that may start now.
func (s *Service) startable(r Rollout) []int {
	if r.WavePercent >= 100 {
		return nextWave(r)
	}
	var controllers []int
	controllersDone := true
	for i, st := range r.Steps {
		if st.Kind != "controller" || st.done() {
			continue
		}
		controllersDone = false
		if st.State != "pending" {
			return nil
		}
		controllers = append(controllers, i)
	}
	if !controllersDone {
		// The leader goes last; it hands over leadership when it restarts.
		for _, i := range controllers {
			if r.Steps[i].Target != s.member.Hostname || len(controllers) == 1 {
				return []int{i}
			}
		}
		return nil
	}
	wave := map[string]int{}
	for _, st := range r.Steps {
		if st.Kind == "worker" && !st.done() {
			if w, ok := wave[st.PoolID]; !ok || st.Wave < w {
				wave[st.PoolID] = st.Wave
			}
		}
	}
	var out []int
	for i, st := range r.Steps {
		if st.Kind == "worker" && st.State == "pending" && st.Wave == wave[st.PoolID] && (r.PauseAfter == nil || st.Wave <= *r.PauseAfter) {
			out = append(out, i)
		}
	}
	return out
}

// nextWave starts every pending step of the lowest unfinished wave, controllers and workers alike (the 100% plan).
func nextWave(r Rollout) []int {
	lowest := 0
	for _, st := range r.Steps {
		if !st.done() && (lowest == 0 || st.Wave < lowest) {
			lowest = st.Wave
		}
	}
	if lowest == 0 || (r.PauseAfter != nil && lowest > *r.PauseAfter) {
		return nil
	}
	var out []int
	for i, st := range r.Steps {
		if st.Wave == lowest && st.State == "pending" {
			out = append(out, i)
		}
	}
	return out
}

// handOver gives the leader lease to a live controller that already runs the rollout's version when the next wave
// would update this leader's own host, so the controller driving the rollout stays up while the rest updates.
func (s *Service) handOver(ctx context.Context, r Rollout) (bool, error) {
	mine := false
	for _, i := range nextWave(r) {
		if st := r.Steps[i]; st.Kind == "controller" && st.Target == s.member.Hostname {
			mine = true
		}
	}
	if !mine {
		return false, nil
	}
	controllers, err := s.member.Controllers(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range controllers {
		if c.Live && c.Version == r.Version && c.Hostname != s.member.Hostname {
			_, epoch := s.member.Leader()
			if err := s.member.HandOver(ctx, epoch, c.ID); err != nil {
				s.log.Warn("leadership handover", "to", c.Hostname, "error", err)
				return false, nil
			}
			s.log.Info("leadership handed over for the rollout", "to", c.Hostname, "version", r.Version)
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) advanceStep(ctx context.Context, r Rollout, st *Step) error {
	age := s.now().Sub(st.UpdatedAt)
	if st.Kind == "controller" {
		return s.advanceController(ctx, r, st, age)
	}
	return s.advanceWorker(ctx, r, st, age)
}

// advanceController updates one controller host. A host that keeps its worker (ADR 0046, OD-28) drains that worker
// first; its installer then updates both, and the step succeeds only once the restarted worker runs the release and
// passes its self-test. A failed self-test rolls the whole host back, as for any worker.
func (s *Service) advanceController(ctx context.Context, r Rollout, st *Step, age time.Duration) error {
	var w workerRow
	if st.LocalWorker != "" {
		var err error
		w, err = s.worker(ctx, st.LocalWorker)
		if errors.Is(err, sql.ErrNoRows) {
			st.LocalWorker = ""
		} else if err != nil {
			return err
		}
	}
	switch st.State {
	case "pending":
		if st.LocalWorker == "" {
			return s.setStep(ctx, r.ID, st.Seq, "requested", "")
		}
		if _, err := s.workers.ApplyAction(ctx, systemWorkers(w.org), w.id, workers.ActionDrain, "self-update to "+r.Version+" with its controller", nil, ""); err != nil {
			return s.setStep(ctx, r.ID, st.Seq, "failed", "draining its worker "+w.name+": "+err.Error())
		}
		return s.setStep(ctx, r.ID, st.Seq, "draining", "draining its worker "+w.name+" before the controller updates")
	case "draining":
		busy, err := s.busy(ctx, w.id)
		if err != nil {
			return err
		}
		if busy > 0 {
			if age > drainTimeout {
				return s.setStep(ctx, r.ID, st.Seq, "failed", "its worker "+w.name+" did not drain within 2 hours")
			}
			return nil
		}
		return s.setStep(ctx, r.ID, st.Seq, "requested", "its worker "+w.name+" is drained; updating the host")
	case "testing":
		return s.checkSelfTest(ctx, r, st, w, age)
	case "rolling_back":
		return s.checkRollback(ctx, r, st, w, age)
	}
	controllers, err := s.member.Controllers(ctx)
	if err != nil {
		return err
	}
	for _, c := range controllers {
		if !c.Live || c.Hostname != st.Target || c.Version != r.Version {
			continue
		}
		if st.LocalWorker == "" {
			return s.setStep(ctx, r.ID, st.Seq, "succeeded", "running "+r.Version)
		}
		// The host may already have run this version; only a process started after staging shows the install ran.
		if st.State == "applying" && c.StartedAt.After(st.UpdatedAt) {
			return s.startSelfTest(ctx, r, st, w, age, "the controller runs "+r.Version+"; its worker "+w.name)
		}
	}
	if age > controllerTimeout {
		return s.setStep(ctx, r.ID, st.Seq, "failed", "the controller did not come back on "+r.Version+" within 15 minutes")
	}
	return nil
}

func (s *Service) busy(ctx context.Context, workerID string) (int, error) {
	var n int
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(workers.BusySlotsQuery), workerID, workerID).Scan(&n)
	return n, err
}

// startSelfTest puts an updated worker back to work and starts its self-test once it runs the release.
func (s *Service) startSelfTest(ctx context.Context, r Rollout, st *Step, w workerRow, age time.Duration, who string) error {
	if w.version == r.Version && s.workers.Connected(w.id) {
		s.reactivate(ctx, w)
		if _, err := s.workers.RequestWorkerSelfTest(ctx, systemWorkers(w.org), w.id, ""); err != nil {
			return s.rollBack(ctx, r, st, w, "its self-test could not start on "+r.Version+": "+err.Error())
		}
		return s.setStep(ctx, r.ID, st.Seq, "testing", who+" runs "+r.Version+"; running its self-test")
	}
	if age > workerTimeout {
		return s.setStep(ctx, r.ID, st.Seq, "failed", who+" did not come back on "+r.Version+" within 20 minutes")
	}
	return nil
}

// checkSelfTest ends a step once the updated worker's self-test passed, or rolls its host back. An updated worker
// builds uploaded source again only after its own self-test passes (ADR 0027).
func (s *Service) checkSelfTest(ctx context.Context, r Rollout, st *Step, w workerRow, age time.Duration) error {
	var status string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT status FROM pool_self_tests WHERE worker_id = ? AND started_at >= ? ORDER BY started_at DESC LIMIT 1`), w.id, st.UpdatedAt.Add(-time.Minute)).Scan(&status)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	case status == "passed":
		done := "running " + r.Version + "; its self-test passed"
		if st.Kind == "controller" {
			done = "the controller and its worker " + w.name + " run " + r.Version + "; the worker's self-test passed"
		}
		return s.setStep(ctx, r.ID, st.Seq, "succeeded", done)
	case status != "" && status != "running":
		return s.rollBack(ctx, r, st, w, "its self-test did not pass on "+r.Version)
	case age > workerTimeout:
		return s.rollBack(ctx, r, st, w, "its self-test on "+r.Version+" did not finish within 20 minutes")
	}
	return nil
}

func (s *Service) checkRollback(ctx context.Context, r Rollout, st *Step, w workerRow, age time.Duration) error {
	if w.version != r.Version && s.workers.Connected(w.id) {
		return s.setStep(ctx, r.ID, st.Seq, "failed", st.Detail+"; rolled back to "+w.version)
	}
	if age > workerTimeout {
		return s.setStep(ctx, r.ID, st.Seq, "failed", st.Detail+"; the rollback did not finish within 20 minutes")
	}
	return nil
}

type workerRow struct {
	id, org, name, version, state string
	minor                         int
}

func (s *Service) worker(ctx context.Context, id string) (workerRow, error) {
	var w workerRow
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id, org_id, name, version, operational_state, protocol_minor FROM workers WHERE id = ? AND enrollment_state = 'enrolled'`), id).
		Scan(&w.id, &w.org, &w.name, &w.version, &w.state, &w.minor)
	return w, err
}

func (s *Service) reactivate(ctx context.Context, w workerRow) {
	if _, err := s.workers.ApplyAction(ctx, systemWorkers(w.org), w.id, workers.ActionUncordon, "self-update finished", nil, ""); err != nil {
		s.log.Warn("self-update could not reactivate a worker", "worker", w.id, "error", err)
	}
}

// reactivateDrained puts a worker the rollout drained back to work, unless it is being removed.
func (s *Service) reactivateDrained(ctx context.Context, workerID string) {
	w, err := s.worker(ctx, workerID)
	if err != nil || w.state != workers.Draining {
		return
	}
	var removing int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM worker_removals WHERE worker_id = ?`), workerID).Scan(&removing); err != nil || removing > 0 {
		return
	}
	s.reactivate(ctx, w)
}

func (s *Service) advanceWorker(ctx context.Context, r Rollout, st *Step, age time.Duration) error {
	w, err := s.worker(ctx, st.Target)
	if errors.Is(err, sql.ErrNoRows) {
		return s.setStep(ctx, r.ID, st.Seq, "skipped", "the worker is no longer enrolled")
	}
	if err != nil {
		return err
	}
	switch st.State {
	case "pending":
		if w.minor < 1 {
			return s.setStep(ctx, r.ID, st.Seq, "failed", "this worker predates self-update; reinstall it once with install.sh --release-signers")
		}
		if _, err := s.workers.ApplyAction(ctx, systemWorkers(w.org), w.id, workers.ActionDrain, "self-update to "+r.Version, nil, ""); err != nil {
			return s.setStep(ctx, r.ID, st.Seq, "failed", "draining: "+err.Error())
		}
		return s.setStep(ctx, r.ID, st.Seq, "draining", "waiting for running builds to finish")
	case "draining":
		busy, err := s.busy(ctx, w.id)
		if err != nil {
			return err
		}
		if busy > 0 || !s.workers.Connected(w.id) {
			if age > drainTimeout {
				return s.setStep(ctx, r.ID, st.Seq, "failed", "the worker did not drain within 2 hours")
			}
			return nil
		}
		// The step must say requested before the worker can ask for the release.
		if err := s.setStep(ctx, r.ID, st.Seq, "requested", "asked the worker to fetch "+r.Version); err != nil {
			return err
		}
		if err := s.workers.Send(w.id, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_Update{Update: &workerv1.UpdateRequest{Version: r.Version}}}); err != nil {
			return s.setStep(ctx, r.ID, st.Seq, "draining", "the update request did not reach the worker; retrying")
		}
		return nil
	case "testing":
		return s.checkSelfTest(ctx, r, st, w, age)
	case "rolling_back":
		return s.checkRollback(ctx, r, st, w, age)
	default:
		return s.startSelfTest(ctx, r, st, w, age, "the worker")
	}
}

// rollBack drains a worker that installed the release but failed its self-test and has its host reinstall the
// previous release. Workers before protocol 1.3 cannot be asked; they stay on the release, building no uploaded
// source, and the step fails.
func (s *Service) rollBack(ctx context.Context, r Rollout, st *Step, w workerRow, why string) error {
	if st.Kind == "controller" {
		why = "its worker " + w.name + ": " + why + "; the host reinstalls the previous release of the controller and the worker"
	}
	if w.minor < rollbackMinor {
		return s.setStep(ctx, r.ID, st.Seq, "failed", why+"; this worker predates remote rollback (protocol 1.3), so it stays on "+r.Version+" without building uploaded source; roll it back on the host (docs/operators/upgrades.md#rollback)")
	}
	if _, err := s.workers.ApplyAction(ctx, systemWorkers(w.org), w.id, workers.ActionDrain, "self-update rollback", nil, ""); err != nil {
		return s.setStep(ctx, r.ID, st.Seq, "failed", why+"; draining for the rollback failed: "+err.Error())
	}
	if err := s.workers.Send(w.id, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_Update{Update: &workerv1.UpdateRequest{Version: r.Version, Rollback: true}}}); err != nil {
		return s.setStep(ctx, r.ID, st.Seq, "failed", why+"; the rollback request did not reach the worker: "+err.Error())
	}
	return s.setStep(ctx, r.ID, st.Seq, "rolling_back", why)
}
