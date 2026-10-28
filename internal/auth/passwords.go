package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/tenancy"
)

// TemporaryPasswordTTL bounds how long an administrator-issued password can be used to choose a new one (ADR 0065).
const TemporaryPasswordTTL = 24 * time.Hour

var (
	ErrPasswordChangeRequired = errors.New("this password is temporary; choose a new password to sign in")
	ErrWrongPassword          = errors.New("the current password is incorrect")
	ErrNotLocalUser           = errors.New("this account has no Samuori password; its identity provider manages the password")
	ErrResetSelf              = errors.New("change your own password from your account page")
	ErrSamePassword           = errors.New("the new password must differ from the current one")
)

// TemporaryPassword is returned once to the administrator who reset a password; only its hash is stored.
type TemporaryPassword struct {
	Value     string
	ExpiresAt time.Time
}

// ResetPassword gives a local user a random temporary password that must be replaced at the next sign-in, and ends
// the user's sessions. API tokens are kept: disable the user to stop them.
func (s *Service) ResetPassword(ctx context.Context, actor authz.Principal, userID, requestID string) (TemporaryPassword, error) {
	if !actor.IsOrgAdmin() {
		return TemporaryPassword{}, tenancy.ErrForbidden
	}
	if userID == actor.ID {
		return TemporaryPassword{}, ErrResetSelf
	}
	var username string
	var hasCredential bool
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT u.username, EXISTS (SELECT 1 FROM user_credentials c WHERE c.user_id = u.id) FROM users u
		WHERE u.id = ? AND u.org_id = ? AND u.kind = 'user'`), userID, actor.OrgID).Scan(&username, &hasCredential)
	if errors.Is(err, sql.ErrNoRows) {
		return TemporaryPassword{}, tenancy.ErrNotFound
	}
	if err != nil {
		return TemporaryPassword{}, err
	}
	if !hasCredential {
		return TemporaryPassword{}, ErrNotLocalUser
	}
	value := temporaryPassword(username)
	hash, err := HashPassword(value)
	if err != nil {
		return TemporaryPassword{}, err
	}
	now := s.now().UTC()
	out := TemporaryPassword{Value: value, ExpiresAt: now.Add(TemporaryPasswordTTL)}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE user_credentials SET password_hash = ?, temporary_until = ?, updated_at = ? WHERE user_id = ?`), hash, out.ExpiresAt, now, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`), now, userID); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "user.password-reset", ResourceType: "user", ResourceID: userID, Outcome: "succeeded", RequestID: requestID})
	})
	if err != nil {
		return TemporaryPassword{}, err
	}
	return out, nil
}

func temporaryPassword(username string) string {
	for {
		b := make([]byte, 18)
		_, _ = rand.Read(b)
		v := base64.RawURLEncoding.EncodeToString(b)
		if CheckPasswordPolicy(username, v) == nil {
			return v
		}
	}
}

// ChangePassword replaces the caller's own local password after checking the current one, under the sign-in rate
// limit and lockout. Every other session of the user ends; the session making the change stays signed in.
func (s *Service) ChangePassword(ctx context.Context, p authz.Principal, sessionToken, current, next, clientIP, requestID string) error {
	now := s.now()
	if !s.allowIP(clientIP, now) {
		return ErrRateLimited
	}
	var username, orgID, hash string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT u.username, u.org_id, c.password_hash FROM users u JOIN user_credentials c ON c.user_id = u.id WHERE u.id = ? AND u.kind = 'user' AND u.disabled = ?`), p.ID, false).
		Scan(&username, &orgID, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotLocalUser
	}
	if err != nil {
		return err
	}
	locked, err := s.locked(ctx, username, now)
	if err != nil {
		return err
	}
	if locked {
		return ErrLocked
	}
	if !VerifyPassword(hash, current) {
		s.fail(ctx, username, now)
		s.auditPassword(ctx, p, "denied", "wrong current password", requestID)
		return ErrWrongPassword
	}
	if current == next {
		return ErrSamePassword
	}
	if err := s.checkPassword(username, next); err != nil {
		return err
	}
	s.clearFailures(ctx, username)
	return s.setPassword(ctx, p, p.ID, next, hashSecret(sessionToken), "user.password-change", requestID)
}

// setPassword stores a new permanent password and revokes every session of the user except keepSession.
func (s *Service) setPassword(ctx context.Context, actor authz.Principal, userID, password, keepSession, action, requestID string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE user_credentials SET password_hash = ?, temporary_until = NULL, updated_at = ? WHERE user_id = ?`), hash, now, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND token_hash <> ? AND revoked_at IS NULL`), now, userID, keepSession); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: action, ResourceType: "user", ResourceID: userID, Outcome: "succeeded", RequestID: requestID})
	})
}

// HasPassword reports whether the user has a local Samuori password.
func (s *Service) HasPassword(ctx context.Context, userID string) (bool, error) {
	var n int
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM user_credentials WHERE user_id = ?`), userID).Scan(&n)
	return n > 0, err
}

func (s *Service) auditPassword(ctx context.Context, p authz.Principal, outcome, reason, requestID string) {
	_ = s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "user.password-change", ResourceType: "user", ResourceID: p.ID, Outcome: outcome, Reason: reason, RequestID: requestID})
	})
}
