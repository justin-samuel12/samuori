package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

// ErrAuthFailed is the only error a failed external sign-in reports to the browser; the audit log has the cause.
var ErrAuthFailed = errors.New("sign-in with the identity provider failed")

// Claims is what a provider asserts about a signed-in person.
type Claims struct {
	Subject, Username, DisplayName string
	Groups                         []string
}

// OIDCClient performs the authorization code flow. Exchange must verify the ID token's signature, issuer, audience,
// expiry and nonce before returning claims (SEC-AUTH-05).
type OIDCClient interface {
	AuthCodeURL(ctx context.Context, p Provider, redirectURL, state, nonce, challenge string) (string, error)
	Exchange(ctx context.Context, p Provider, clientSecret, redirectURL, code, verifier, nonce string) (Claims, error)
	Check(ctx context.Context, p Provider) error
}

// Directory authenticates against LDAP. Authenticate must escape the username in filters (SEC-AUTH-06) and refuse
// empty passwords, which LDAP treats as unauthenticated binds.
type Directory interface {
	Authenticate(ctx context.Context, p Provider, bindPassword, username, password string) (Claims, error)
	Check(ctx context.Context, p Provider, bindPassword string) error
}

// Service runs external sign-ins.
type Service struct {
	store       *Store
	oidc        OIDCClient
	dir         Directory
	redirectURL string
	now         func() time.Time
}

// loginTTL bounds how long a browser may take at the identity provider.
const loginTTL = 10 * time.Minute

// NewService wires the adapters. redirectURL is the OIDC callback, derived from the configured external URL; empty
// disables OIDC sign-in.
func NewService(st *Store, oidc OIDCClient, dir Directory, redirectURL string) *Service {
	return &Service{store: st, oidc: oidc, dir: dir, redirectURL: redirectURL, now: time.Now}
}

func (s *Service) RedirectURL() string { return s.redirectURL }

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// OIDCStart says where a sign-in returns and, for a renewal, which session and user it renews (ADR 0068). ReturnTo must
// already be a validated same-origin path (auth.SafeReturnTo).
type OIDCStart struct {
	ReturnTo, RenewSessionHash, RenewUserID string
}

// OIDCResult is a finished sign-in: the user, with the start's return path and renewal binding.
type OIDCResult struct {
	UserID, OrgID string
	OIDCStart
}

