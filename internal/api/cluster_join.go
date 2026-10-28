package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/clusterjoin"
)

// ClusterJoin lets more controllers join this cluster with single-use tokens (docs/operators/high-availability.md#join).
// Bundle is nil on controllers that cannot hand out a join bundle, such as standalone installations.
type ClusterJoin struct {
	Tokens *clusterjoin.Store
	Bundle func(host string) (clusterjoin.Bundle, error)
	// Pin and URL tell the joining controller where to connect and which TLS certificate to expect.
	Pin, URL string
}

func toJoinToken(t clusterjoin.Token) apigen.ControllerJoinToken {
	out := apigen.ControllerJoinToken{Id: t.ID, CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, UsedAt: t.UsedAt, RevokedAt: t.RevokedAt}
	if t.UsedHost != "" {
		out.UsedHost = &t.UsedHost
	}
	return out
}

func (a *API) joinProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, clusterjoin.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, clusterjoin.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, clusterjoin.ErrInvalid):
		apigen.WriteProblem(w, http.StatusForbidden, "invalid_token", err.Error())
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) ListControllerJoinTokens(w http.ResponseWriter, r *http.Request, params apigen.ListControllerJoinTokensParams) {
	if a.clusterJoin.Tokens == nil {
		writeJSON(w, http.StatusOK, apigen.ControllerJoinTokenList{Items: []apigen.ControllerJoinToken{}})
		return
	}
	list, err := a.clusterJoin.Tokens.List(r.Context(), principal(r))
	if err != nil {
		a.joinProblem(w, r, err)
		return
	}
	out := apigen.ControllerJoinTokenList{Items: []apigen.ControllerJoinToken{}}
	for _, t := range list {
		out.Items = append(out.Items, toJoinToken(t))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x apigen.ControllerJoinToken) string { return x.Id }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateControllerJoinToken(w http.ResponseWriter, r *http.Request, _ apigen.CreateControllerJoinTokenParams) {
	if a.clusterJoin.Bundle == nil || a.clusterJoin.Tokens == nil {
		apigen.WriteProblem(w, http.StatusConflict, "not_clustered", "this controller was not set up with cluster init, so other controllers cannot join it")
		return
	}
	var body apigen.ControllerJoinTokenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "invalid body")
		return
	}
	t, value, err := a.clusterJoin.Tokens.Create(r.Context(), principal(r), time.Duration(body.TtlSeconds)*time.Second, requestID(w))
	if err != nil {
		a.joinProblem(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, apigen.ControllerJoinTokenIssued{Id: t.ID, ExpiresAt: t.ExpiresAt, Token: value, Pin: a.clusterJoin.Pin, Url: a.clusterJoin.URL,
		Code: clusterjoin.EncodeCode(a.clusterJoin.URL, a.clusterJoin.Pin, value)})
}

func (a *API) RevokeControllerJoinToken(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RevokeControllerJoinTokenParams) {
	if a.clusterJoin.Tokens == nil {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err := a.clusterJoin.Tokens.Revoke(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.joinProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) joinRateLimited(w http.ResponseWriter, r *http.Request, kind string) (string, bool) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ok, retry := a.joinLimit.allow(kind + ":" + ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		apigen.WriteProblem(w, http.StatusTooManyRequests, "rate_limited", "too many join attempts")
		return ip, true
	}
	return ip, false
}

// JoinCluster redeems a join token and opens a join request; it returns no secrets. The token is never logged.
func (a *API) JoinCluster(w http.ResponseWriter, r *http.Request) {
	ip, limited := a.joinRateLimited(w, r, "join")
	if limited {
		return
	}
	if a.clusterJoin.Bundle == nil || a.clusterJoin.Tokens == nil {
		apigen.WriteProblem(w, http.StatusConflict, "not_clustered", "this controller does not accept joining controllers")
		return
	}
	var body apigen.ClusterJoinRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "invalid body")
		return
	}
	token := ""
	if body.Token != nil {
		token = *body.Token
	}
	q, err := a.clusterJoin.Tokens.Redeem(r.Context(), token, body.Host, ip, body.SecretHash, requestID(w))
	if err != nil {
		a.joinProblem(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, apigen.ClusterJoinPending{RequestId: q.ID, VerificationCode: q.Code})
}

