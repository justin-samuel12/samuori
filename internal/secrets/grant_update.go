package secrets

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/tenancy"
)

// GrantChange is an administrator's edit of a grant's name, usage, version policy and eligible pools.
type GrantChange struct {
	Name          string
	Usage         string
	VersionPolicy string
	PoolIDs       []string
	Reason        string
}

// GrantUpdate is the edited grant and the bindings the edit disabled because they no longer fit.
type GrantUpdate struct {
	Grant    Grant
	Disabled []GrantUse
}

// Covers reports whether a grant scope (usage, pools) includes another. An empty pool list allows every pool.
func Covers(usage string, pools []string, innerUsage string, innerPools []string) bool {
	if usage != "both" && usage != innerUsage {
		return false
	}
	if len(pools) == 0 {
		return true
	}
	return len(innerPools) > 0 && !slices.ContainsFunc(innerPools, func(p string) bool { return !slices.Contains(pools, p) })
}

// UpdateGrant changes a grant in place (ADR 0064). Bindings made from it follow the new scope. A binding whose scope
// the new grant no longer covers is disabled with an audited reason; one that still fits stays active, and keeps its
// version unless the version policy changed, since then it would deliver a different value.
func (s *Service) UpdateGrant(ctx context.Context, actor authz.Principal, projectID, grantID string, c GrantChange, requestID string) (GrantUpdate, error) {
	if !actor.IsOrgAdmin() {
		return GrantUpdate{}, ErrForbidden
	}
	if !slices.Contains([]string{"build", "test", "both"}, c.Usage) {
		return GrantUpdate{}, &Invalid{"usage must be build, test or both"}
	}
	if len(c.Reason) < 3 {
		return GrantUpdate{}, &Invalid{"a reason is required"}
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > 128 {
		return GrantUpdate{}, &Invalid{"the name must be 1 to 128 characters"}
	}
	if c.VersionPolicy == "" {
		c.VersionPolicy = "latest"
	}
	old, err := s.grant(ctx, actor.OrgID, projectID, grantID)
	if err != nil {
		return GrantUpdate{}, err
	}
	if old.Status != Active {
		return GrantUpdate{}, ErrNotFound
	}
	t, err := s.resolve(ctx, actor.OrgID, projectID, old.ProviderID, old.ProviderReference, c.VersionPolicy, c.PoolIDs)
	if err != nil {
		return GrantUpdate{}, err
	}
	c.VersionPolicy, c.PoolIDs = t.versionPolicy, t.poolIDs
	slices.Sort(c.PoolIDs)
	c.PoolIDs = slices.Compact(c.PoolIDs)
	fields := old.Fields
	newVersion := c.VersionPolicy != old.VersionPolicy
	if newVersion && len(old.Fields) > 0 {
		if fields, err = s.keyNames(ctx, actor.OrgID, old.ProviderID, old.ProviderReference, c.VersionPolicy); err != nil {
			return GrantUpdate{}, &Invalid{"the provider could not list that secret's keys: " + clipText(err.Error(), 300)}
		}
	}
	pools, _ := json.Marshal(c.PoolIDs)
	keys, _ := json.Marshal(fields)
	now := s.now().UTC()
	out := GrantUpdate{Disabled: []GrantUse{}}
	var kept int
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tenancy.RequireActive(ctx, s.db, tx, projectID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_grants SET name = ?, usage = ?, version_policy = ?, pool_ids = ?, fields = ? WHERE id = ? AND project_id = ? AND org_id = ? AND status = ?`),
			c.Name, c.Usage, c.VersionPolicy, string(pools), string(keys), grantID, projectID, actor.OrgID, Active)
		if err != nil {
			if isUnique(err) {
				return &Invalid{"this project already has a granted key with that name"}
			}
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		rows, err := tx.QueryContext(ctx, s.db.Rebind(`SELECT id, logical_id, provider_ref, usage, pool_ids, version, value_encoding FROM secret_bindings WHERE grant_id = ? AND status = ?`), grantID, Active)
		if err != nil {
			return err
		}
		type bound struct {
			use   GrantUse
			fits  bool
			field string
		}
		var list []bound
		for rows.Next() {
			var b bound
			var ref, usage, bp string
			if err := rows.Scan(&b.use.BindingID, &b.use.LogicalID, &ref, &usage, &bp, &b.use.Version, &b.use.Encoding); err != nil {
				rows.Close()
				return err
			}
			var bPools []string
			_ = json.Unmarshal([]byte(bp), &bPools)
			b.use.GrantID = grantID
			b.fits = Covers(c.Usage, c.PoolIDs, usage, bPools)
			if len(old.Fields) > 0 {
				_, b.use.Field, _ = strings.Cut(ref, "#")
				b.fits = b.fits && slices.Contains(fields, b.use.Field)
			}
			list = append(list, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		bump := 0
		if newVersion {
			bump = 1
		}
		for _, b := range list {
			if !b.fits {
				why := "grant narrowed: " + c.Reason
				if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_bindings SET status = ?, version = version + 1, reason = ?, updated_by = ?, updated_at = ? WHERE id = ? AND status = ?`),
					Disabled, why, actor.ID, now, b.use.BindingID, Active); err != nil {
					return err
				}
				if err := audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.unbind", ResourceType: "secret-binding", ResourceID: b.use.BindingID, Outcome: "succeeded",
					Reason: fmt.Sprintf("%s no longer fits %s: %s", b.use.LogicalID, c.Name, why), RequestID: requestID}); err != nil {
					return err
				}
				out.Disabled = append(out.Disabled, b.use)
				continue
			}
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_bindings SET usage = ?, pool_ids = ?, version_policy = ?, version = version + ?, updated_by = ?, updated_at = ? WHERE id = ? AND status = ?`),
				c.Usage, string(pools), c.VersionPolicy, bump, actor.ID, now, b.use.BindingID, Active); err != nil {
				return err
			}
			kept++
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.grant.update", ResourceType: "secret-grant", ResourceID: grantID, Outcome: "succeeded",
			Reason: fmt.Sprintf("%s; %d bindings kept, %d disabled: %s", grantDiff(old, c), kept, len(out.Disabled), c.Reason), RequestID: requestID})
	})
	if err != nil {
		return GrantUpdate{}, err
	}
	out.Grant, err = s.grant(ctx, actor.OrgID, projectID, grantID)
	return out, err
}

func grantDiff(old Grant, c GrantChange) string {
	pools := func(p []string) string {
		if len(p) == 0 {
			return "all pools"
		}
		return strings.Join(p, ",")
	}
	return fmt.Sprintf("name %q -> %q, usage %s -> %s, version %s -> %s, pools %s -> %s",
		old.Name, c.Name, old.Usage, c.Usage, old.VersionPolicy, c.VersionPolicy, pools(old.PoolIDs), pools(c.PoolIDs))
}

// UpdateAllowedSecret changes an allowed secret's label and note. Grants already made keep their own names.
func (s *Service) UpdateAllowedSecret(ctx context.Context, actor authz.Principal, id, label, note, requestID string) (AllowedSecret, error) {
	if !actor.IsOrgAdmin() {
		return AllowedSecret{}, ErrForbidden
	}
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 128 || len(note) > 1024 {
		return AllowedSecret{}, &Invalid{"the label must be 1 to 128 characters and the note at most 1024"}
	}
	old, err := s.allowedSecret(ctx, actor.OrgID, id)
	if err != nil {
		return AllowedSecret{}, err
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE allowed_secrets SET label = ?, note = ? WHERE id = ? AND org_id = ? AND status = ?`), label, note, id, actor.OrgID, Active)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.allow.update", ResourceType: "allowed-secret", ResourceID: id, Outcome: "succeeded",
			Reason: fmt.Sprintf("label %q -> %q, note %q -> %q", old.Label, label, clipText(old.Note, 200), clipText(note, 200)), RequestID: requestID})
	})
	if err != nil {
		return AllowedSecret{}, err
	}
	return s.allowedSecret(ctx, actor.OrgID, id)
}
