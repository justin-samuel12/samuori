package builds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/imagesources"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
	"samuori.invalid/samuori/internal/workers"
)

// MirrorTokens issues tokens for authorizing registry mirrors.
type MirrorTokens interface {
	Issue(ctx context.Context, r imagesources.Request) (imagesources.Token, error)
}

func (sc *Scheduler) UseMirrorTokens(m MirrorTokens) { sc.mirrors = m }

// MirrorToken issues a mirror token to the worker holding the current lease of a running attempt, granting only the
// repositories the run's team may pull through that mirror. A token that would grant nothing is refused, so the pull
// fails with an explicit authentication error and never falls back (SEC-REG-06). Tokens are never logged or stored.
func (sc *Scheduler) MirrorToken(ctx context.Context, workerID string, req *workerv1.MirrorTokenRequest) (*workerv1.MirrorTokenResponse, error) {
	deny := func(r Run, reason string) (*workerv1.MirrorTokenResponse, error) {
		sc.auditMirror(ctx, workerID, req, r, "denied", reason)
		return &workerv1.MirrorTokenResponse{Result: &workerv1.MirrorTokenResponse_Denied{Denied: &workerv1.MirrorTokenDenied{Reason: reason}}}, nil
	}
	if len(req.GetScopes()) > 64 || len(req.GetMirror()) > 260 {
		return deny(Run{}, "the request is too large")
	}
	r, err := sc.current(ctx, sc.s.db.SQL, workerID, req.GetLease())
	if errors.Is(err, workers.ErrStaleLease) {
		return deny(Run{}, "lease not current")
	}
	if err != nil {
		return nil, err
	}
	if (r.State != Preparing && r.State != Building) || r.LeaseDeadline == nil || !sc.s.now().Before(*r.LeaseDeadline) {
		return deny(r, "attempt not running or lease expired")
	}
	if sc.mirrors == nil {
		return deny(r, "private image sources are not configured")
	}
	t, err := sc.mirrors.Issue(ctx, imagesources.Request{OrgID: r.OrgID, TeamID: r.TeamID, Subject: r.CurrentAttemptID, Mirror: req.GetMirror(), Scopes: req.GetScopes()})
	if err != nil {
		return deny(r, "the token issuer is unavailable: "+err.Error())
	}
	if len(t.Granted) == 0 {
		return deny(r, "no approved image source allows "+strings.Join(t.Denied, ", "))
	}
	names := make([]string, 0, len(t.Granted))
	for _, g := range t.Granted {
		names = append(names, g.Name)
	}
	reason := "granted " + strings.Join(names, ", ")
	if len(t.Denied) > 0 {
		reason += "; refused " + strings.Join(t.Denied, ", ")
	}
	sc.auditMirror(ctx, workerID, req, r, "issued", reason)
	return &workerv1.MirrorTokenResponse{Result: &workerv1.MirrorTokenResponse_Token{Token: &workerv1.MirrorToken{Token: t.Value, ExpiresInSeconds: int64(t.ExpiresIn), IssuedAt: timestamppb.New(t.IssuedAt)}}}, nil
}

func (sc *Scheduler) auditMirror(ctx context.Context, workerID string, req *workerv1.MirrorTokenRequest, r Run, outcome, reason string) {
	actor := authz.Principal{ID: workerID, OrgID: r.OrgID, Kind: authz.Service}
	detail := fmt.Sprintf("attempt %s, mirror %s: %s", req.GetLease().GetAttemptId(), req.GetMirror(), reason)
	if len(detail) > 1000 {
		detail = detail[:1000]
	}
	err := sc.s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, sc.s.db, tx, audit.Event{Actor: actor, Action: "mirror-token.issue", ResourceType: "build", ResourceID: r.ID, Outcome: outcome, Reason: detail})
	})
	if err != nil {
		sc.log.Error("audit mirror token", "error", err)
	}
}
