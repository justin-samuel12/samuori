// Package identity connects external identity providers, OIDC and LDAP, to Samuori's users and roles
// (docs/architecture/identity.md). Protocol work lives in adapters behind OIDC and Directory; this package keeps
// configuration, group mappings, provisioning and role synchronization.
package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/secrets"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("already exists")
	ErrDisabled  = errors.New("the identity provider is disabled")
)

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

const (
	OIDC = "oidc"
	LDAP = "ldap"
)

// OIDCConfig configures an OpenID Connect provider using the authorization code flow with PKCE.
type OIDCConfig struct {
	Issuer           string   `json:"issuer"`
	ClientID         string   `json:"clientId"`
	Scopes           []string `json:"scopes,omitempty"`
	UsernameClaim    string   `json:"usernameClaim,omitempty"`
	DisplayNameClaim string   `json:"displayNameClaim,omitempty"`
	GroupsClaim      string   `json:"groupsClaim,omitempty"`
}

// LDAPConfig configures an LDAP or Active Directory server. Filters use {username} and {dn}, which are always
// escaped before substitution (SEC-AUTH-06).
type LDAPConfig struct {
	URL                  string `json:"url"`
	StartTLS             bool   `json:"startTls,omitempty"`
	CACertPEM            string `json:"caCertPem,omitempty"`
	BindDN               string `json:"bindDn"`
	UserBaseDN           string `json:"userBaseDn"`
	UserFilter           string `json:"userFilter"`
	UsernameAttribute    string `json:"usernameAttribute,omitempty"`
	DisplayNameAttribute string `json:"displayNameAttribute,omitempty"`
	GroupBaseDN          string `json:"groupBaseDn,omitempty"`
	GroupFilter          string `json:"groupFilter,omitempty"`
	GroupNameAttribute   string `json:"groupNameAttribute,omitempty"`
}

// Provider is an organization's identity provider. SecretID names the OIDC client secret or the LDAP bind password,
// a controller-only secret.
type Provider struct {
	ID, OrgID, Kind, Name, DisplayName string
	Enabled                            bool
	OIDC                               *OIDCConfig
	LDAP                               *LDAPConfig
	SecretID                           string
	CreatedAt, UpdatedAt               time.Time
}

// Mapping grants a role to members of a provider group. Bindings it creates are replaced at every sign-in.
type Mapping struct {
	ID, ProviderID, Group string
	Binding               authz.Binding
}

// Store manages providers and mappings.
type Store struct {
	db      *db.DB
	secrets *secrets.Service
	now     func() time.Time
}

func NewStore(d *db.DB, sec *secrets.Service) *Store {
	return &Store{db: d, secrets: sec, now: time.Now}
}

var name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (c *OIDCConfig) normalize() error {
	u, err := url.Parse(c.Issuer)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname()))) {
		return &Invalid{"the issuer must be an https URL (http only on loopback, for testing)"}
	}
	if c.ClientID == "" || len(c.ClientID) > 256 {
		return &Invalid{"a client ID is required"}
	}
	if !slices.Contains(c.Scopes, "openid") {
		c.Scopes = append([]string{"openid"}, c.Scopes...)
	}
	if c.UsernameClaim == "" {
		c.UsernameClaim = "preferred_username"
	}
	if c.DisplayNameClaim == "" {
		c.DisplayNameClaim = "name"
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	return nil
}

// URLs lists the directory servers in the order they are tried.
func (c *LDAPConfig) URLs() []string {
	return strings.Fields(strings.ReplaceAll(c.URL, ",", " "))
}

