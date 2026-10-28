package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/tenancy"
)

const (
	SessionCookie = "samuori_session"
	CSRFCookie    = "samuori_csrf"
	CSRFHeader    = "X-CSRF-Token"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrLocked             = errors.New("too many failed attempts; try again later")
	ErrRateLimited        = errors.New("too many requests; try again later")
	ErrCSRF               = errors.New("missing or invalid CSRF token")
	ErrBootstrapDone      = errors.New("setup has already been completed")
	ErrBootstrapToken     = errors.New("invalid or expired bootstrap token")
	ErrNotFound           = errors.New("not found")
)

type Policy struct {
	MaxFailures    int
	LockoutFor     time.Duration
	PerIPPerMinute int
	BootstrapTTL   time.Duration
	// DevWeakPasswords exempts local passwords from the policy (empty ones excepted); only a local all-in-one
	// development server sets it (config.DevWeakPasswords).
	DevWeakPasswords bool
}

func DefaultPolicy() Policy {
	return Policy{MaxFailures: 5, LockoutFor: 15 * time.Minute, PerIPPerMinute: 20, BootstrapTTL: 24 * time.Hour}
}

type Service struct {
	db      *db.DB
	tenancy *tenancy.Store
	policy  Policy
	now     func() time.Time

	directory DirectoryLogin

	mu    sync.Mutex
	perIP map[string][]time.Time
}

func NewService(d *db.DB, t *tenancy.Store, p Policy) *Service {
	return &Service{db: d, tenancy: t, policy: p, now: time.Now,
		perIP: map[string][]time.Time{}}
}

type Session struct {
	Token, CSRF string
	UserID      string
}

// DirectoryLogin authenticates against an external directory provider and returns the provisioned user.
type DirectoryLogin func(ctx context.Context, providerID, username, password, requestID string) (userID, orgID string, err error)

// UseDirectory enables sign-in through LDAP providers.
func (s *Service) UseDirectory(f DirectoryLogin) { s.directory = f }

// Login verifies credentials and always issues a fresh session; any session the client presented is ignored. With a
// provider ID the password is checked by that directory, under the same rate limits and lockout.
func (s *Service) Login(ctx context.Context, providerID, username, password, clientIP, requestID string) (Session, error) {
	now := s.now()
	if !s.allowIP(clientIP, now) {
		return Session{}, ErrRateLimited
	}
	key := username
	if providerID != "" {
		key = providerID + "/" + username
	}
	locked, err := s.locked(ctx, key, now)
	if err != nil {
		return Session{}, err
	}
	if locked {
		s.auditLogin(ctx, "", username, "denied", "account locked", requestID)
		return Session{}, ErrLocked
	}
	if providerID != "" {
		if s.directory == nil {
			return Session{}, ErrInvalidCredentials
		}
		userID, orgID, err := s.directory(ctx, providerID, username, password, requestID)
		if err != nil {
			s.fail(ctx, key, now)
			return Session{}, ErrInvalidCredentials
		}
		s.clearFailures(ctx, key)
		return s.IssueSession(ctx, userID, orgID, requestID, "directory sign-in")
	}
	var userID, orgID, hash string
	var disabled bool
	err = s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT u.id, u.org_id, u.disabled, c.password_hash FROM users u JOIN user_credentials c ON c.user_id = u.id WHERE u.username = ?`), username).
		Scan(&userID, &orgID, &disabled, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		VerifyPassword(dummyHash, password)
		s.fail(ctx, key, now)
		s.auditLogin(ctx, "", username, "denied", "unknown user", requestID)
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, err
	}
	if disabled || !VerifyPassword(hash, password) {
		s.fail(ctx, key, now)
		s.auditLogin(ctx, orgID, username, "denied", "bad credentials or disabled", requestID)
		return Session{}, ErrInvalidCredentials
	}
	recovery, err := s.localRestricted(ctx, orgID, userID)
	if err != nil {
		return Session{}, err
	}
	if recovery == restrictedDenied {
		s.auditLogin(ctx, orgID, username, "denied", "local passwords are limited to administrators; use the identity provider", requestID)
		return Session{}, ErrInvalidCredentials
	}
	s.clearFailures(ctx, key)
	reason := ""
	if recovery == restrictedRecovery {
		reason = "local recovery sign-in while local passwords are limited to administrators"
	}
	return s.IssueSession(ctx, userID, orgID, requestID, reason)
}

const (
	unrestricted = iota
	restrictedRecovery
	restrictedDenied
)

// localRestricted applies the organization's local sign-in policy: when it is "administrators", only organization
// administrators may use local passwords, as recovery access.
func (s *Service) localRestricted(ctx context.Context, orgID, userID string) (int, error) {
	var policy string
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT local_login FROM organizations WHERE id = ?`), orgID).Scan(&policy); err != nil {
		return 0, err
	}
	if policy != "administrators" {
		return unrestricted, nil
	}
	var n int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM role_bindings WHERE principal_id = ? AND role = 'administrator' AND scope_type = 'organization' AND scope_id = ?`), userID, orgID).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		return restrictedDenied, nil
	}
	return restrictedRecovery, nil
}

// IssueSession creates a session for a user already authenticated by a password, directory or OIDC provider.
func (s *Service) IssueSession(ctx context.Context, userID, orgID, requestID, reason string) (Session, error) {
	now := s.now()
	token, tokenHash := newSecret()
	csrf, csrfHash := newSecret()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO sessions (token_hash, user_id, csrf_hash, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`),
			tokenHash, userID, csrfHash, now.UTC(), now.UTC()); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{ID: userID, OrgID: orgID, Kind: authz.User}, Action: "auth.login", ResourceType: "session", Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, CSRF: csrf, UserID: userID}, nil
}

