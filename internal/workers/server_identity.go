package workers

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/pki"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

// RevokeHook reacts to a revocation, for example fencing the worker's leases or holding its outputs.
type RevokeHook func(ctx context.Context, w Worker, compromisedSince *time.Time) error

// OnRevoke registers a hook that runs after every revocation.
func (s *Server) OnRevoke(h RevokeHook) { s.revokeHooks = append(s.revokeHooks, h) }

func (s *Server) RenewCertificate(ctx context.Context, req *workerv1.RenewCertificateRequest) (*workerv1.RenewCertificateResponse, error) {
	w := ctx.Value(workerKey{}).(Worker)
	der, expires, err := s.store.Renew(ctx, w, req.GetCsrDer(), req.GetAcknowledgedBundleVersion())
	switch {
	case errors.Is(err, ErrRevoked):
		return nil, status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, ErrConflict):
		return nil, status.Error(codes.Aborted, "the certificate changed during renewal; reconnect and retry")
	case err != nil && isCSRError(err):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		s.log.Error("certificate renewal", "worker", w.ID, "error", err)
		return nil, status.Error(codes.Internal, "certificate renewal failed")
	}
	s.log.Info("worker certificate renewed", "worker", w.ID, "expires", expires)
	return &workerv1.RenewCertificateResponse{CertificateDer: der, CertificateExpiresAt: timestamppb.New(expires), TrustBundle: s.bundle()}, nil
}

func isCSRError(err error) bool {
	return strings.HasPrefix(err.Error(), "pki:")
}

// Actions an operator applies to a worker (applyWorkerAction).
const (
	ActionCordon     = "cordon"
	ActionUncordon   = "uncordon"
	ActionDrain      = "drain"
	ActionQuarantine = "quarantine"
	ActionRevoke     = "revoke"
)

// ApplyAction cordons, drains, quarantines, reactivates or revokes a worker. Revocation takes effect at once: the
// identity is denied, its stream is closed and the revoke hooks fence its leases and, for a compromised worker, hold
// what it produced since compromisedSince (ADR 0026).
func (s *Server) ApplyAction(ctx context.Context, actor authz.Principal, id, action, reason string, compromisedSince *time.Time, requestID string) (Worker, error) {
	switch action {
	case ActionCordon, ActionUncordon, ActionDrain, ActionQuarantine:
		state := map[string]string{ActionCordon: Cordoned, ActionUncordon: Active, ActionDrain: Draining, ActionQuarantine: Quarantined}[action]
		w, err := s.store.SetOperational(ctx, actor, id, state, reason, requestID)
		if err == nil && action == ActionDrain {
			_ = s.Send(id, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_Drain{Drain: &workerv1.Drain{Reason: reason}}})
		}
		return w, err
	case ActionRemove:
		return s.RequestRemoval(ctx, actor, id, reason, requestID)
	case ActionRevoke:
		w, err := s.store.Revoke(ctx, actor, id, reason, compromisedSince, requestID)
		if err != nil {
			return Worker{}, err
		}
		s.Disconnect(id)
		for _, h := range s.revokeHooks {
			if err := h(context.WithoutCancel(ctx), w, compromisedSince); err != nil {
				s.log.Error("revocation follow-up", "worker", id, "error", err)
				return w, err
			}
		}
		return w, nil
	}
	return Worker{}, ErrConflict
}

// PushBundle sends the current trust bundle to every connected worker.
func (s *Server) PushBundle() {
	b := s.bundle()
	s.mu.Lock()
	ids := make([]string, 0, len(s.connected))
	for id := range s.connected {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		_ = s.Send(id, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_TrustBundle{TrustBundle: b}})
	}
}

// Rotation reports the trust bundle and the workers that block a planned rotation from switching.
type Rotation struct {
	BundleVersion uint64
	Anchors       []pki.AnchorInfo
	InProgress    bool
	Pending       []Worker
}

var ErrRotationBlocked = errors.New("workers have not acknowledged the new trust bundle")

