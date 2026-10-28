package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/tenancy"
)

// SessionTimeouts are an organization's browser-session limits (ADR 0045).
type SessionTimeouts struct {
	Idle, Max, Warning time.Duration
}

const (
	MinIdle, MaxIdle       = 15 * time.Minute, 120 * time.Minute
	MinMax, MaxMax         = time.Hour, 24 * time.Hour
	MinWarning, MaxWarning = 30 * time.Second, 600 * time.Second
)

var ErrSessionTimeouts = errors.New("invalid session timeouts")

func (t SessionTimeouts) Validate() error {
	switch {
	case t.Idle < MinIdle || t.Idle > MaxIdle || t.Idle%time.Minute != 0:
		return fmt.Errorf("%w: the idle timeout is a whole number of minutes from 15 to 120", ErrSessionTimeouts)
	case t.Max < MinMax || t.Max > MaxMax || t.Max%time.Hour != 0:
		return fmt.Errorf("%w: the maximum session is a whole number of hours from 1 to 24", ErrSessionTimeouts)
	case t.Warning < MinWarning || t.Warning > MaxWarning || t.Warning%time.Second != 0:
		return fmt.Errorf("%w: the warning is a whole number of seconds from 30 to 600", ErrSessionTimeouts)
	case t.Idle > t.Max:
		return fmt.Errorf("%w: the idle timeout cannot be longer than the maximum session", ErrSessionTimeouts)
	}
	return nil
}

// SessionStatus reports a browser session's deadlines. IdleExpiresAt never passes AbsoluteExpiresAt.
type SessionStatus struct {
	IdleExpiresAt, AbsoluteExpiresAt, ServerTime time.Time
	Timeouts                                     SessionTimeouts
}

type sessionRow struct {
	tokenHash, userID, csrfHash string
	created, lastSeen           time.Time
	timeouts                    SessionTimeouts
}

func (r sessionRow) absoluteDeadline() time.Time { return r.created.Add(r.timeouts.Max) }

func (r sessionRow) idleDeadline() time.Time {
	if d := r.lastSeen.Add(r.timeouts.Idle); d.Before(r.absoluteDeadline()) {
		return d
	}
	return r.absoluteDeadline()
}

func (r sessionRow) expired(now time.Time) bool { return now.After(r.idleDeadline()) }

func (r sessionRow) status(now time.Time) SessionStatus {
	return SessionStatus{IdleExpiresAt: r.idleDeadline().UTC(), AbsoluteExpiresAt: r.absoluteDeadline().UTC(), ServerTime: now.UTC(), Timeouts: r.timeouts}
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Service) loadSession(ctx context.Context, tokenHash string) (sessionRow, error) {
	return s.loadSessionWith(ctx, s.db.SQL, tokenHash)
}

func (s *Service) loadSessionWith(ctx context.Context, q queryRower, tokenHash string) (sessionRow, error) {
	r := sessionRow{tokenHash: tokenHash}
	var idle, maxLen, warning int64
	err := q.QueryRowContext(ctx, s.db.Rebind(`SELECT s.user_id, s.csrf_hash, s.created_at, s.last_seen_at, o.session_idle_seconds, o.session_max_seconds, o.session_warning_seconds
		FROM sessions s JOIN users u ON u.id = s.user_id JOIN organizations o ON o.id = u.org_id
		WHERE s.token_hash = ? AND s.revoked_at IS NULL`), tokenHash).
		Scan(&r.userID, &r.csrfHash, &r.created, &r.lastSeen, &idle, &maxLen, &warning)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.timeouts = SessionTimeouts{Idle: time.Duration(idle) * time.Second, Max: time.Duration(maxLen) * time.Second, Warning: time.Duration(warning) * time.Second}
	return r, err
}

// Session reports the deadlines of a live browser session without renewing it.
func (s *Service) Session(ctx context.Context, sessionToken string) (SessionStatus, error) {
	now := s.now()
	r, err := s.loadSession(ctx, hashSecret(sessionToken))
	if err != nil {
		return SessionStatus{}, err
	}
	if r.expired(now) {
		return SessionStatus{}, ErrNotFound
	}
	return r.status(now), nil
}

// ExtendSession renews the idle deadline of a live session. The absolute maximum is never extended, and an expired
// session stays expired.
func (s *Service) ExtendSession(ctx context.Context, sessionToken string) (SessionStatus, error) {
	var out SessionStatus
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		r, err := s.loadSessionWith(ctx, tx, hashSecret(sessionToken))
		if err != nil {
			return err
		}
		if r.expired(now) {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ? AND revoked_at IS NULL`), now.UTC(), r.tokenHash); err != nil {
			return err
		}
		r.lastSeen = now
		out = r.status(now)
		return nil
	})
	return out, err
}

// SessionTimeouts returns the organization's session limits.
func (s *Service) SessionTimeouts(ctx context.Context, orgID string) (SessionTimeouts, error) {
	var idle, maxLen, warning int64
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT session_idle_seconds, session_max_seconds, session_warning_seconds FROM organizations WHERE id = ?`), orgID).
		Scan(&idle, &maxLen, &warning)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionTimeouts{}, ErrNotFound
	}
	return SessionTimeouts{Idle: time.Duration(idle) * time.Second, Max: time.Duration(maxLen) * time.Second, Warning: time.Duration(warning) * time.Second}, err
}

// SetSessionTimeouts changes the organization's session limits; they apply to existing sessions at once.
func (s *Service) SetSessionTimeouts(ctx context.Context, actor authz.Principal, t SessionTimeouts, requestID string) error {
	if !actor.IsOrgAdmin() {
		return tenancy.ErrForbidden
	}
	if err := t.Validate(); err != nil {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE organizations SET session_idle_seconds = ?, session_max_seconds = ?, session_warning_seconds = ? WHERE id = ?`),
			int64(t.Idle/time.Second), int64(t.Max/time.Second), int64(t.Warning/time.Second), actor.OrgID); err != nil {
			return err
		}
		reason := fmt.Sprintf("idle %s, maximum %s, warning %s", t.Idle, t.Max, t.Warning)
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "identity.session-timeouts", ResourceType: "organization", ResourceID: actor.OrgID, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
}
