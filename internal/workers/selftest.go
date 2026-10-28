package workers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/isolation"
)

// SelfTestTimeout bounds a running self-test; one that has not reported by then counts as failed.
const SelfTestTimeout = 45 * time.Minute

var (
	ErrSelfTestRunning  = errors.New("a self-test is already running for this worker")
	ErrNoSelfTestWorker = errors.New("no connected worker in this pool offers the pool's isolation profile with strict egress")
)

type SelfTest struct {
	ID, OrgID, PoolID, WorkerID, Status string
	Profile, ConfigDigest               string
	FailedChecks                        []string
	StartedAt                           time.Time
	FinishedAt                          *time.Time
}

type CheckResult struct {
	ID, Outcome, Reason string
}

func canOperate(p authz.Principal) bool {
	return authz.Can(p, []authz.Role{authz.Operator, authz.Administrator}, authz.Resource{OrgID: p.OrgID})
}

// SelfTestCandidates lists the pool's workers that could run its self-test, before connection is considered.
func (s *Store) SelfTestCandidates(ctx context.Context, orgID string, pool Pool) ([]Worker, error) {
	list, err := s.List(ctx, orgID, "", 10_000)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []Worker
	for _, w := range list {
		st := w.State(now)
		if w.PoolID == pool.ID && st.EnrollmentActive && st.OperationalState == Active && st.StrictEgress && slices.Contains(st.IsolationProfiles, pool.IsolationProfile) {
			out = append(out, w)
		}
	}
	return out, nil
}

// StartSelfTest records a running self-test of one worker for its pool's profile and its current configuration
// digest. Running tests older than SelfTestTimeout are closed as failed first, so a lost worker cannot hold itself.
func (s *Store) StartSelfTest(ctx context.Context, actor authz.Principal, w Worker, pool Pool, requestID string) (SelfTest, error) {
	if actor.Kind != authz.System && !canOperate(actor) {
		return SelfTest{}, ErrForbidden
	}
	if w.PoolID == "" || w.PoolID != pool.ID {
		return SelfTest{}, ErrUnassigned
	}
	now := s.now().UTC()
	t := SelfTest{ID: ids.New(), OrgID: actor.OrgID, PoolID: pool.ID, WorkerID: w.ID, Status: "running", Profile: pool.IsolationProfile, ConfigDigest: w.Capabilities.ConfigDigest, FailedChecks: []string{}, StartedAt: now}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE pool_self_tests SET status = 'failed', failed_checks = ?, finished_at = ? WHERE worker_id = ? AND status = 'running' AND started_at < ?`),
			`["timed-out"]`, now, w.ID, now.Add(-SelfTestTimeout)); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM pool_self_tests WHERE worker_id = ? AND status = 'running'`), w.ID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrSelfTestRunning
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO pool_self_tests (id, org_id, pool_id, worker_id, status, failed_checks, started_at, profile, config_digest) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			t.ID, actor.OrgID, pool.ID, w.ID, t.Status, "[]", now, t.Profile, t.ConfigDigest); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "worker.self_test.start", ResourceType: "worker", ResourceID: w.ID, Outcome: "succeeded", Reason: "profile " + t.Profile + ", configuration " + t.ConfigDigest, RequestID: requestID})
	})
	return t, err
}