// SetLocalLogin sets who may sign in with local passwords: "all" or "administrators".
func (s *Service) SetLocalLogin(ctx context.Context, actor authz.Principal, policy, requestID string) error {
	if !actor.IsOrgAdmin() {
		return tenancy.ErrForbidden
	}
	if policy != "all" && policy != "administrators" {
		return errors.New("the local sign-in policy is all or administrators")
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE organizations SET local_login = ? WHERE id = ?`), policy, actor.OrgID); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "identity.local-login", ResourceType: "organization", ResourceID: actor.OrgID, Outcome: "succeeded", Reason: policy, RequestID: requestID})
	})
}

// LocalLogin returns the organization's local sign-in policy.
func (s *Service) LocalLogin(ctx context.Context, orgID string) (string, error) {
	policy := "all"
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT local_login FROM organizations WHERE id = ?`), orgID).Scan(&policy)
	if errors.Is(err, sql.ErrNoRows) {
		return "all", nil
	}
	return policy, err
}

func (s *Service) Logout(ctx context.Context, sessionToken string) error {
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`), s.now().UTC(), hashSecret(sessionToken))
	return err
}

// Authenticate implements the API guard's authenticator: bearer tokens first, then the session cookie.
func (s *Service) Authenticate(r *http.Request) (authz.Principal, bool, error) {
	ctx := r.Context()
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return s.authenticateToken(ctx, strings.TrimPrefix(h, "Bearer "))
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return authz.Principal{}, false, nil
	}
	now := s.now()
	row, err := s.loadSession(ctx, hashSecret(c.Value))
	if errors.Is(err, ErrNotFound) {
		return authz.Principal{}, false, nil
	}
	if err != nil {
		return authz.Principal{}, false, err
	}
	if row.expired(now) {
		return authz.Principal{}, false, nil
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if subtle.ConstantTimeCompare([]byte(hashSecret(r.Header.Get(CSRFHeader))), []byte(row.csrfHash)) != 1 {
			return authz.Principal{}, false, ErrCSRF
		}
		// Only state-changing requests count as activity; reads, including background polling, never renew the
		// idle deadline (docs/architecture/identity.md#sessions).
		if now.Sub(row.lastSeen) > time.Minute {
			_, _ = s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ? AND revoked_at IS NULL`), now.UTC(), row.tokenHash)
		}
	}
	userID := row.userID
	p, _, err := s.tenancy.LoadPrincipal(ctx, userID)
	if errors.Is(err, tenancy.ErrNotFound) {
		return authz.Principal{}, false, nil
	}
	return p, err == nil, err
}

