package workers

import (
	"context"
	"database/sql"
	"samuori.invalid/samuori/internal/version"
	"slices"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/isolation"
)

// SetApproval enables or disables a pool, recorded as an immutable approval decision. Creating a pool already
// approves it; this re-enables a disabled pool or the system-created local one. Approval alone never makes a
// worker verified: untrusted builds also need its passing self-test.
func (s *Store) SetApproval(ctx context.Context, actor authz.Principal, poolID string, approved bool, reason, requestID string) (Pool, error) {
	if !actor.IsOrgAdmin() {
		return Pool{}, ErrForbidden
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_pools SET profile_approved = ? WHERE id = ? AND org_id = ?`), approved, poolID, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		if err := s.recordApproval(ctx, tx, actor, poolID, approved, reason); err != nil {
			return err
		}
		outcome := "approved"
		if !approved {
			outcome = "withdrawn"
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pool.profile." + outcome, ResourceType: "worker_pool", ResourceID: poolID, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Pool{}, err
	}
	return s.Pool(ctx, actor.OrgID, poolID)
}

func (s *Store) recordApproval(ctx context.Context, tx *sql.Tx, actor authz.Principal, poolID string, approved bool, reason string) error {
	_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO pool_approvals (id, org_id, pool_id, approved, reason, actor_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
		ids.New(), actor.OrgID, poolID, approved, reason, actor.ID, s.now().UTC())
	return err
}

func (s *Store) Pool(ctx context.Context, orgID, id string) (Pool, error) {
	pools, err := s.ListPools(ctx, orgID)
	if err != nil {
		return Pool{}, err
	}
	for _, p := range pools {
		if p.ID == id {
			return p, nil
		}
	}
	return Pool{}, ErrNotFound
}

// State is what eligibility needs from the pool; verification belongs to each worker (OD-22).
func (p Pool) State() isolation.PoolState {
	return isolation.PoolState{Profile: isolation.Profile(p.IsolationProfile), ProfileApproved: p.ProfileApproved, TrustedWorkloadsOnly: p.TrustedWorkloadsOnly}
}

func (w Worker) State(now time.Time) isolation.WorkerState {
	return isolation.WorkerState{EnrollmentActive: w.EnrollmentState == Enrolled && now.Before(w.CertExpiresAt), Unassigned: w.PoolID == "", OperationalState: w.OperationalState, ProtocolMajor: ProtocolMajor, VersionCompatible: w.VersionCompatible(),
		OS: w.Capabilities.OS, Architectures: w.Capabilities.Architectures, IsolationProfiles: w.Capabilities.IsolationProfiles, FreeSlots: w.Capabilities.DaemonSlots, StrictEgress: w.Capabilities.StrictEgress, Engines: w.Capabilities.Engines,
		Providers: w.Capabilities.Providers, ConfigDigest: w.Capabilities.ConfigDigest, SelfTest: isolation.SelfTestState{Status: w.SelfTest.Status, FinishedAt: w.SelfTest.FinishedAt, Profile: w.SelfTest.Profile, ConfigDigest: w.SelfTest.ConfigDigest}}
}

// VersionCompatible applies the version matrix to the version the worker last reported.
func (w Worker) VersionCompatible() bool {
	ok, _ := version.WorkerCompatible(version.Version, w.Version)
	return ok
}

// UntrustedCapacity reports whether any enrolled worker may build uploaded source for every platform, with the
// reasons each candidate was refused when none can.
func (s *Store) UntrustedCapacity(ctx context.Context, orgID string, locations []string, req isolation.Requirement) (bool, []string, error) {
	req.Untrusted = true
	pools, err := s.ListPools(ctx, orgID)
	if err != nil {
		return false, nil, err
	}
	byID := map[string]Pool{}
	for _, p := range pools {
		byID[p.ID] = p
	}
	list, err := s.List(ctx, orgID, "", 10_000)
	if err != nil {
		return false, nil, err
	}
	reasons := map[string]bool{}
	now := s.now()
	for _, w := range list {
		if pool := byID[w.PoolID]; len(locations) > 0 && !slices.Contains(locations, pool.Location) {
			continue
		}
		ok, why := isolation.Eligible(byID[w.PoolID].State(), w.State(now), req, now)
		if ok {
			return true, nil, nil
		}
		for _, r := range why {
			reasons[r] = true
		}
	}
	out := []string{}
	for r := range reasons {
		out = append(out, r)
	}
	if len(list) == 0 {
		out = append(out, "no-workers")
	}
	return false, out, nil
}