// FinishSelfTest accepts a report only from the worker the test was sent to, while the test is running. The worker
// passes only when every check its profile requires (isolation.SelfTestChecksFor) reports passed and its configuration did not
// change during the test.
func (s *Store) FinishSelfTest(ctx context.Context, workerID, id string, checks []CheckResult) (SelfTest, error) {
	var t SelfTest
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT org_id, pool_id, worker_id, status, started_at, profile, config_digest FROM pool_self_tests WHERE id = ?`), id).
			Scan(&t.OrgID, &t.PoolID, &t.WorkerID, &t.Status, &t.StartedAt, &t.Profile, &t.ConfigDigest); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		t.ID = id
		if t.WorkerID != workerID {
			return ErrForbidden
		}
		if t.Status != "running" {
			return ErrConflict
		}
		t.FailedChecks = failedChecks(isolation.Profile(t.Profile), checks)
		var caps string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT capabilities FROM workers WHERE id = ?`), workerID).Scan(&caps); err != nil {
			return err
		}
		var current Capabilities
		_ = json.Unmarshal([]byte(caps), &current)
		if t.ConfigDigest == "" || current.ConfigDigest != t.ConfigDigest {
			t.FailedChecks = append(t.FailedChecks, "config-changed")
		}
		t.Status = "passed"
		if len(t.FailedChecks) > 0 {
			t.Status = "failed"
		}
		now := s.now().UTC()
		t.FinishedAt = &now
		raw, _ := json.Marshal(t.FailedChecks)
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE pool_self_tests SET status = ?, failed_checks = ?, finished_at = ? WHERE id = ?`), t.Status, string(raw), now, id); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{OrgID: t.OrgID, Kind: authz.System}, Action: "worker.self_test." + t.Status,
			ResourceType: "worker", ResourceID: workerID, Outcome: "succeeded", Reason: summary(workerID, checks)})
	})
	return t, err
}

func failedChecks(profile isolation.Profile, checks []CheckResult) []string {
	required := isolation.SelfTestChecksFor(profile)
	byID := map[string]CheckResult{}
	for _, c := range checks {
		byID[c.ID] = c
	}
	failed := []string{}
	for _, id := range required {
		if c, ok := byID[id]; !ok || c.Outcome != "passed" {
			failed = append(failed, id)
		}
	}
	for id, c := range byID {
		if c.Outcome != "passed" && !slices.Contains(required, id) {
			failed = append(failed, id)
		}
	}
	sort.Strings(failed)
	return failed
}

func summary(workerID string, checks []CheckResult) string {
	parts := []string{"worker " + workerID}
	for _, c := range checks {
		if c.Outcome != "passed" {
			parts = append(parts, c.ID+" "+c.Outcome+": "+c.Reason)
		}
	}
	s := strings.Join(parts, "; ")
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}

// ApprovedPools lists pools across organizations whose profile is approved, for self-test renewal.
func (s *Store) ApprovedPools(ctx context.Context) ([]Pool, []string, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT DISTINCT org_id FROM worker_pools WHERE profile_approved = ?`), true)
	if err != nil {
		return nil, nil, err
	}
	var orgs []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			rows.Close()
			return nil, nil, err
		}
		orgs = append(orgs, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	var pools []Pool
	var owners []string
	for _, o := range orgs {
		list, err := s.ListPools(ctx, o)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range list {
			if p.ProfileApproved {
				pools, owners = append(pools, p), append(owners, o)
			}
		}
	}
	return pools, owners, nil
}

// NeedsSelfTest reports whether the controller should test a worker for its pool now: it can run the pool's
// profile with strict egress, nothing is running, and its newest test is missing, stale, or for another profile or
// configuration. A failed test of the current configuration waits for an operator to rerun it.
func NeedsSelfTest(w Worker, pool Pool, now time.Time) bool {
	st := w.State(now)
	if w.PoolID != pool.ID || w.SelfTestRunning || !st.EnrollmentActive || st.OperationalState != Active || !st.StrictEgress || !slices.Contains(st.IsolationProfiles, pool.IsolationProfile) {
		return false
	}
	t := w.SelfTest
	current := t.Profile == pool.IsolationProfile && t.ConfigDigest != "" && t.ConfigDigest == w.Capabilities.ConfigDigest
	switch {
	case !current || t.Status == "never-run":
		return true
	case t.Status == "failed":
		return false
	default:
		return t.FinishedAt == nil || now.Sub(*t.FinishedAt) > SelfTestRenewal
	}
}

// Verified reports a fresh passing self-test of this worker for the pool's profile and its current configuration.
func Verified(w Worker, pool Pool, now time.Time) bool {
	t := w.SelfTest
	return t.Status == "passed" && t.FinishedAt != nil && now.Sub(*t.FinishedAt) <= isolation.SelfTestFreshness &&
		t.Profile == pool.IsolationProfile && t.ConfigDigest != "" && t.ConfigDigest == w.Capabilities.ConfigDigest
}

// CurrentSelfTest reports whether the newest finished self-test covers the worker's current profile and configuration.
func CurrentSelfTest(w Worker, pool Pool) bool {
	return w.SelfTest.Profile == pool.IsolationProfile && w.SelfTest.ConfigDigest != "" && w.SelfTest.ConfigDigest == w.Capabilities.ConfigDigest
}