type joinPollResult struct {
	State  string              `json:"state"`
	Bundle *clusterjoin.Bundle `json:"bundle,omitempty"`
}

// PollClusterJoin hands the accepted bundle to the host holding the request's secret, once. Neither the secret nor
// the bundle is logged.
func (a *API) PollClusterJoin(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	if _, limited := a.joinRateLimited(w, r, "join-poll"); limited {
		return
	}
	if a.clusterJoin.Bundle == nil || a.clusterJoin.Tokens == nil {
		apigen.WriteProblem(w, http.StatusConflict, "not_clustered", "this controller does not accept joining controllers")
		return
	}
	var body apigen.ClusterJoinPoll
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "invalid body")
		return
	}
	secret := ""
	if body.Secret != nil {
		secret = *body.Secret
	}
	q, b, err := a.clusterJoin.Tokens.Poll(r.Context(), id, secret, a.clusterJoin.Bundle, requestID(w))
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case errors.Is(err, clusterjoin.ErrInvalid):
		apigen.WriteProblem(w, http.StatusForbidden, "invalid_join_request", "the join request is unknown or completed, or the secret does not match")
	case errors.Is(err, clusterjoin.ErrRejected):
		detail := err.Error()
		if q.Reason != "" {
			detail += ": " + q.Reason
		}
		apigen.WriteProblem(w, http.StatusForbidden, "join_rejected", detail)
	case errors.Is(err, clusterjoin.ErrExpired):
		apigen.WriteProblem(w, http.StatusForbidden, "join_expired", err.Error())
	case err != nil:
		a.internalError(w, r, err)
	case b != nil:
		writeJSON(w, http.StatusOK, joinPollResult{State: clusterjoin.StateAccepted, Bundle: b})
	default:
		writeJSON(w, http.StatusOK, joinPollResult{State: q.State})
	}
}

func toJoinRequest(q clusterjoin.Request) apigen.ControllerJoinRequest {
	out := apigen.ControllerJoinRequest{Id: q.ID, TokenId: q.TokenID, Host: q.Host, RemoteAddress: q.RemoteAddr, VerificationCode: q.Code,
		State: apigen.ControllerJoinRequestState(q.State), DecidedAt: q.DecidedAt, CreatedAt: q.CreatedAt, ExpiresAt: q.ExpiresAt}
	if q.DecidedBy != "" {
		out.DecidedBy = &q.DecidedBy
	}
	if q.Reason != "" {
		out.Reason = &q.Reason
	}
	return out
}

func (a *API) ListControllerJoinRequests(w http.ResponseWriter, r *http.Request, params apigen.ListControllerJoinRequestsParams) {
	out := apigen.ControllerJoinRequestList{Items: []apigen.ControllerJoinRequest{}}
	if a.clusterJoin.Tokens == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	list, err := a.clusterJoin.Tokens.ListRequests(r.Context(), principal(r))
	if err != nil {
		a.joinProblem(w, r, err)
		return
	}
	for _, q := range list {
		out.Items = append(out.Items, toJoinRequest(q))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x apigen.ControllerJoinRequest) string { return x.Id }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) DecideControllerJoinRequest(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DecideControllerJoinRequestParams) {
	if a.clusterJoin.Tokens == nil {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	var body apigen.ControllerJoinDecision
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || (body.Decision != apigen.ControllerJoinDecisionDecisionAccept && body.Decision != apigen.ControllerJoinDecisionDecisionReject) {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "decision must be accept or reject")
		return
	}
	reason := ""
	if body.Reason != nil {
		reason = *body.Reason
	}
	q, err := a.clusterJoin.Tokens.Decide(r.Context(), principal(r), id, body.Decision == apigen.ControllerJoinDecisionDecisionAccept, reason, requestID(w))
	if errors.Is(err, clusterjoin.ErrDecided) {
		apigen.WriteProblem(w, http.StatusConflict, "already_decided", err.Error())
		return
	}
	if err != nil {
		a.joinProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toJoinRequest(q))
}