func (c *LDAPConfig) normalize() error {
	urls := c.URLs()
	if len(urls) == 0 || len(urls) > 8 {
		return &Invalid{"give one to eight server URLs, such as ldaps://dc1.example.com ldaps://dc2.example.com"}
	}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "ldaps" && u.Scheme != "ldap") {
			return &Invalid{"each URL must be ldaps://host or ldap://host: " + raw}
		}
		if u.Scheme == "ldap" && !c.StartTLS && !loopback(u.Hostname()) {
			return &Invalid{"plain ldap:// needs StartTLS unless the server is on loopback; passwords are never sent unencrypted"}
		}
	}
	c.URL = strings.Join(urls, " ")
	if c.BindDN == "" || c.UserBaseDN == "" {
		return &Invalid{"a bind DN and a user base DN are required"}
	}
	if strings.Count(c.UserFilter, "{username}") != 1 || !strings.HasPrefix(c.UserFilter, "(") {
		return &Invalid{"the user filter must contain {username} once, such as (&(objectClass=person)(uid={username}))"}
	}
	if c.GroupFilter != "" && (c.GroupBaseDN == "" || !strings.Contains(c.GroupFilter, "{dn}")) {
		return &Invalid{"a group filter needs a group base DN and must contain {dn}, such as (member={dn})"}
	}
	if c.UsernameAttribute == "" {
		c.UsernameAttribute = "uid"
	}
	if c.DisplayNameAttribute == "" {
		c.DisplayNameAttribute = "cn"
	}
	if c.GroupNameAttribute == "" {
		c.GroupNameAttribute = "cn"
	}
	return nil
}

func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (p *Provider) config() (string, error) {
	var v any
	switch p.Kind {
	case OIDC:
		if p.OIDC == nil {
			return "", &Invalid{"OIDC settings are required"}
		}
		if err := p.OIDC.normalize(); err != nil {
			return "", err
		}
		v = p.OIDC
	case LDAP:
		if p.LDAP == nil {
			return "", &Invalid{"LDAP settings are required"}
		}
		if err := p.LDAP.normalize(); err != nil {
			return "", err
		}
		v = p.LDAP
	default:
		return "", &Invalid{"kind must be oidc or ldap"}
	}
	raw, err := json.Marshal(v)
	return string(raw), err
}

// Create adds a disabled provider; an administrator enables it after testing.
func (s *Store) Create(ctx context.Context, actor authz.Principal, p Provider, requestID string) (Provider, error) {
	if !actor.IsOrgAdmin() {
		return Provider{}, ErrForbidden
	}
	if !name.MatchString(p.Name) {
		return Provider{}, &Invalid{"names are lower-case letters, digits and dashes"}
	}
	if p.DisplayName == "" {
		p.DisplayName = p.Name
	}
	if len(p.DisplayName) > 64 {
		return Provider{}, &Invalid{"the display name is at most 64 characters"}
	}
	cfg, err := p.config()
	if err != nil {
		return Provider{}, err
	}
	if p.SecretID == "" && p.Kind == LDAP {
		return Provider{}, &Invalid{"LDAP needs a bind password secret"}
	}
	p.ID, p.OrgID, p.Enabled = ids.New(), actor.OrgID, false
	p.CreatedAt = s.now().UTC()
	p.UpdatedAt = p.CreatedAt
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if p.SecretID != "" {
			if err := s.secrets.MarkControllerOnly(ctx, tx, actor.OrgID, p.SecretID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO identity_providers (id, org_id, kind, name, display_name, enabled, config, secret_id, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			p.ID, p.OrgID, p.Kind, p.Name, p.DisplayName, false, cfg, nullable(p.SecretID), actor.ID, p.CreatedAt, p.UpdatedAt); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "identity-provider.create", ResourceType: "identity-provider", ResourceID: p.ID, Outcome: "succeeded", Reason: p.Kind + " " + p.Name, RequestID: requestID})
	})
	if err != nil {
		var inv *secrets.Invalid
		if errors.As(err, &inv) {
			return Provider{}, &Invalid{inv.Reason}
		}
		return Provider{}, err
	}
	return p, nil
}

