package secrets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

const Revoked = "revoked"

// Grant makes one secret available to one project. Developers and maintainers of the project bind granted keys to
// their spec's secret IDs; they never choose a provider or reference themselves (docs/architecture/secrets.md#grants).
type Grant struct {
	ID                string
	ProjectID         string
	Name              string
	ProviderID        string
	ProviderName      string
	ProviderType      string
	ProviderReference string
	VersionPolicy     string
	Usage             string
	PoolIDs           []string
	// Fields are the key names of a key set: a whole KV v2 secret whose keys the project binds one at a time.
	// Empty for a grant of a single key.
	Fields    []string
	Status    string
	Reason    string
	CreatedBy string
	CreatedAt time.Time
	RevokedAt *time.Time
}

type GrantRequest struct {
	ProjectID         string
	Name              string
	ProviderID        string
	ProviderReference string
	VersionPolicy     string
	Usage             string
	PoolIDs           []string
	Reason            string
	// AllowedSecretID grants an entry of the allowed list; it sets the provider and reference.
	AllowedSecretID string
}

// GrantUse is an active binding of a project, as its members see it: which spec ID uses which granted key.
type GrantUse struct {
	LogicalID string
	GrantID   string
	// Field is the key of a key set the ID is bound to.
	Field     string
	BindingID string
	Version   int
	Encoding  string
}

var (
	bindGrantRoles = []authz.Role{authz.Developer, authz.Maintainer, authz.Administrator}
	viewGrantRoles = []authz.Role{authz.Developer, authz.Maintainer, authz.Administrator, authz.SecurityReviewer}
)

