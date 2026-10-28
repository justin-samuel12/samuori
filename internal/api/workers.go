package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/workerjoin"
	"samuori.invalid/samuori/internal/workers"
)

type poolView struct {
	ID                   string       `json:"id"`
	Name                 string       `json:"name"`
	Location             string       `json:"location"`
	IsolationProfile     string       `json:"isolationProfile"`
	TrustDomain          string       `json:"trustDomain"`
	TrustedWorkloadsOnly bool         `json:"trustedWorkloadsOnly"`
	ProfileApproved      bool         `json:"profileApproved"`
	ApprovedBy           string       `json:"approvedBy,omitempty"`
	ApprovedAt           *time.Time   `json:"approvedAt,omitempty"`
	Verification         verification `json:"verification"`
	CreatedAt            time.Time    `json:"-"`
}

type verification struct {
	Workers         int `json:"workers"`
	VerifiedWorkers int `json:"verifiedWorkers"`
}

// toPool counts enrolled workers and those verified by their own self-test (OD-22).
func toPool(p workers.Pool, list []workers.Worker) poolView {
	v := poolView{ID: p.ID, Name: p.Name, Location: p.Location, IsolationProfile: p.IsolationProfile, TrustDomain: p.TrustDomain,
		TrustedWorkloadsOnly: p.TrustedWorkloadsOnly, ProfileApproved: p.ProfileApproved, ApprovedBy: p.ApprovedBy, ApprovedAt: p.ApprovedAt}
	now := time.Now()
	for _, w := range list {
		if w.PoolID != p.ID || w.EnrollmentState != workers.Enrolled {
			continue
		}
		v.Verification.Workers++
		if workers.Verified(w, p, now) {
			v.Verification.VerifiedWorkers++
		}
	}
	return v
}