// SetEnabled turns a provider on or off. Disabling it stops new sign-ins; existing sessions keep their expiry.
func (s *Store) SetEnabled(ctx context.Context, actor authz.Principal, id string, enabled bool, requestID string) (Provider, error) {
	if !actor.IsOrgAdmin() {
		return Provider{}, ErrForbidden
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE identity_providers SET enabled = ?, updated_at = ? WHERE id = ? AND org_id = ?`), enabled, s.now().UTC(), id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		action := "identity-provider.disable"
		if enabled {
			action = "identity-provider.enable"
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: action, ResourceType: "identity-provider", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
	if err != nil {
		return Provider{}, err
	}
	return s.Get(ctx, actor.OrgID, id)
}

// Update replaces a provider's label, settings and, when secretID is set, its secret. Kind and name stay fixed.
func (s *Store) Update(ctx context.Context, actor authz.Principal, id string, p Provider, requestID string) (Provider, error) {
	if !actor.IsOrgAdmin() {
		return Provider{}, ErrForbidden
	}
	cur, err := s.Get(ctx, actor.OrgID, id)
	if err != nil {
		return Provider{}, err
	}
	p.Kind, p.Name = cur.Kind, cur.Name
	if p.DisplayName == "" {
		p.DisplayName = cur.DisplayName
	}
	if len(p.DisplayName) > 64 {
		return Provider{}, &Invalid{"the display name is at most 64 characters"}
	}
	cfg, err := p.config()
	if err != nil {
		return Provider{}, err
	}
	if p.SecretID == "" {
		p.SecretID = cur.SecretID
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if p.SecretID != cur.SecretID {
			if err := s.secrets.MarkControllerOnly(ctx, tx, actor.OrgID, p.SecretID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE identity_providers SET display_name = ?, config = ?, secret_id = ?, updated_at = ? WHERE id = ? AND org_id = ?`),
			p.DisplayName, cfg, nullable(p.SecretID), s.now().UTC(), id, actor.OrgID); err != nil {
			return err
		}
		reason := "settings"
		if p.SecretID != cur.SecretID {
			reason = "settings and secret"
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "identity-provider.update", ResourceType: "identity-provider", ResourceID: id, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		var inv *secrets.Invalid
		if errors.As(err, &inv) {
			return Provider{}, &Invalid{inv.Reason}
		}
		return Provider{}, err
	}
	return s.Get(ctx, actor.OrgID, id)
}

const providerCols = `id, org_id, kind, name, display_name, enabled, config, COALESCE(secret_id, ''), created_at, updated_at`

func scanProvider(sc interface{ Scan(...any) error }) (Provider, error) {
	var p Provider
	var cfg string
	if err := sc.Scan(&p.ID, &p.OrgID, &p.Kind, &p.Name, &p.DisplayName, &p.Enabled, &cfg, &p.SecretID, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Provider{}, err
	}
	switch p.Kind {
	case OIDC:
		p.OIDC = &OIDCConfig{}
		_ = json.Unmarshal([]byte(cfg), p.OIDC)
	case LDAP:
		p.LDAP = &LDAPConfig{}
		_ = json.Unmarshal([]byte(cfg), p.LDAP)
	}
	return p, nil
}

// Get returns one of an organization's providers.
func (s *Store) Get(ctx context.Context, orgID, id string) (Provider, error) {
	p, err := scanProvider(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+providerCols+` FROM identity_providers WHERE id = ? AND org_id = ?`), id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	return p, err
}