func (s *Service) projectResource(ctx context.Context, orgID, projectID string) (authz.Resource, error) {
	var team string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT team_id FROM projects WHERE id = ? AND org_id = ?`), projectID, orgID).Scan(&team)
	if errors.Is(err, sql.ErrNoRows) {
		return authz.Resource{}, ErrNotFound
	}
	return authz.Resource{OrgID: orgID, TeamID: team, ProjectID: projectID}, err
}

// CreateGrant is administrator-only: the grant is the decision that a project may use a secret.
func (s *Service) CreateGrant(ctx context.Context, actor authz.Principal, req GrantRequest, requestID string) (Grant, error) {
	if !actor.IsOrgAdmin() {
		return Grant{}, ErrForbidden
	}
	if req.Usage == "" {
		req.Usage = "build"
	}
	if !slices.Contains([]string{"build", "test", "both"}, req.Usage) {
		return Grant{}, &Invalid{"usage must be build, test or both"}
	}
	if len(req.Reason) < 3 {
		return Grant{}, &Invalid{"a reason is required"}
	}
	if req.AllowedSecretID != "" {
		a, err := s.allowedSecret(ctx, actor.OrgID, req.AllowedSecretID)
		if errors.Is(err, ErrNotFound) {
			return Grant{}, &Invalid{"that secret is not on the allowed list"}
		}
		if err != nil {
			return Grant{}, err
		}
		req.ProviderID, req.ProviderReference = a.ProviderID, a.Reference
		if req.Name == "" {
			req.Name = a.Label
		}
	}
	t, err := s.resolve(ctx, actor.OrgID, req.ProjectID, req.ProviderID, req.ProviderReference, req.VersionPolicy, req.PoolIDs)
	if err != nil {
		return Grant{}, err
	}
	var allowedID any
	if t.prov.Type != ProviderLocal {
		a, ok, err := s.allowing(ctx, actor.OrgID, t.prov.ID, t.ref)
		if err != nil {
			return Grant{}, err
		}
		if !ok {
			return Grant{}, &Invalid{"add this secret under Allowed secrets before granting it"}
		}
		allowedID = a.ID
	}
	fields := []string{}
	if isKeySet(t.prov.Type, t.ref) {
		if fields, err = s.keyNames(ctx, actor.OrgID, t.prov.ID, t.ref, t.versionPolicy); err != nil {
			return Grant{}, &Invalid{"the provider could not list that secret's keys: " + clipText(err.Error(), 300)}
		}
	}
	if req.Name == "" {
		req.Name = t.label
	}
	if len(req.Name) > 128 {
		return Grant{}, &Invalid{"the name must be at most 128 characters"}
	}
	pools, _ := json.Marshal(t.poolIDs)
	keys, _ := json.Marshal(fields)
	id, now := ids.New(), s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO secret_grants (id, org_id, project_id, name, provider_id, provider_ref, version_policy, usage, pool_ids, status, reason, created_by, created_at, fields, allowed_secret_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			id, actor.OrgID, req.ProjectID, req.Name, t.prov.ID, t.ref, t.versionPolicy, req.Usage, string(pools), Active, req.Reason, actor.ID, now, string(keys), allowedID); err != nil {
			if isUnique(err) {
				return &Invalid{"this project already has a granted key with that name"}
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.grant", ResourceType: "secret-grant", ResourceID: id, Outcome: "succeeded",
			Reason: fmt.Sprintf("project %s may use %s %s as %q (%s, %d keys): %s", req.ProjectID, t.prov.Name, t.label, req.Name, req.Usage, len(fields), req.Reason), RequestID: requestID})
	})
	if err != nil {
		return Grant{}, err
	}
	return s.grant(ctx, actor.OrgID, req.ProjectID, id)
}

// RevokeGrant withdraws a key and disables every binding made from it, so running builds can no longer receive it.
func (s *Service) RevokeGrant(ctx context.Context, actor authz.Principal, projectID, grantID, reason, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	if len(reason) < 3 {
		return &Invalid{"a reason is required"}
	}
	now := s.now().UTC()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_grants SET status = ?, revoked_by = ?, revoked_at = ? WHERE id = ? AND project_id = ? AND org_id = ? AND status = ?`),
			Revoked, actor.ID, now, grantID, projectID, actor.OrgID, Active)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_bindings SET status = ?, version = version + 1, reason = ?, updated_by = ?, updated_at = ? WHERE grant_id = ? AND status = ?`),
			Disabled, "grant revoked: "+reason, actor.ID, now, grantID, Active); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.grant.revoke", ResourceType: "secret-grant", ResourceID: grantID, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
}

// BindGrant binds one of the project's granted keys to a logical secret ID. The caller must be a developer or
// maintainer of the project (or an administrator), and the grant must be active and belong to this project.
func (s *Service) BindGrant(ctx context.Context, actor authz.Principal, projectID, logical, grantID, field, encoding, requestID string) (Binding, error) {
	res, err := s.projectResource(ctx, actor.OrgID, projectID)
	if err != nil {
		return Binding{}, err
	}
	if !authz.Can(actor, bindGrantRoles, res) {
		return Binding{}, ErrForbidden
	}
	if !logicalID.MatchString(logical) {
		return Binding{}, &Invalid{"the logical ID must match the spec's secret ID pattern"}
	}
	g, err := s.grant(ctx, actor.OrgID, projectID, grantID)
	if errors.Is(err, ErrNotFound) || (err == nil && g.Status != Active) {
		return Binding{}, &Invalid{"that key is not granted to this project"}
	}
	if err != nil {
		return Binding{}, err
	}
	prov, err := s.provider(ctx, actor.OrgID, g.ProviderID)
	if err != nil {
		return Binding{}, err
	}
	t := target{prov: prov, ref: g.ProviderReference, label: g.Name, versionPolicy: g.VersionPolicy, poolIDs: g.PoolIDs}
	switch {
	case len(g.Fields) > 0 && !slices.Contains(g.Fields, field):
		return Binding{}, &Invalid{"choose one of the keys in " + g.Name}
	case len(g.Fields) > 0:
		t.ref, t.label = g.ProviderReference+"#"+field, g.Name+" › "+field
	case field != "":
		return Binding{}, &Invalid{g.Name + " is a single key; it has no keys to choose from"}
	}
	if t.encoding, err = s.checkEncoding(ctx, actor.OrgID, t, encoding); err != nil {
		return Binding{}, err
	}
	return s.insertBinding(ctx, actor, projectID, logical, t, g.Usage, nil, "granted key "+t.label, g.ID, requestID)
}

// ListGrants returns the project's grants and its active bindings. Members of the project may read them; provider
// references are for administrators, and the API layer omits them for everyone else.
func (s *Service) ListGrants(ctx context.Context, actor authz.Principal, projectID string) ([]Grant, []GrantUse, error) {
	res, err := s.projectResource(ctx, actor.OrgID, projectID)
	if err != nil {
		return nil, nil, err
	}
	if !authz.Can(actor, viewGrantRoles, res) {
		return nil, nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+grantCols+grantFrom+` WHERE g.org_id = ? AND g.project_id = ? AND g.status = ? ORDER BY g.name`), actor.OrgID, projectID, Active)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	grants := []Grant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, nil, err
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	active, err := s.ActiveFor(ctx, actor.OrgID, projectID)
	if err != nil {
		return nil, nil, err
	}
	sets := map[string]bool{}
	for _, g := range grants {
		sets[g.ID] = len(g.Fields) > 0
	}
	uses := []GrantUse{}
	for _, b := range active {
		u := GrantUse{LogicalID: b.LogicalID, GrantID: b.GrantID, BindingID: b.ID, Version: b.Version, Encoding: b.Encoding}
		if sets[b.GrantID] {
			_, u.Field, _ = strings.Cut(b.ProviderReference, "#")
		}
		uses = append(uses, u)
	}
	slices.SortFunc(uses, func(a, b GrantUse) int {
		if a.LogicalID < b.LogicalID {
			return -1
		}
		if a.LogicalID > b.LogicalID {
			return 1
		}
		return 0
	})
	return grants, uses, nil
}

const grantCols = `g.id, g.project_id, g.name, g.provider_id, p.name, p.type, g.provider_ref, g.version_policy, g.usage, g.pool_ids, g.status, g.reason, g.created_by, g.created_at, g.revoked_at, g.fields`

const grantFrom = ` FROM secret_grants g JOIN secret_providers p ON p.id = g.provider_id`

func scanGrant(sc interface{ Scan(...any) error }) (Grant, error) {
	var g Grant
	var pools, fields string
	var revoked sql.NullTime
	if err := sc.Scan(&g.ID, &g.ProjectID, &g.Name, &g.ProviderID, &g.ProviderName, &g.ProviderType, &g.ProviderReference, &g.VersionPolicy, &g.Usage, &pools, &g.Status, &g.Reason, &g.CreatedBy, &g.CreatedAt, &revoked, &fields); err != nil {
		return Grant{}, err
	}
	g.Fields = []string{}
	_ = json.Unmarshal([]byte(fields), &g.Fields)
	if revoked.Valid {
		g.RevokedAt = &revoked.Time
	}
	g.PoolIDs = []string{}
	_ = json.Unmarshal([]byte(pools), &g.PoolIDs)
	return g, nil
}

func (s *Service) grant(ctx context.Context, orgID, projectID, id string) (Grant, error) {
	g, err := scanGrant(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+grantCols+grantFrom+` WHERE g.id = ? AND g.project_id = ? AND g.org_id = ?`), id, projectID, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Grant{}, ErrNotFound
	}
	return g, err
}

// RefreshGrantKeys rereads a key set's key names, for keys added or removed in the store since the grant. Bindings
// to keys that are gone are disabled so builds fail clearly instead of receiving nothing.
func (s *Service) RefreshGrantKeys(ctx context.Context, actor authz.Principal, projectID, grantID, requestID string) (Grant, error) {
	if !actor.IsOrgAdmin() {
		return Grant{}, ErrForbidden
	}
	g, err := s.grant(ctx, actor.OrgID, projectID, grantID)
	if err != nil {
		return Grant{}, err
	}
	if g.Status != Active || !isKeySet(g.ProviderType, g.ProviderReference) {
		return Grant{}, &Invalid{"only an active key set can be refreshed"}
	}
	fields, err := s.keyNames(ctx, actor.OrgID, g.ProviderID, g.ProviderReference, g.VersionPolicy)
	if err != nil {
		return Grant{}, &Invalid{"the provider could not list that secret's keys: " + clipText(err.Error(), 300)}
	}
	keys, _ := json.Marshal(fields)
	now := s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_grants SET fields = ? WHERE id = ?`), string(keys), grantID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, s.db.Rebind(`SELECT id, provider_ref FROM secret_bindings WHERE grant_id = ? AND status = ?`), grantID, Active)
		if err != nil {
			return err
		}
		var gone []string
		for rows.Next() {
			var id, ref string
			if err := rows.Scan(&id, &ref); err != nil {
				rows.Close()
				return err
			}
			if _, key, _ := strings.Cut(ref, "#"); !slices.Contains(fields, key) {
				gone = append(gone, id)
			}
		}
		rows.Close()
		for _, id := range gone {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_bindings SET status = ?, version = version + 1, reason = ?, updated_by = ?, updated_at = ? WHERE id = ?`),
				Disabled, "the key no longer exists in "+g.Name, actor.ID, now, id); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.grant.refresh", ResourceType: "secret-grant", ResourceID: grantID, Outcome: "succeeded",
			Reason: fmt.Sprintf("%d keys; %d bindings to removed keys disabled", len(fields), len(gone)), RequestID: requestID})
	})
	if err != nil {
		return Grant{}, err
	}
	return s.grant(ctx, actor.OrgID, projectID, grantID)
}
