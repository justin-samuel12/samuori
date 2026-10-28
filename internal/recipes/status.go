package recipes

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
)

var (
	ErrNotFound  = errors.New("recipe not found")
	ErrForbidden = errors.New("forbidden")
)

// Status is an organization's decision about one recipe version. Built-in versions are approved until an
// administrator deprecates or withdraws them.
type Status struct {
	Status, Reason string
}

type Statuses struct {
	db          *db.DB
	cat         *Catalog
	imagePolicy ImagePolicy
}

func NewStatuses(d *db.DB, cat *Catalog) *Statuses { return &Statuses{db: d, cat: cat} }

func (s *Statuses) Catalog() *Catalog { return s.cat }

func (s *Statuses) Get(ctx context.Context, orgID, id, version string) (Recipe, Status, error) {
	r, ok := s.cat.Get(id, version)
	if !ok {
		v, err := s.custom(ctx, orgID, id, version)
		if err != nil || v.Status == Draft {
			return Recipe{}, Status{}, ErrNotFound
		}
		return asRecipe(v), Status{Status: v.Status, Reason: v.Reason}, nil
	}
	st := Status{Status: Approved}
	var reason sql.NullString
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT status, reason FROM recipe_status WHERE org_id = ? AND recipe_id = ? AND version = ?`), orgID, id, version).Scan(&st.Status, &reason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Recipe{}, Status{}, err
	}
	st.Reason = reason.String
	withRuntimes := []Recipe{r}
	if err := s.withOrgRuntimes(ctx, orgID, withRuntimes); err != nil {
		return Recipe{}, Status{}, err
	}
	return withRuntimes[0], st, nil
}

func (s *Statuses) All(ctx context.Context, orgID string) ([]Recipe, map[string]Status, error) {
	all := s.cat.All()
	statuses := map[string]Status{}
	for _, r := range all {
		statuses[r.ID+"@"+r.Version] = Status{Status: Approved}
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT recipe_id, version, status, reason FROM recipe_status WHERE org_id = ?`), orgID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, v, st string
		var reason sql.NullString
		if err := rows.Scan(&id, &v, &st, &reason); err != nil {
			return nil, nil, err
		}
		if _, ok := statuses[id+"@"+v]; ok {
			statuses[id+"@"+v] = Status{Status: st, Reason: reason.String}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := s.withOrgRuntimes(ctx, orgID, all); err != nil {
		return nil, nil, err
	}
	custom, err := s.customWhere(ctx, `org_id = ? AND status <> ?`, orgID, Draft)
	if err != nil {
		return nil, nil, err
	}
	for _, v := range custom {
		all = append(all, asRecipe(v))
		statuses[v.Definition.ID+"@"+v.Definition.Version] = Status{Status: v.Status, Reason: v.Reason}
	}
	return all, statuses, nil
}

// Set records an administrator's status decision with a reason.
func (s *Statuses) Set(ctx context.Context, actor authz.Principal, id, version, status, reason, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	if _, ok := s.cat.Get(id, version); !ok {
		return ErrNotFound
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM recipe_status WHERE org_id = ? AND recipe_id = ? AND version = ?`), actor.OrgID, id, version); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO recipe_status (org_id, recipe_id, version, status, reason, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			actor.OrgID, id, version, status, reason, actor.ID, time.Now().UTC()); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "recipe.status", ResourceType: "recipe", ResourceID: id + "@" + version, Outcome: "succeeded", Reason: status + ": " + reason, RequestID: requestID})
	})
}
