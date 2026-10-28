package workers

import (
	"context"
	"errors"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"samuori.invalid/samuori/internal/authz"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

// SelfTestRenewal is how old a passing self-test may get before the controller reruns it, ahead of
// isolation.SelfTestFreshness.
const SelfTestRenewal = 20 * time.Hour

// RequestWorkerSelfTest starts a self-test on one connected worker for its pool's profile (OD-22). Results come
// only from that worker's provider; the controller never records a pass on its own.
func (s *Server) RequestWorkerSelfTest(ctx context.Context, actor authz.Principal, workerID, requestID string) (SelfTest, error) {
	if actor.Kind != authz.System && !canOperate(actor) {
		return SelfTest{}, ErrForbidden
	}
	w, err := s.store.Get(ctx, actor.OrgID, workerID)
	if err != nil {
		return SelfTest{}, err
	}
	if w.PoolID == "" {
		return SelfTest{}, ErrUnassigned
	}
	pool, err := s.store.Pool(ctx, actor.OrgID, w.PoolID)
	if err != nil {
		return SelfTest{}, err
	}
	st := w.State(s.store.now())
	if !s.Connected(w.ID) || !st.EnrollmentActive || st.OperationalState != Active || !st.StrictEgress || !slices.Contains(st.IsolationProfiles, pool.IsolationProfile) {
		return SelfTest{}, ErrNoSelfTestWorker
	}
	t, err := s.store.StartSelfTest(ctx, actor, w, pool, requestID)
	if err != nil {
		return SelfTest{}, err
	}
	if err := s.Send(w.ID, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_SelfTest{SelfTest: &workerv1.SelfTestRequest{SelfTestId: t.ID, Profile: pool.IsolationProfile}}}); err != nil {
		return s.store.FinishSelfTest(context.WithoutCancel(ctx), w.ID, t.ID, []CheckResult{{ID: "delivery", Outcome: "errored", Reason: err.Error()}})
	}
	return t, nil
}

// RequestSelfTest starts a self-test on every connected worker of the pool that can run its profile and is not
// already testing. A pool is never verified as a whole; each worker records its own result.
func (s *Server) RequestSelfTest(ctx context.Context, actor authz.Principal, poolID, requestID string) ([]SelfTest, error) {
	if actor.Kind != authz.System && !canOperate(actor) {
		return nil, ErrForbidden
	}
	pool, err := s.store.Pool(ctx, actor.OrgID, poolID)
	if err != nil {
		return nil, err
	}
	candidates, err := s.store.SelfTestCandidates(ctx, actor.OrgID, pool)
	if err != nil {
		return nil, err
	}
	var started []SelfTest
	running := false
	for _, w := range candidates {
		if !s.Connected(w.ID) {
			continue
		}
		t, err := s.RequestWorkerSelfTest(ctx, actor, w.ID, requestID)
		switch {
		case errors.Is(err, ErrSelfTestRunning):
			running = true
		case errors.Is(err, ErrNoSelfTestWorker):
		case err != nil:
			return started, err
		default:
			started = append(started, t)
		}
	}
	if len(started) == 0 {
		if running {
			return nil, ErrSelfTestRunning
		}
		return nil, ErrNoSelfTestWorker
	}
	return started, nil
}

func (s *Server) ReportSelfTest(ctx context.Context, r *workerv1.SelfTestReport) (*workerv1.SelfTestAck, error) {
	w := ctx.Value(workerKey{}).(Worker)
	var checks []CheckResult
	for _, c := range r.GetChecks() {
		checks = append(checks, CheckResult{ID: c.GetCheckId(), Outcome: c.GetOutcome(), Reason: c.GetReason()})
	}
	t, err := s.store.FinishSelfTest(ctx, w.ID, r.GetSelfTestId(), checks)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return &workerv1.SelfTestAck{Accepted: false, Rejection: "no self-test with this id was sent to this worker"}, nil
	case errors.Is(err, ErrConflict):
		return &workerv1.SelfTestAck{Accepted: false, Rejection: "the self-test is no longer running"}, nil
	case err != nil:
		s.log.Error("self-test report", "worker", w.ID, "error", err)
		return nil, status.Error(codes.Internal, "recording the self-test failed")
	}
	s.log.Info("worker self-test finished", "pool", t.PoolID, "worker", w.ID, "status", t.Status, "failed", t.FailedChecks)
	return &workerv1.SelfTestAck{Accepted: true}, nil
}

// RenewSelfTests tests every connected worker of an approved pool whose newest self-test is missing, stale, or
// for another profile or configuration, so verification follows configuration changes (OD-22).
func (s *Server) RenewSelfTests(ctx context.Context) {
	pools, orgs, err := s.store.ApprovedPools(ctx)
	if err != nil {
		s.log.Error("self-test renewal", "error", err)
		return
	}
	listed := map[string][]Worker{}
	for i, p := range pools {
		list, ok := listed[orgs[i]]
		if !ok {
			if list, err = s.store.List(ctx, orgs[i], "", 10_000); err != nil {
				s.log.Error("self-test renewal", "error", err)
				return
			}
			listed[orgs[i]] = list
		}
		for _, w := range list {
			if w.PoolID == p.ID && s.ConnectedHere(w.ID) && NeedsSelfTest(w, p, s.store.now()) {
				s.renew(ctx, orgs[i], w.ID)
			}
		}
	}
}

func (s *Server) renew(ctx context.Context, orgID, workerID string) {
	if _, err := s.RequestWorkerSelfTest(ctx, authz.Principal{OrgID: orgID, Kind: authz.System}, workerID, ""); err != nil && !errors.Is(err, ErrNoSelfTestWorker) && !errors.Is(err, ErrSelfTestRunning) {
		s.log.Warn("self-test renewal", "worker", workerID, "error", err)
	}
}

// selfTestOnConnect tests a newly connected worker of an approved pool when it needs it, for example after a
// configuration change.
func (s *Server) selfTestOnConnect(ctx context.Context, workerID string) {
	orgID, err := s.store.OrgOf(ctx, workerID)
	if err != nil {
		return
	}
	w, err := s.store.Get(ctx, orgID, workerID)
	if err != nil {
		return
	}
	pool, err := s.store.Pool(ctx, orgID, w.PoolID)
	if err == nil && pool.ProfileApproved && NeedsSelfTest(w, pool, s.store.now()) {
		s.renew(ctx, orgID, workerID)
	}
}

// RunSelfTestRenewal calls RenewSelfTests every interval until ctx ends.
func (s *Server) RunSelfTestRenewal(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RenewSelfTests(ctx)
		}
	}
}