func (s *Store) byID(ctx context.Context, id string) (Provider, error) {
	p, err := scanProvider(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+providerCols+` FROM identity_providers WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	return p, err
}

// List returns the organization's providers for administrators.
func (s *Store) List(ctx context.Context, actor authz.Principal) ([]Provider, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	return s.list(ctx, `SELECT `+providerCols+` FROM identity_providers WHERE org_id = ? ORDER BY name`, actor.OrgID)
}

// Enabled lists every enabled provider, for the sign-in page. Only names and kinds are shown there.
func (s *Store) Enabled(ctx context.Context) ([]Provider, error) {
	return s.list(ctx, `SELECT `+providerCols+` FROM identity_providers WHERE enabled = ? ORDER BY display_name`, true)
}

func (s *Store) list(ctx context.Context, q string, args ...any) ([]Provider, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

var validRoles = []authz.Role{authz.Developer, authz.Maintainer, authz.Publisher, authz.Operator, authz.SecurityReviewer, authz.Administrator}

// AddMapping maps a provider group to a role at a scope in the provider's organization.
func (s *Store) AddMapping(ctx context.Context, actor authz.Principal, providerID string, m Mapping, requestID string) (Mapping, error) {
	if !actor.IsOrgAdmin() {
		return Mapping{}, ErrForbidden
	}
	if _, err := s.Get(ctx, actor.OrgID, providerID); err != nil {
		return Mapping{}, err
	}
	if m.Group == "" || len(m.Group) > 512 {
		return Mapping{}, &Invalid{"a group name is required"}
	}
	if !slices.Contains(validRoles, m.Binding.Role) {
		return Mapping{}, &Invalid{"unknown role"}
	}
	if err := s.scopeInOrg(ctx, actor.OrgID, m.Binding); err != nil {
		return Mapping{}, err
	}
	m.ID, m.ProviderID = ids.New(), providerID
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO identity_group_mappings (id, org_id, provider_id, group_name, role, scope_type, scope_id, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			m.ID, actor.OrgID, providerID, m.Group, string(m.Binding.Role), string(m.Binding.ScopeType), m.Binding.ScopeID, actor.ID, s.now().UTC()); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "identity-mapping.create", ResourceType: "identity-provider", ResourceID: providerID, Outcome: "succeeded",
			Reason: fmt.Sprintf("%s -> %s@%s:%s", m.Group, m.Binding.Role, m.Binding.ScopeType, m.Binding.ScopeID), RequestID: requestID})
	})
	return m, err
}

func (s *Store) scopeInOrg(ctx context.Context, orgID string, b authz.Binding) error {
	table := map[authz.ScopeType]string{authz.ScopeTeam: "teams", authz.ScopeProject: "projects"}[b.ScopeType]
	switch {
	case b.ScopeType == authz.ScopeOrganization:
		if b.ScopeID != orgID {
			return &Invalid{"an organization binding must name this organization"}
		}
		return nil
	case table == "":
		return &Invalid{"the scope must be organization, team or project"}
	}
	var n int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE id = ? AND org_id = ?`), b.ScopeID, orgID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return &Invalid{"the scope does not exist in this organization"}
	}
	return nil
}

// RemoveMapping deletes a mapping. Bindings it granted disappear at each member's next sign-in.
func (s *Store) RemoveMapping(ctx context.Context, actor authz.Principal, providerID, mappingID, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM identity_group_mappings WHERE id = ? AND provider_id = ? AND org_id = ?`), mappingID, providerID, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "identity-mapping.delete", ResourceType: "identity-provider", ResourceID: providerID, Outcome: "succeeded", Reason: mappingID, RequestID: requestID})
	})
}

// Mappings lists a provider's group mappings.
func (s *Store) Mappings(ctx context.Context, actor authz.Principal, providerID string) ([]Mapping, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	return s.mappings(ctx, actor.OrgID, providerID)
}

func (s *Store) mappings(ctx context.Context, orgID, providerID string) ([]Mapping, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, group_name, role, scope_type, scope_id FROM identity_group_mappings WHERE provider_id = ? AND org_id = ? ORDER BY group_name, role`), providerID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Mapping{}
	for rows.Next() {
		m := Mapping{ProviderID: providerID}
		var role, scope string
		if err := rows.Scan(&m.ID, &m.Group, &role, &scope, &m.Binding.ScopeID); err != nil {
			return nil, err
		}
		m.Binding.Role, m.Binding.ScopeType = authz.Role(role), authz.ScopeType(scope)
		out = append(out, m)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUnique(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}