type Token struct {
	ID, Name             string
	Scopes               []string
	ExpiresAt, CreatedAt time.Time
	LastUsedAt           *time.Time
	Value                string
}

func (s *Service) CreateToken(ctx context.Context, p authz.Principal, name string, scopes []string, expires time.Time, requestID string) (Token, error) {
	return s.createToken(ctx, p, p.ID, name, scopes, expires, requestID)
}

// CreateTokenFor issues a token owned by another principal, such as a service identity; actor is audited.
func (s *Service) CreateTokenFor(ctx context.Context, actor authz.Principal, ownerID, name string, scopes []string, expires time.Time, requestID string) (Token, error) {
	return s.createToken(ctx, actor, ownerID, name, scopes, expires, requestID)
}

// RevokeTokenFor revokes a token owned by ownerID; actor is audited.
func (s *Service) RevokeTokenFor(ctx context.Context, actor authz.Principal, ownerID, id, requestID string) error {
	return s.revokeToken(ctx, actor, ownerID, id, requestID)
}

// TokensOf lists the active tokens of a principal.
func (s *Service) TokensOf(ctx context.Context, ownerID string) ([]Token, error) {
	return s.ListTokens(ctx, authz.Principal{ID: ownerID})
}

func (s *Service) createToken(ctx context.Context, p authz.Principal, ownerID, name string, scopes []string, expires time.Time, requestID string) (Token, error) {
	value, hash := newSecret()
	t := Token{ID: ids.New(), Name: name, Scopes: scopes, ExpiresAt: expires.UTC(), CreatedAt: s.now().UTC(), Value: "samuori_" + value}
	hash = hashSecret(t.Value)
	scopesJSON, _ := json.Marshal(scopes)
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO api_tokens (id, user_id, name, token_hash, scopes, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			t.ID, ownerID, name, hash, string(scopesJSON), t.ExpiresAt, t.CreatedAt); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "token.create", ResourceType: "api_token", ResourceID: t.ID, Outcome: "succeeded", RequestID: requestID})
	})
	return t, err
}

func (s *Service) ListTokens(ctx context.Context, p authz.Principal) ([]Token, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, name, scopes, expires_at, created_at, last_used_at FROM api_tokens WHERE user_id = ? AND revoked_at IS NULL ORDER BY id`), p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var scopes string
		var last sql.NullTime
		if err := rows.Scan(&t.ID, &t.Name, &scopes, &t.ExpiresAt, &t.CreatedAt, &last); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(scopes), &t.Scopes)
		if last.Valid {
			t.LastUsedAt = &last.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) RevokeToken(ctx context.Context, p authz.Principal, id, requestID string) error {
	return s.revokeToken(ctx, p, p.ID, id, requestID)
}

func (s *Service) revokeToken(ctx context.Context, p authz.Principal, ownerID, id, requestID string) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`), s.now().UTC(), id, ownerID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "token.revoke", ResourceType: "api_token", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}

func (s *Service) authenticateToken(ctx context.Context, value string) (authz.Principal, bool, error) {
	var id, userID, scopes string
	var expires time.Time
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id, user_id, scopes, expires_at FROM api_tokens WHERE token_hash = ? AND revoked_at IS NULL`), hashSecret(value)).
		Scan(&id, &userID, &scopes, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !s.now().Before(expires)) {
		return authz.Principal{}, false, nil
	}
	if err != nil {
		return authz.Principal{}, false, err
	}
	_, _ = s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`), s.now().UTC(), id)
	p, _, err := s.tenancy.LoadPrincipal(ctx, userID)
	if errors.Is(err, tenancy.ErrNotFound) {
		return authz.Principal{}, false, nil
	}
	if err != nil {
		return authz.Principal{}, false, err
	}
	var list []string
	_ = json.Unmarshal([]byte(scopes), &list)
	p.ReadOnly = !contains(list, "write")
	return p, true, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// AllowOIDCStart rate-limits OIDC sign-in starts per address, in a budget separate from password attempts.
