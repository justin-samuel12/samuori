package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"unicode"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// Renewing a session re-authenticates its user and replaces it with a new session; the old one is revoked and the
// maximum still bounds each session (ADR 0068, docs/architecture/identity.md#sessions).

var (
	ErrRenewWithProvider = errors.New("this account signs in with an identity provider; renew through the provider")
	ErrDifferentUser     = errors.New("a session can only be renewed by the user it belongs to")
)

const (
	RenewWithPassword = "password"
	RenewWithProvider = "provider"
)

// Renewal says how a session's user re-authenticates: with a password (local or LDAP) or through an OIDC provider.
type Renewal struct {
	Method, ProviderID, ProviderName string
}

type renewalAccount struct {
	userID, orgID, username, hash string
	disabled, hasCredential       bool
	temporary                     sql.NullTime
	providerID, providerKind      string
	providerName                  string
}

func (s *Service) renewalAccount(ctx context.Context, userID string) (renewalAccount, error) {
	a := renewalAccount{userID: userID}
	var hash, providerID, kind, name sql.NullString
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT u.org_id, u.username, u.disabled, c.password_hash, c.temporary_until, u.provider_id, p.kind, p.display_name
		FROM users u LEFT JOIN user_credentials c ON c.user_id = u.id LEFT JOIN identity_providers p ON p.id = u.provider_id
		WHERE u.id = ? AND u.kind = 'user'`), userID).
		Scan(&a.orgID, &a.username, &a.disabled, &hash, &a.temporary, &providerID, &kind, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.hash, a.hasCredential = hash.String, hash.Valid
	a.providerID, a.providerKind, a.providerName = providerID.String, kind.String, name.String
	return a, err
}

// RenewalFor reports how the user of a browser session renews it.
func (s *Service) RenewalFor(ctx context.Context, userID string) (Renewal, error) {
	a, err := s.renewalAccount(ctx, userID)
	if err != nil {
		return Renewal{}, err
	}
	if !a.hasCredential && a.providerKind == "oidc" {
		return Renewal{Method: RenewWithProvider, ProviderID: a.providerID, ProviderName: a.providerName}, nil
	}
	return Renewal{Method: RenewWithPassword, ProviderName: a.providerName}, nil
}

// RenewSession re-authenticates the user of a live session with their password, under the sign-in rate limit and
// lockout, and replaces the session with a new one. Only the session's own user can renew it; OIDC accounts renew
// through their provider (ErrRenewWithProvider).
func (s *Service) RenewSession(ctx context.Context, sessionToken, password, clientIP, requestID string) (Session, SessionStatus, error) {
	now := s.now()
	if !s.allowIP(clientIP, now) {
		return Session{}, SessionStatus{}, ErrRateLimited
	}
	row, err := s.loadSession(ctx, hashSecret(sessionToken))
	if err != nil {
		return Session{}, SessionStatus{}, err
	}
	if row.expired(now) {
		return Session{}, SessionStatus{}, ErrNotFound
	}
	a, err := s.renewalAccount(ctx, row.userID)
	if err != nil {
		return Session{}, SessionStatus{}, err
	}
	actor := authz.Principal{ID: a.userID, OrgID: a.orgID, Kind: authz.User}
	key := a.username
	directory := !a.hasCredential && a.providerKind == "ldap"
	switch {
	case directory:
		key = a.providerID + "/" + a.username
	case !a.hasCredential:
		return Session{}, SessionStatus{}, ErrRenewWithProvider
	}
	locked, err := s.locked(ctx, key, now)
	if err != nil {
		return Session{}, SessionStatus{}, err
	}
	if locked {
		s.auditRenewal(ctx, actor, "denied", "account locked", requestID)
		return Session{}, SessionStatus{}, ErrLocked
	}
	if a.disabled {
		s.fail(ctx, key, now)
		s.auditRenewal(ctx, actor, "denied", "the account is disabled", requestID)
		return Session{}, SessionStatus{}, ErrInvalidCredentials
	}
	if directory {
		if s.directory == nil {
			return Session{}, SessionStatus{}, ErrInvalidCredentials
		}
		userID, _, err := s.directory(ctx, a.providerID, a.username, password, requestID)
		if err != nil || userID != a.userID {
			s.fail(ctx, key, now)
			s.auditRenewal(ctx, actor, "denied", "wrong password", requestID)
			return Session{}, SessionStatus{}, ErrWrongPassword
		}
	} else {
		if !VerifyPassword(a.hash, password) {
			s.fail(ctx, key, now)
			s.auditRenewal(ctx, actor, "denied", "wrong password", requestID)
			return Session{}, SessionStatus{}, ErrWrongPassword
		}
		if a.temporary.Valid {
			s.auditRenewal(ctx, actor, "denied", "temporary password; a new password is required", requestID)
			return Session{}, SessionStatus{}, ErrPasswordChangeRequired
		}
		restricted, err := s.localRestricted(ctx, a.orgID, a.userID)
		if err != nil {
			return Session{}, SessionStatus{}, err
		}
		if restricted == restrictedDenied {
			s.auditRenewal(ctx, actor, "denied", "local passwords are limited to administrators; use the identity provider", requestID)
			return Session{}, SessionStatus{}, ErrInvalidCredentials
		}
	}
	s.clearFailures(ctx, key)
	return s.ReplaceSession(ctx, SessionHash(sessionToken), a.userID, requestID, "password")
}

// SessionHash is the stored form of a session token.
func SessionHash(token string) string { return hashSecret(token) }

// ReplaceSession revokes a live session and issues a new one for the same, already re-authenticated user in one
// transaction. A session that has ended, or belongs to another user, is not replaced.
func (s *Service) ReplaceSession(ctx context.Context, oldHash, userID, requestID, reason string) (Session, SessionStatus, error) {
	token, tokenHash := newSecret()
	csrf, csrfHash := newSecret()
	var status SessionStatus
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		old, err := s.loadSessionWith(ctx, tx, oldHash)
		if err != nil {
			return err
		}
		if old.expired(now) {
			return ErrNotFound
		}
		if old.userID != userID {
			return ErrDifferentUser
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`), now.UTC(), oldHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO sessions (token_hash, user_id, csrf_hash, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`),
			tokenHash, userID, csrfHash, now.UTC(), now.UTC()); err != nil {
			return err
		}
		fresh, err := s.loadSessionWith(ctx, tx, tokenHash)
		if err != nil {
			return err
		}
		status = fresh.status(now)
		var orgID string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT org_id FROM users WHERE id = ?`), userID).Scan(&orgID); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{ID: userID, OrgID: orgID, Kind: authz.User}, Action: "session.renew", ResourceType: "session", Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Session{}, SessionStatus{}, err
	}
	return Session{Token: token, CSRF: csrf, UserID: userID}, status, nil
}

func (s *Service) auditRenewal(ctx context.Context, p authz.Principal, outcome, reason, requestID string) {
	_ = s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "session.renew", ResourceType: "session", Outcome: outcome, Reason: reason, RequestID: requestID})
	})
}

// SafeReturnTo accepts only a same-origin path of the web UI, such as /projects/abc?tab=builds, so a sign-in can never
// redirect to another site. It returns "/" and false for anything else.
func SafeReturnTo(v string) (string, bool) {
	if v == "" || len(v) > 2048 || v[0] != '/' || strings.HasPrefix(v, "//") || strings.ContainsAny(v, "\\") {
		return "/", false
	}
	for _, r := range v {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "/", false
		}
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return "/", false
	}
	if u.Path == "/api" || strings.HasPrefix(u.Path, "/api/") {
		return "/", false
	}
	return v, true
}
