package recipes

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
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/recipedef"
)

const Draft = "draft"

var (
	ErrExists   = errors.New("this recipe version already exists")
	ErrNotDraft = errors.New("only a draft can be changed; create a new version instead")
)

// Invalid explains a refused definition or decision.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

// CustomVersion is one version of an administrator's recipe (ADR 0041).
type CustomVersion struct {
	Definition recipedef.Definition
	Status     string
	Reason     string
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedBy  string
	UpdatedAt  time.Time
}

func (s *Statuses) validateCustom(d recipedef.Definition) error {
	if len(s.cat.idsNamed(d.ID)) > 0 {
		return &Invalid{fmt.Sprintf("%q is a built-in recipe; choose another ID", d.ID)}
	}
	cat, err := egress.Builtin()
	if err != nil {
		return err
	}
	for _, p := range d.EgressPresets {
		if _, ok := cat.Get(p); !ok {
			return &Invalid{fmt.Sprintf("egress preset %q does not exist; use one of %s", p, strings.Join(cat.IDs(), ", "))}
		}
	}
	return nil
}

// CreateCustom saves a new draft version from its YAML or JSON definition.
func (s *Statuses) CreateCustom(ctx context.Context, actor authz.Principal, text []byte, requestID string) (CustomVersion, error) {
	if !actor.IsOrgAdmin() {
		return CustomVersion{}, ErrForbidden
	}
	d, err := recipedef.Parse(text)
	if err != nil {
		return CustomVersion{}, err
	}
	if err := s.validateCustom(d); err != nil {
		return CustomVersion{}, err
	}
	raw, _ := json.Marshal(d)
	now := time.Now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM custom_recipes WHERE org_id = ? AND recipe_id = ? AND version = ?`), actor.OrgID, d.ID, d.Version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrExists
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO custom_recipes (org_id, recipe_id, version, status, definition, created_by, created_at, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			actor.OrgID, d.ID, d.Version, Draft, string(raw), actor.ID, now, actor.ID, now); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "recipe.create", ResourceType: "recipe", ResourceID: d.ID + "@" + d.Version, Outcome: "succeeded", Reason: d.DisplayName, RequestID: requestID})
	})
	if err != nil {
		return CustomVersion{}, err
	}
	return s.custom(ctx, actor.OrgID, d.ID, d.Version)
}

// ReplaceDraft changes a draft. Its ID and version stay those of the draft.
func (s *Statuses) ReplaceDraft(ctx context.Context, actor authz.Principal, id, version string, text []byte, requestID string) (CustomVersion, error) {
	if !actor.IsOrgAdmin() {
		return CustomVersion{}, ErrForbidden
	}
	d, err := recipedef.Parse(text)
	if err != nil {
		return CustomVersion{}, err
	}
	if d.ID != id || d.Version != version {
		return CustomVersion{}, &Invalid{"the definition's id and version must stay " + id + " " + version + "; create a new version to change them"}
	}
	if err := s.validateCustom(d); err != nil {
		return CustomVersion{}, err
	}
	cur, err := s.custom(ctx, actor.OrgID, id, version)
	if err != nil {
		return CustomVersion{}, err
	}
	if cur.Status != Draft {
		return CustomVersion{}, ErrNotDraft
	}
	raw, _ := json.Marshal(d)
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE custom_recipes SET definition = ?, updated_by = ?, updated_at = ? WHERE org_id = ? AND recipe_id = ? AND version = ? AND status = ?`),
			string(raw), actor.ID, time.Now().UTC(), actor.OrgID, id, version, Draft)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotDraft
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "recipe.update", ResourceType: "recipe", ResourceID: id + "@" + version, Outcome: "succeeded", RequestID: requestID})
	})
	if err != nil {
		return CustomVersion{}, err
	}
	return s.custom(ctx, actor.OrgID, id, version)
}

// DecideCustom approves a draft, or deprecates or withdraws an approved version, with a reason.
func (s *Statuses) DecideCustom(ctx context.Context, actor authz.Principal, id, version, action, reason, requestID string) (CustomVersion, error) {
	if !actor.IsOrgAdmin() {
		return CustomVersion{}, ErrForbidden
	}
	if len(strings.TrimSpace(reason)) < 3 {
		return CustomVersion{}, &Invalid{"a reason is required"}
	}
	from, to := map[string][]string{
		"approve":   {Draft},
		"deprecate": {Approved},
		"withdraw":  {Approved, Deprecated},
	}[action], map[string]string{"approve": Approved, "deprecate": Deprecated, "withdraw": Withdrawn}[action]
	if to == "" {
		return CustomVersion{}, &Invalid{"the action must be approve, deprecate or withdraw"}
	}
	cur, err := s.custom(ctx, actor.OrgID, id, version)
	if err != nil {
		return CustomVersion{}, err
	}
	if !slices.Contains(from, cur.Status) {
		return CustomVersion{}, &Invalid{fmt.Sprintf("a %s version cannot be %s", cur.Status, map[string]string{"approve": "approved", "deprecate": "deprecated", "withdraw": "withdrawn"}[action])}
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE custom_recipes SET status = ?, reason = ?, updated_by = ?, updated_at = ? WHERE org_id = ? AND recipe_id = ? AND version = ? AND status = ?`),
			to, reason, actor.ID, time.Now().UTC(), actor.OrgID, id, version, cur.Status); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "recipe." + action, ResourceType: "recipe", ResourceID: id + "@" + version, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return CustomVersion{}, err
	}
	return s.custom(ctx, actor.OrgID, id, version)
}

// ListCustom returns every version, drafts included, for administrators.
func (s *Statuses) ListCustom(ctx context.Context, actor authz.Principal) ([]CustomVersion, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	return s.customWhere(ctx, `org_id = ?`, actor.OrgID)
}

func (s *Statuses) custom(ctx context.Context, orgID, id, version string) (CustomVersion, error) {
	list, err := s.customWhere(ctx, `org_id = ? AND recipe_id = ? AND version = ?`, orgID, id, version)
	if err != nil {
		return CustomVersion{}, err
	}
	if len(list) == 0 {
		return CustomVersion{}, ErrNotFound
	}
	return list[0], nil
}

func (s *Statuses) customWhere(ctx context.Context, where string, args ...any) ([]CustomVersion, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT definition, status, reason, created_by, created_at, updated_by, updated_at FROM custom_recipes WHERE `+where+` ORDER BY recipe_id, version`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CustomVersion{}
	for rows.Next() {
		var v CustomVersion
		var raw string
		if err := rows.Scan(&raw, &v.Status, &v.Reason, &v.CreatedBy, &v.CreatedAt, &v.UpdatedBy, &v.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &v.Definition); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// asRecipe presents a custom version the way built-in recipes are listed and admitted.
func asRecipe(v CustomVersion) Recipe {
	d := v.Definition
	list := func(l []string) []string { return append([]string{}, l...) }
	return Recipe{ID: d.ID, Version: d.Version, DisplayName: d.DisplayName, Description: d.Description, Languages: list(d.Languages),
		ApplicationTypes: list(d.ApplicationTypes), Platforms: list(d.Platforms), EgressPresets: list(d.EgressPresets), Runtimes: []Runtime{}, Custom: &d}
}