func (a *API) ListWorkerPools(w http.ResponseWriter, r *http.Request) {
	pools, err := a.workers.ListPools(r.Context(), principal(r).OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	list, err := a.workers.List(r.Context(), principal(r).OrgID, "", 10_000)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Items []poolView `json:"items"`
	}{Items: make([]poolView, 0, len(pools))}
	for _, p := range pools {
		out.Items = append(out.Items, toPool(p, list))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateWorkerPool(w http.ResponseWriter, r *http.Request, _ apigen.CreateWorkerPoolParams) {
	var in apigen.WorkerPoolCreate
	if !decodeBody(w, r, &in) {
		return
	}
	p := workers.Pool{Name: in.Name, IsolationProfile: string(in.IsolationProfile), TrustDomain: in.TrustDomain}
	if in.Location != nil {
		p.Location = *in.Location
	}
	if in.TrustedWorkloadsOnly != nil {
		p.TrustedWorkloadsOnly = *in.TrustedWorkloadsOnly
	}
	p, err := a.workers.CreatePool(r.Context(), principal(r), p, requestID(w))
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	if p, err = a.workers.Pool(r.Context(), principal(r).OrgID, p.ID); err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPool(p, nil))
}

func (a *API) CreateEnrollmentToken(w http.ResponseWriter, r *http.Request, params apigen.CreateEnrollmentTokenParams) {
	var in apigen.EnrollmentTokenRequest
	if !decodeBody(w, r, &in) {
		return
	}
	p := principal(r)
	var replay bool
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		_, created, err := a.ops.Begin(r.Context(), tx, p, "enrollment-token", params.IdempotencyKey)
		replay = !created
		return err
	})
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if replay {
		apigen.WriteProblem(w, http.StatusConflict, "already_issued", "a token was already issued for this request and cannot be shown again; create a new one")
		return
	}
	t, err := a.workers.CreateToken(r.Context(), p, time.Duration(in.TtlSeconds)*time.Second, requestID(w))
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	out := apigen.EnrollmentTokenIssued{Id: t.ID, ExpiresAt: t.ExpiresAt, Token: t.Value, CaPin: t.CAPin}
	if code, ok := a.workerJoinCode(r.Context(), t); ok {
		out.Code = &code
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) ListEnrollmentTokens(w http.ResponseWriter, r *http.Request) {
	list, err := a.workers.ListTokens(r.Context(), principal(r), 100)
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	out := apigen.EnrollmentTokenList{Items: []apigen.EnrollmentToken{}}
	for _, t := range list {
		out.Items = append(out.Items, apigen.EnrollmentToken{Id: t.ID, PoolId: opt(t.PoolID), CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt,
			UsedAt: t.UsedAt, WorkerId: opt(t.WorkerID), WorkerName: opt(t.WorkerName), RevokedAt: t.RevokedAt, RevokedBy: opt(t.RevokedBy)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) RevokeEnrollmentToken(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RevokeEnrollmentTokenParams) {
	if err := a.workers.RevokeToken(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.workersError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// WorkerJoin is this controller's worker endpoint, from which worker join codes name the endpoints a remote worker
// dials (ADR 0045, SEC-WRK-11).
type WorkerJoin struct {
	Listen    string
	Hostnames []string
}

func (a *API) workerJoinCode(ctx context.Context, t workers.Token) (string, bool) {
	var controllers []string
	if a.cluster != nil {
		list, err := a.cluster.Controllers(ctx)
		if err != nil {
			a.log.Warn("listing controllers for a worker join code", "err", err)
		}
		for _, c := range list {
			if c.Live && c.Host != "" {
				controllers = append(controllers, c.Host)
			}
		}
	}
	endpoints := workerjoin.Endpoints(a.workerJoin.Listen, a.workerJoin.Hostnames, controllers)
	if len(endpoints) == 0 {
		return "", false
	}
	code, err := workerjoin.Encode(workerjoin.Code{Controllers: endpoints, Pin: t.CAPin, Token: t.Value})
	return code, err == nil
}

type workerView struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	PoolID           string       `json:"poolId,omitempty"`
	EnrollmentState  string       `json:"enrollmentState"`
	OperationalState string       `json:"operationalState"`
	Eligibility      eligibility  `json:"eligibility"`
	Capabilities     capabilities `json:"capabilities"`
	SelfTest         selfTestView `json:"selfTest"`
	Version          string       `json:"version"`
	CertExpiresAt    time.Time    `json:"certificateExpiresAt"`
	LastSeenAt       *time.Time   `json:"lastSeenAt,omitempty"`
	BundleVersion    uint64       `json:"bundleVersion"`
	RevokedAt        *time.Time   `json:"revokedAt,omitempty"`
	RevokeReason     string       `json:"revokeReason,omitempty"`
	CompromisedSince *time.Time   `json:"compromisedSince,omitempty"`
	Removal          *removalView `json:"removal,omitempty"`
}

type removalView struct {
	State       string    `json:"state"`
	Outcome     string    `json:"outcome,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	Reason      string    `json:"reason"`
	RequestedAt time.Time `json:"requestedAt"`
}

type eligibility struct {
	UntrustedBuilds bool     `json:"untrustedBuilds"`
	Reasons         []string `json:"reasons"`
}

type capabilities struct {
	OS                string   `json:"os,omitempty"`
	Architectures     []string `json:"architectures"`
	Engines           []string `json:"engines"`
	IsolationProfiles []string `json:"isolationProfiles"`
	DaemonSlots       uint32   `json:"daemonSlots"`
	ConfigDigest      string   `json:"configDigest,omitempty"`
}

type selfTestView struct {
	Status       string     `json:"status"`
	Running      bool       `json:"running"`
	Current      bool       `json:"current"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	Profile      string     `json:"profile,omitempty"`
	ConfigDigest string     `json:"configDigest,omitempty"`
	FailedChecks []string   `json:"failedChecks"`
}

// toWorker reports eligibility for uploaded source: enrollment establishes identity, never trust (ADR 0013, OD-4).
func (a *API) toWorker(r *http.Request, wk workers.Worker, pools map[string]workers.Pool) workerView {
	v := workerView{ID: wk.ID, Name: wk.Name, PoolID: wk.PoolID, EnrollmentState: wk.EnrollmentState, OperationalState: wk.OperationalState,
		Version: wk.Version, CertExpiresAt: wk.CertExpiresAt, LastSeenAt: wk.LastSeenAt,
		BundleVersion: wk.BundleVersion, RevokedAt: wk.RevokedAt, RevokeReason: wk.RevokeReason, CompromisedSince: wk.CompromisedSince,
		Capabilities: capabilities{OS: wk.Capabilities.OS, Architectures: nonNil(wk.Capabilities.Architectures), Engines: nonNil(wk.Capabilities.Engines),
			IsolationProfiles: nonNil(wk.Capabilities.IsolationProfiles), DaemonSlots: wk.Capabilities.DaemonSlots, ConfigDigest: wk.Capabilities.ConfigDigest},
		SelfTest: selfTestView{Status: wk.SelfTest.Status, Running: wk.SelfTestRunning, Current: workers.CurrentSelfTest(wk, pools[wk.PoolID]), FinishedAt: wk.SelfTest.FinishedAt,
			Profile: wk.SelfTest.Profile, ConfigDigest: wk.SelfTest.ConfigDigest, FailedChecks: nonNil(wk.SelfTest.FailedChecks)}}
	now := time.Now()
	if v.EnrollmentState == workers.Enrolled && !now.Before(wk.CertExpiresAt) {
		v.EnrollmentState = workers.Expired
	}
	ok, reasons := isolation.Eligible(pools[wk.PoolID].State(), wk.State(now), isolation.Requirement{Untrusted: true}, now)
	v.Eligibility = eligibility{UntrustedBuilds: ok, Reasons: reasons}
	if rm, found := a.workers.RemovalOf(r.Context(), wk.ID); found {
		v.Removal = &removalView{State: rm.State, Outcome: rm.Outcome, Detail: rm.Detail, Reason: rm.Reason, RequestedAt: rm.RequestedAt}
	}
	return v
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (a *API) poolIndex(r *http.Request) (map[string]workers.Pool, error) {
	pools, err := a.workers.ListPools(r.Context(), principal(r).OrgID)
	out := map[string]workers.Pool{}
	for _, p := range pools {
		out[p.ID] = p
	}
	return out, err
}

func (a *API) ListWorkers(w http.ResponseWriter, r *http.Request, params apigen.ListWorkersParams) {
	limit, cursor := pageParams(params.Limit, params.Cursor)
	list, err := a.workers.List(r.Context(), principal(r).OrgID, cursor, limit+1)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	pools, err := a.poolIndex(r)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Items      []workerView `json:"items"`
		NextCursor *string      `json:"nextCursor,omitempty"`
	}{Items: make([]workerView, 0, len(list))}
	if len(list) > limit {
		list = list[:limit]
		next := list[limit-1].ID
		out.NextCursor = &next
	}
	for _, wk := range list {
		out.Items = append(out.Items, a.toWorker(r, wk, pools))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetWorker(w http.ResponseWriter, r *http.Request, id apigen.WorkerId) {
	wk, err := a.workers.Get(r.Context(), principal(r).OrgID, id)
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	pools, err := a.poolIndex(r)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.toWorker(r, wk, pools))
}

func (a *API) SetPoolApproval(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.SetPoolApprovalParams) {
	var in apigen.PoolApproval
	if !decodeBody(w, r, &in) {
		return
	}
	p, err := a.workers.SetApproval(r.Context(), principal(r), id, in.Approved, in.Reason, requestID(w))
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	if in.Approved && a.channel != nil {
		// Approval never verifies workers; it starts each connected worker's own self-test (OD-22).
		if _, err := a.channel.RequestSelfTest(r.Context(), principal(r), id, requestID(w)); err != nil && !errors.Is(err, workers.ErrNoSelfTestWorker) && !errors.Is(err, workers.ErrSelfTestRunning) {
			a.log.Warn("self-tests after approval", "pool", id, "error", err)
		}
	}
	list, err := a.workers.List(r.Context(), principal(r).OrgID, "", 10_000)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPool(p, list))
}

// AssignWorkerPool puts a worker in a pool or moves it; its self-test for the new pool starts when the pool is
// enabled (ADR 0045).
func (a *API) AssignWorkerPool(w http.ResponseWriter, r *http.Request, id apigen.WorkerId, _ apigen.AssignWorkerPoolParams) {
	var in apigen.WorkerAssignment
	if !decodeBody(w, r, &in) {
		return
	}
	reason := ""
	if in.Reason != nil {
		reason = *in.Reason
	}
	wk, err := a.workers.AssignWorker(r.Context(), principal(r), id, in.PoolId, reason, requestID(w))
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	pools, err := a.poolIndex(r)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if pools[wk.PoolID].ProfileApproved && a.channel != nil {
		if _, err := a.channel.RequestWorkerSelfTest(r.Context(), principal(r), wk.ID, requestID(w)); err != nil && !errors.Is(err, workers.ErrNoSelfTestWorker) && !errors.Is(err, workers.ErrSelfTestRunning) {
			a.log.Warn("self-test after assignment", "worker", wk.ID, "error", err)
		}
		if wk, err = a.workers.Get(r.Context(), principal(r).OrgID, wk.ID); err != nil {
			a.internalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, a.toWorker(r, wk, pools))
}

// RunPoolSelfTest starts a self-test on each connected worker of the pool; each worker is verified on its own.
func (a *API) RunPoolSelfTest(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RunPoolSelfTestParams) {
	if a.channel == nil {
		apigen.WriteProblem(w, http.StatusConflict, "no_worker_channel", "this controller has no worker endpoint")
		return
	}
	started, err := a.channel.RequestSelfTest(r.Context(), principal(r), id, requestID(w))
	if a.selfTestError(w, r, err) {
		return
	}
	out := struct {
		Items []apigen.Operation `json:"items"`
	}{Items: []apigen.Operation{}}
	for _, t := range started {
		out.Items = append(out.Items, selfTestOperation(t, requestID(w)))
	}
	writeJSON(w, http.StatusAccepted, out)
}

func (a *API) RunWorkerSelfTest(w http.ResponseWriter, r *http.Request, id apigen.WorkerId, _ apigen.RunWorkerSelfTestParams) {
	if a.channel == nil {
		apigen.WriteProblem(w, http.StatusConflict, "no_worker_channel", "this controller has no worker endpoint")
		return
	}
	t, err := a.channel.RequestWorkerSelfTest(r.Context(), principal(r), id, requestID(w))
	if a.selfTestError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, selfTestOperation(t, requestID(w)))
}

func (a *API) selfTestError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, workers.ErrNoSelfTestWorker):
		apigen.WriteProblem(w, http.StatusConflict, "no_self_test_worker", err.Error())
	case errors.Is(err, workers.ErrSelfTestRunning):
		apigen.WriteProblem(w, http.StatusConflict, "self_test_running", err.Error())
	case errors.Is(err, workers.ErrUnassigned):
		apigen.WriteProblem(w, http.StatusConflict, "unassigned", err.Error())
	default:
		a.workersError(w, r, err)
	}
	return true
}

func selfTestOperation(t workers.SelfTest, reqID string) apigen.Operation {
	resType := "worker"
	worker := apigen.Id(t.WorkerID)
	op := apigen.Operation{Id: apigen.Id(t.ID), Status: apigen.OperationStatusRunning, ResourceType: &resType, ResourceId: &worker}
	if t.Status != "running" {
		op.Status = apigen.OperationStatusFailed
		op.Error = &apigen.Problem{Code: "self_test_not_delivered", Message: "the self-test could not be delivered to the worker", RequestId: reqID}
	}
	return op
}

func (a *API) workersError(w http.ResponseWriter, r *http.Request, err error) {
	var inUse *workers.InUse
	switch {
	case errors.As(err, &inUse):
		apigen.WriteProblem(w, http.StatusConflict, "in_use", "It cannot be deleted while it is used by "+strings.Join(inUse.Uses, "; ")+".")
	case errors.Is(err, workers.ErrTokenSpent):
		apigen.WriteProblem(w, http.StatusConflict, "token_spent", err.Error())
	case errors.Is(err, workers.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, workers.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators manage workers")
	case errors.Is(err, workers.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", "the name is already used, or the request conflicts with the current state")
	case errors.Is(err, workers.ErrUnassigned):
		apigen.WriteProblem(w, http.StatusConflict, "unassigned", err.Error())
	case errors.Is(err, workers.ErrWorkerBusy):
		apigen.WriteProblem(w, http.StatusConflict, "worker_busy", err.Error())
	case errors.Is(err, workers.ErrInUse):
		apigen.WriteProblem(w, http.StatusConflict, "in_use", "it is still in use by workers, pools or projects")
	case errors.Is(err, workers.ErrInvalid):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", "names use lowercase letters, digits and dashes, and locations must exist")
	default:
		a.internalError(w, r, err)
	}
}