func (s *Server) Rotation(ctx context.Context) (Rotation, error) {
	version, anchors := s.store.auth.Anchors()
	r := Rotation{BundleVersion: version, Anchors: anchors}
	for _, a := range anchors {
		if a.State == pki.StateNext {
			r.InProgress = true
		}
	}
	pending, err := s.store.PendingAcknowledgement(ctx, version)
	r.Pending = pending
	return r, err
}

// BeginRotation adds a new CA to the trust bundle and sends it to every connected worker. Issuance switches only
// after every active worker has acknowledged the new bundle.
func (s *Server) BeginRotation(ctx context.Context, actor authz.Principal, requestID string) (Rotation, error) {
	if !actor.IsOrgAdmin() {
		return Rotation{}, ErrForbidden
	}
	if err := s.store.auth.BeginRotation(); err != nil {
		return Rotation{}, err
	}
	s.auditPKI(ctx, actor, "pki.rotation.begin", requestID)
	s.PushBundle()
	return s.Rotation(ctx)
}

// SwitchRotation makes the new CA issue once no active worker is pending. The old CA stays trusted until every
// certificate it issued has expired.
func (s *Server) SwitchRotation(ctx context.Context, actor authz.Principal, requestID string) (Rotation, error) {
	if !actor.IsOrgAdmin() {
		return Rotation{}, ErrForbidden
	}
	r, err := s.Rotation(ctx)
	if err != nil {
		return r, err
	}
	if !r.InProgress {
		return r, pki.ErrNoRotation
	}
	if len(r.Pending) > 0 {
		return r, ErrRotationBlocked
	}
	if err := s.store.auth.Switch(s.store.now().Add(s.store.CertTTL + ServerCertTTL)); err != nil {
		return r, err
	}
	s.auditPKI(ctx, actor, "pki.rotation.switch", requestID)
	return s.Rotation(ctx)
}

// EmergencyRotation replaces every CA, revokes every worker identity and closes their streams. Workers must be
// re-enrolled out of band with the new CA pin (ADR 0026).
func (s *Server) EmergencyRotation(ctx context.Context, actor authz.Principal, reason, requestID string) (Rotation, error) {
	if !actor.IsOrgAdmin() {
		return Rotation{}, ErrForbidden
	}
	if err := s.store.auth.Emergency(); err != nil {
		return Rotation{}, err
	}
	s.auditPKI(ctx, actor, "pki.rotation.emergency", requestID)
	revoked, err := s.store.RevokeAll(ctx, actor, "emergency CA rotation: "+reason, requestID)
	for _, id := range revoked {
		s.Disconnect(id)
		if w, gerr := s.store.get(ctx, `id = ?`, id); gerr == nil {
			for _, h := range s.revokeHooks {
				_ = h(context.WithoutCancel(ctx), w, nil)
			}
		}
	}
	if err != nil {
		return Rotation{}, err
	}
	return s.Rotation(ctx)
}

func (s *Server) auditPKI(ctx context.Context, actor authz.Principal, action, requestID string) {
	version, _ := s.store.auth.Bundle()
	err := s.store.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.store.db, tx, audit.Event{Actor: actor, Action: action, ResourceType: "pki", ResourceID: s.store.auth.Issuing().Pin(), Outcome: "succeeded", Reason: "bundle version " + strconv.FormatUint(version, 10), RequestID: requestID})
	})
	if err != nil {
		s.log.Error("audit", "action", action, "error", err)
	}
}

// RunRetirement removes retired CAs once their certificates have expired and sends the smaller bundle.
func (s *Server) RunRetirement(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.RetireOnce(); err != nil {
				s.log.Error("CA retirement", "error", err)
			}
		}
	}
}

// RetireOnce removes CAs whose retirement time has passed and sends the smaller bundle.
func (s *Server) RetireOnce() error {
	changed, err := s.store.auth.RetireExpired(s.store.now())
	if changed && err == nil {
		s.log.Info("retired CA removed from the trust bundle")
		s.PushBundle()
	}
	return err
}