// StartOIDC begins a sign-in. It returns the provider URL and a browser token that the callback must present, so a
// login started in one browser cannot be completed in another.
func (s *Service) StartOIDC(ctx context.Context, providerID string, start OIDCStart) (authURL, browser string, err error) {
	if s.redirectURL == "" || s.oidc == nil {
		return "", "", &Invalid{"OIDC sign-in needs the controller's external URL to be configured"}
	}
	p, err := s.store.byID(ctx, providerID)
	if err != nil || p.Kind != OIDC {
		return "", "", ErrNotFound
	}
	if !p.Enabled {
		return "", "", ErrDisabled
	}
	state, nonce, verifier := random(), random(), random()
	browser = random()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	now := s.now().UTC()
	db := s.store.db
	if _, err := db.SQL.ExecContext(ctx, db.Rebind(`DELETE FROM oidc_logins WHERE expires_at < ?`), now.Add(-time.Hour)); err != nil {
		return "", "", err
	}
	if start.ReturnTo == "" {
		start.ReturnTo = "/"
	}
	if _, err := db.SQL.ExecContext(ctx, db.Rebind(`INSERT INTO oidc_logins (state_hash, provider_id, browser_hash, nonce, verifier, created_at, expires_at, return_to, renew_session_hash, renew_user_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		hash(state), p.ID, hash(browser), nonce, verifier, now, now.Add(loginTTL), start.ReturnTo, nullable(start.RenewSessionHash), nullable(start.RenewUserID)); err != nil {
		return "", "", err
	}
	authURL, err = s.oidc.AuthCodeURL(ctx, p, s.redirectURL, state, nonce, challenge)
	return authURL, browser, err
}

// FinishOIDC completes a sign-in from the provider's callback. The state is single-use, expires, and must belong to
// the same browser. A renewal must come back as the user it was started for.
func (s *Service) FinishOIDC(ctx context.Context, state, code, browser, requestID string) (OIDCResult, error) {
	var out OIDCResult
	db := s.store.db
	now := s.now().UTC()
	res, err := db.SQL.ExecContext(ctx, db.Rebind(`UPDATE oidc_logins SET used_at = ? WHERE state_hash = ? AND used_at IS NULL AND expires_at > ?`), now, hash(state), now)
	if err != nil {
		return out, err
	}
	if n, _ := res.RowsAffected(); n != 1 || state == "" {
		s.deny(ctx, "", "", "the sign-in state is unknown, expired or already used", requestID)
		return out, ErrAuthFailed
	}
	var providerID, browserHash, nonce, verifier string
	var renewSession, renewUser sql.NullString
	if err := db.SQL.QueryRowContext(ctx, db.Rebind(`SELECT provider_id, browser_hash, nonce, verifier, return_to, renew_session_hash, renew_user_id FROM oidc_logins WHERE state_hash = ?`), hash(state)).
		Scan(&providerID, &browserHash, &nonce, &verifier, &out.ReturnTo, &renewSession, &renewUser); err != nil {
		return out, err
	}
	out.RenewSessionHash, out.RenewUserID = renewSession.String, renewUser.String
	p, err := s.store.byID(ctx, providerID)
	if err != nil {
		return out, err
	}
	if subtle.ConstantTimeCompare([]byte(hash(browser)), []byte(browserHash)) != 1 {
		s.deny(ctx, p.OrgID, p.ID, "the sign-in was finished in a different browser than it started", requestID)
		return out, ErrAuthFailed
	}
	if !p.Enabled {
		s.deny(ctx, p.OrgID, p.ID, "the provider is disabled", requestID)
		return out, ErrAuthFailed
	}
	secret, err := s.secret(ctx, p)
	if err != nil {
		return out, err
	}
	claims, err := s.oidc.Exchange(ctx, p, secret, s.redirectURL, code, verifier, nonce)
	if err != nil {
		s.deny(ctx, p.OrgID, p.ID, clip(err.Error()), requestID)
		return out, ErrAuthFailed
	}
	if out.RenewUserID != "" {
		var subject string
		err := db.SQL.QueryRowContext(ctx, db.Rebind(`SELECT external_id FROM users WHERE id = ? AND provider_id = ?`), out.RenewUserID, p.ID).Scan(&subject)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		if err != nil || subject != claims.Subject {
			s.deny(ctx, p.OrgID, p.ID, "the renewal was completed by a different account than the session's", requestID)
			return out, ErrAuthFailed
		}
	}
	out.UserID, err = s.provision(ctx, p, claims, requestID)
	out.OrgID = p.OrgID
	return out, err
}

// LDAPLogin authenticates a username and password against an LDAP provider.
func (s *Service) LDAPLogin(ctx context.Context, providerID, username, password, requestID string) (userID, orgID string, err error) {
	p, err := s.store.byID(ctx, providerID)
	if err != nil || p.Kind != LDAP || s.dir == nil {
		return "", "", ErrAuthFailed
	}
	if !p.Enabled {
		s.deny(ctx, p.OrgID, p.ID, "the provider is disabled", requestID)
		return "", "", ErrAuthFailed
	}
	if password == "" || username == "" {
		s.deny(ctx, p.OrgID, p.ID, "empty username or password", requestID)
		return "", "", ErrAuthFailed
	}
	secret, err := s.secret(ctx, p)
	if err != nil {
		return "", "", err
	}
	claims, err := s.dir.Authenticate(ctx, p, secret, username, password)
	if err != nil {
		s.deny(ctx, p.OrgID, p.ID, clip(err.Error()), requestID)
		return "", "", ErrAuthFailed
	}
	userID, err = s.provision(ctx, p, claims, requestID)
	return userID, p.OrgID, err
}

// Test checks a provider's configuration and connectivity without signing anyone in.
func (s *Service) Test(ctx context.Context, actor authz.Principal, providerID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	p, err := s.store.Get(ctx, actor.OrgID, providerID)
	if err != nil {
		return err
	}
	switch p.Kind {
	case OIDC:
		if s.oidc == nil {
			return errors.New("OIDC is not available")
		}
		return s.oidc.Check(ctx, p)
	default:
		if s.dir == nil {
			return errors.New("LDAP is not available")
		}
		secret, err := s.secret(ctx, p)
		if err != nil {
			return err
		}
		return s.dir.Check(ctx, p, secret)
	}
}

func (s *Service) secret(ctx context.Context, p Provider) (string, error) {
	if p.SecretID == "" {
		return "", nil
	}
	v, err := s.store.secrets.ControllerValue(ctx, p.OrgID, p.SecretID)
	if err != nil {
		return "", fmt.Errorf("the provider's secret is unavailable")
	}
	return string(v), nil
}

var usernameChars = regexp.MustCompile(`[^a-z0-9._@-]+`)

func username(c Claims) string {
	u := strings.ToLower(strings.TrimSpace(c.Username))
	if u == "" {
		u = strings.ToLower(c.Subject)
	}
	u = usernameChars.ReplaceAllString(u, "-")
	if len(u) > 64 {
		u = u[:64]
	}
	return u
}

// provision finds or creates the user linked to the provider's subject and replaces the bindings that group
// mappings grant. An existing local account with the same username is never linked automatically.
func (s *Service) provision(ctx context.Context, p Provider, c Claims, requestID string) (string, error) {
	if c.Subject == "" {
		s.deny(ctx, p.OrgID, p.ID, "the provider asserted no subject", requestID)
		return "", ErrAuthFailed
	}
	maps, err := s.store.mappings(ctx, p.OrgID, p.ID)
	if err != nil {
		return "", err
	}
	db := s.store.db
	now := s.now().UTC()
	var userID string
	denied := ""
	err = db.InTx(ctx, func(tx *sql.Tx) error {
		var disabled bool
		err := tx.QueryRowContext(ctx, db.Rebind(`SELECT id, disabled FROM users WHERE provider_id = ? AND external_id = ?`), p.ID, c.Subject).Scan(&userID, &disabled)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			name := username(c)
			if name == "" {
				denied = "the provider asserted no usable username"
				return nil
			}
			var taken int
			if err := tx.QueryRowContext(ctx, db.Rebind(`SELECT COUNT(*) FROM users WHERE org_id = ? AND username = ?`), p.OrgID, name).Scan(&taken); err != nil {
				return err
			}
			if taken > 0 {
				denied = "the username " + name + " already belongs to another account"
				return nil
			}
			display := c.DisplayName
			if display == "" {
				display = name
			}
			userID = ids.New()
			if _, err := tx.ExecContext(ctx, db.Rebind(`INSERT INTO users (id, org_id, username, display_name, disabled, created_at, kind, provider_id, external_id) VALUES (?, ?, ?, ?, ?, ?, 'user', ?, ?)`),
				userID, p.OrgID, name, clipN(display, 128), false, now, p.ID, c.Subject); err != nil {
				return err
			}
			if err := audit.Append(ctx, db, tx, audit.Event{Actor: authz.Principal{OrgID: p.OrgID, Kind: authz.System}, Action: "user.provision", ResourceType: "user", ResourceID: userID, Outcome: "succeeded", Reason: p.Name + ": " + name, RequestID: requestID}); err != nil {
				return err
			}
		case err != nil:
			return err
		case disabled:
			denied = "the account is disabled"
			return nil
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(`DELETE FROM role_bindings WHERE principal_id = ? AND source_provider_id = ?`), userID, p.ID); err != nil {
			return err
		}
		granted := []string{}
		for _, m := range maps {
			if !memberOf(p.Kind, c.Groups, m.Group) {
				continue
			}
			res, err := tx.ExecContext(ctx, db.Rebind(`INSERT INTO role_bindings (id, org_id, principal_id, role, scope_type, scope_id, created_at, source_provider_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`),
				ids.New(), p.OrgID, userID, string(m.Binding.Role), string(m.Binding.ScopeType), m.Binding.ScopeID, now, p.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				granted = append(granted, fmt.Sprintf("%s@%s", m.Binding.Role, m.Binding.ScopeType))
			}
		}
		return audit.Append(ctx, db, tx, audit.Event{Actor: authz.Principal{ID: userID, OrgID: p.OrgID, Kind: authz.User}, Action: "identity.sync", ResourceType: "user", ResourceID: userID, Outcome: "succeeded",
			Reason: clip(p.Name + " roles from groups: " + strings.Join(granted, ", ")), RequestID: requestID})
	})
	if err != nil {
		return "", err
	}
	if denied != "" {
		s.deny(ctx, p.OrgID, p.ID, denied, requestID)
		return "", ErrAuthFailed
	}
	return userID, nil
}

func (s *Service) deny(ctx context.Context, orgID, providerID, reason, requestID string) {
	db := s.store.db
	_ = db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, db, tx, audit.Event{Actor: authz.Principal{OrgID: orgID, Kind: authz.System}, Action: "auth.login", ResourceType: "identity-provider", ResourceID: providerID, Outcome: "denied", Reason: clip(reason), RequestID: requestID})
	})
}

func clip(s string) string { return clipN(s, 500) }

func clipN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// memberOf matches directory group names case-insensitively, as LDAP compares them; OIDC claims match exactly.
func memberOf(kind string, groups []string, group string) bool {
	if kind != LDAP {
		return slices.Contains(groups, group)
	}
	return slices.ContainsFunc(groups, func(g string) bool { return strings.EqualFold(g, group) })
}