func (s *Service) AllowOIDCStart(ip string) bool { return s.allowIP("oidc:"+ip, s.now()) }

func (s *Service) allowIP(ip string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	recent := prune(s.perIP[ip], now.Add(-time.Minute))
	if len(recent) >= s.policy.PerIPPerMinute {
		s.perIP[ip] = recent
		return false
	}
	s.perIP[ip] = append(recent, now)
	return true
}

// Failed sign-ins and lockouts are kept in the database, so every controller of a cluster enforces the same lockout.
func (s *Service) locked(ctx context.Context, account string, now time.Time) (bool, error) {
	var n int
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM login_lockouts WHERE account = ? AND locked_until > ?`), account, now.UTC()).Scan(&n)
	return n > 0, err
}

func (s *Service) fail(ctx context.Context, account string, now time.Time) {
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		since := now.Add(-s.policy.LockoutFor).UTC()
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM login_failures WHERE account = ? AND failed_at <= ?`), account, since); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO login_failures (account, failed_at) VALUES (?, ?)`), account, now.UTC()); err != nil {
			return err
		}
		var recent int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM login_failures WHERE account = ? AND failed_at > ?`), account, since).Scan(&recent); err != nil {
			return err
		}
		if recent < s.policy.MaxFailures {
			return nil
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO login_lockouts (account, locked_until) VALUES (?, ?) ON CONFLICT (account) DO UPDATE SET locked_until = excluded.locked_until`),
			account, now.Add(s.policy.LockoutFor).UTC())
		return err
	})
	if err != nil {
		slog.Default().Error("recording a failed sign-in", "error", err)
	}
}

func (s *Service) clearFailures(ctx context.Context, account string) {
	_, _ = s.db.SQL.ExecContext(ctx, s.db.Rebind(`DELETE FROM login_failures WHERE account = ?`), account)
}

func prune(ts []time.Time, after time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(after) {
			out = append(out, t)
		}
	}
	return out
}

func (s *Service) auditLogin(ctx context.Context, orgID, username, outcome, reason, requestID string) {
	_ = s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{OrgID: orgID, Kind: authz.System}, Action: "auth.login", ResourceType: "user", ResourceID: username, Outcome: outcome, Reason: reason, RequestID: requestID})
	})
}

// dummyHash makes unknown-user logins cost the same as real ones.
var dummyHash, _ = HashPassword("samuori-timing-equalizer-not-a-password")

// ClientIP uses the connection address; forwarded headers are not trusted until proxy support is configured.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Service) SetupCompleted(ctx context.Context) (bool, error) {
	var n int
	err := s.db.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM role_bindings WHERE role = 'administrator' AND scope_type = 'organization'`).Scan(&n)
	return n > 0, err
}

// EnsureBootstrapToken writes a single-use token to dataDir/bootstrap-token (0600) when setup is incomplete.
// The token itself is never logged; only its path is returned.
func (s *Service) EnsureBootstrapToken(ctx context.Context, dataDir string) (string, error) {
	return s.ensureBootstrapToken(ctx, dataDir, false)
}

// EnsureSharedBootstrapToken is EnsureBootstrapToken for controllers sharing a database: a controller issues a token
// only when no unexpired one exists, so a starting peer never invalidates the token another controller wrote. It
// returns an empty path when a peer holds the current token.
func (s *Service) EnsureSharedBootstrapToken(ctx context.Context, dataDir string) (string, error) {
	return s.ensureBootstrapToken(ctx, dataDir, true)
}

func (s *Service) ensureBootstrapToken(ctx context.Context, dataDir string, shared bool) (string, error) {
	done, err := s.SetupCompleted(ctx)
	if err != nil || done {
		return "", err
	}
	token, hash := newSecret()
	path := filepath.Join(dataDir, "bootstrap-token")
	held := false
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if shared {
			var n int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM bootstrap_tokens WHERE expires_at > ? AND used_at IS NULL`), s.now().UTC()).Scan(&n); err != nil {
				return err
			}
			if held = n > 0; held {
				return nil
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM bootstrap_tokens`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO bootstrap_tokens (token_hash, expires_at) VALUES (?, ?)`), hash, s.now().Add(s.policy.BootstrapTTL).UTC())
		return err
	})
	if err != nil || held {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// CreateLocalUser adds a password-authenticated user with no roles; administrators grant roles separately.
func (s *Service) CreateLocalUser(ctx context.Context, actor authz.Principal, username, displayName, password, requestID string) (string, error) {
	if !actor.IsOrgAdmin() {
		return "", tenancy.ErrForbidden
	}
	if err := s.checkPassword(username, password); err != nil {
		return "", err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	if displayName == "" {
		displayName = username
	}
	id, now := ids.New(), s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO users (id, org_id, username, display_name, disabled, created_at) VALUES (?, ?, ?, ?, ?, ?)`), id, actor.OrgID, username, displayName, false, now); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(err.Error(), "duplicate key") {
				return tenancy.ErrConflict
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO user_credentials (user_id, password_hash, updated_at) VALUES (?, ?, ?)`), id, hash, now); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "user.create", ResourceType: "user", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
	return id, err
}

func (s *Service) checkPassword(username, password string) error {
	if s.policy.DevWeakPasswords {
		if password == "" {
			return ErrWeakPassword
		}
		return nil
	}
	return CheckPasswordPolicy(username, password)
}

type BootstrapInput struct {
	Token, OrganizationName, Username, DisplayName, Email, Password string
}

// Bootstrap creates the organization and first administrator; the token is consumed in the same transaction.
func (s *Service) Bootstrap(ctx context.Context, in BootstrapInput, dataDir, requestID string, requireToken bool) error {
	if err := s.checkPassword(in.Username, in.Password); err != nil {
		return err
	}
	if done, err := s.SetupCompleted(ctx); err != nil || done {
		if err != nil {
			return err
		}
		return ErrBootstrapDone
	}
	hash, err := HashPassword(in.Password)
	if err != nil {
		return err
	}
	if in.DisplayName == "" {
		in.DisplayName = in.Username
	}
	now := s.now().UTC()
	orgID, userID := ids.New(), ids.New()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if requireToken {
			res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE bootstrap_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`), now, hashSecret(strings.TrimSpace(in.Token)), now)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrBootstrapToken
			}
		}
		stmts := []struct {
			q    string
			args []any
		}{
			{`INSERT INTO organizations (id, name, created_at) VALUES (?, ?, ?)`, []any{orgID, in.OrganizationName, now}},
			{`INSERT INTO locations (id, org_id, name, description, created_at) VALUES (?, ?, 'default', '', ?)`, []any{ids.New(), orgID, now}},
			{`INSERT INTO users (id, org_id, username, display_name, email, disabled, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, []any{userID, orgID, in.Username, in.DisplayName, in.Email, false, now}},
			{`INSERT INTO user_credentials (user_id, password_hash, updated_at) VALUES (?, ?, ?)`, []any{userID, hash, now}},
			{`INSERT INTO role_bindings (id, org_id, principal_id, role, scope_type, scope_id, created_at) VALUES (?, ?, ?, 'administrator', 'organization', ?, ?)`, []any{ids.New(), orgID, userID, orgID, now}},
		}
		for _, st := range stmts {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(st.q), st.args...); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{ID: userID, OrgID: orgID, Kind: authz.User}, Action: "setup.bootstrap", ResourceType: "organization", ResourceID: orgID, Outcome: "succeeded", RequestID: requestID})
	})
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dataDir, "bootstrap-token"))
	return nil
}
