package recipes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/recipes/catalog"
)

var (
	ErrRuntimeExists = errors.New("this recipe version already has a runtime with that label")
	runtimeLabel     = regexp.MustCompile(`^[0-9]{1,4}(\.[0-9]{1,4}){0,2}$`)
)

// ImagePolicy returns why the organization's base-image policy refuses ref, or "" when it allows it.
type ImagePolicy func(ctx context.Context, orgID, ref string) (string, error)

// UseImagePolicy sets the base-image check organization runtimes must pass. Without one, none can be added.
func (s *Statuses) UseImagePolicy(p ImagePolicy) { s.imagePolicy = p }

// OrgRuntime is a runtime version an administrator added to a built-in recipe version (ADR 0051).
type OrgRuntime struct {
	ID, RecipeID, RecipeVersion, Label string
	BuildImage, RunImage, Note         string
	Status, Reason                     string
	CreatedBy                          string
	CreatedAt                          time.Time
	ApprovedBy, WithdrawnBy            string
	ApprovedAt, WithdrawnAt            *time.Time
	// Superseded means a later Samuori release ships the same label, which takes precedence.
	Superseded bool
}

type RuntimeInput struct {
	Label, BuildImage, RunImage, Note string
}

func (o OrgRuntime) runtime() Runtime {
	return Runtime{Runtime: catalog.Runtime{Version: o.Label, BuildImage: o.BuildImage, RunImage: o.RunImage},
		ID: o.ID, Source: SourceOrganization, Status: o.Status, Reason: o.Reason}
}

// CreateRuntime saves a draft runtime for a built-in recipe version after checking its label and images.
func (s *Statuses) CreateRuntime(ctx context.Context, actor authz.Principal, recipeID, recipeVersion string, in RuntimeInput, requestID string) (OrgRuntime, error) {
	if !actor.IsOrgAdmin() {
		return OrgRuntime{}, ErrForbidden
	}
	r, ok := s.cat.Get(recipeID, recipeVersion)
	if !ok {
		return OrgRuntime{}, ErrNotFound
	}
	in.Label, in.BuildImage, in.RunImage, in.Note = strings.TrimSpace(in.Label), strings.TrimSpace(in.BuildImage), strings.TrimSpace(in.RunImage), strings.TrimSpace(in.Note)
	if !runtimeLabel.MatchString(in.Label) {
		return OrgRuntime{}, &Invalid{"the runtime label must be a version such as 24, 3.14 or 10.0"}
	}
	if len(in.Note) > 512 {
		return OrgRuntime{}, &Invalid{"the note must be at most 512 characters"}
	}
	for _, rt := range r.Runtimes {
		if rt.Version == in.Label {
			return OrgRuntime{}, fmt.Errorf("%w: %s %s ships runtime %s", ErrRuntimeExists, r.DisplayName, r.Version, in.Label)
		}
	}
	if s.imagePolicy == nil {
		return OrgRuntime{}, errors.New("no base-image policy check is configured")
	}
	for _, img := range []struct{ name, ref string }{{"build image", in.BuildImage}, {"run image", in.RunImage}} {
		if !pinned.MatchString(img.ref) {
			return OrgRuntime{}, &Invalid{fmt.Sprintf("the %s must be a full reference pinned by digest, such as docker.io/library/node:24-bookworm-slim@sha256:<64 hex digits>", img.name)}
		}
		why, err := s.imagePolicy(ctx, actor.OrgID, img.ref)
		if err != nil {
			return OrgRuntime{}, err
		}
		if why != "" {
			return OrgRuntime{}, &Invalid{fmt.Sprintf("the base-image policy refuses the %s: %s", img.name, why)}
		}
	}
	o := OrgRuntime{ID: ids.New(), RecipeID: recipeID, RecipeVersion: recipeVersion, Label: in.Label, BuildImage: in.BuildImage, RunImage: in.RunImage,
		Note: in.Note, Status: Draft, CreatedBy: actor.ID, CreatedAt: time.Now().UTC()}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM organization_runtimes WHERE org_id = ? AND recipe_id = ? AND recipe_version = ? AND label = ? AND status <> ?`),
			actor.OrgID, recipeID, recipeVersion, in.Label, Withdrawn).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrRuntimeExists
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO organization_runtimes (id, org_id, recipe_id, recipe_version, label, build_image, run_image, note, status, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			o.ID, actor.OrgID, recipeID, recipeVersion, o.Label, o.BuildImage, o.RunImage, o.Note, Draft, actor.ID, o.CreatedAt); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "recipe.runtime.create", ResourceType: "recipe-runtime", ResourceID: o.ID,
			Outcome: "succeeded", Reason: recipeID + "@" + recipeVersion + " runtime " + o.Label, RequestID: requestID})
	})
	if err != nil {
		return OrgRuntime{}, err
	}
	return o, nil
}

// DecideRuntime approves a draft, or withdraws a draft or approved runtime, with a reason. Any organization
// administrator may decide, as for custom recipes (ADR 0041).
func (s *Statuses) DecideRuntime(ctx context.Context, actor authz.Principal, recipeID, recipeVersion, id, action, reason, requestID string) (OrgRuntime, error) {
	if !actor.IsOrgAdmin() {
		return OrgRuntime{}, ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len(reason) < 3 {
		return OrgRuntime{}, &Invalid{"a reason is required"}
	}
	from, to := map[string][]string{"approve": {Draft}, "withdraw": {Draft, Approved}}[action], map[string]string{"approve": Approved, "withdraw": Withdrawn}[action]
	if to == "" {
		return OrgRuntime{}, &Invalid{"the action must be approve or withdraw"}
	}
	cur, err := s.orgRuntime(ctx, actor.OrgID, recipeID, recipeVersion, id)
	if err != nil {
		return OrgRuntime{}, err
	}
	if !slices.Contains(from, cur.Status) {
		return OrgRuntime{}, &Invalid{fmt.Sprintf("a %s runtime cannot be %s", cur.Status, map[string]string{"approve": "approved", "withdraw": "withdrawn"}[action])}
	}
	if action == "approve" {
		if s.imagePolicy == nil {
			return OrgRuntime{}, errors.New("no base-image policy check is configured")
		}
		if why, err := s.imagePolicy(ctx, actor.OrgID, cur.BuildImage); err != nil || why != "" {
			return OrgRuntime{}, refusal("build image", why, err)
		}
		if why, err := s.imagePolicy(ctx, actor.OrgID, cur.RunImage); err != nil || why != "" {
			return OrgRuntime{}, refusal("run image", why, err)
		}
	}
	now := time.Now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		set := `approved_by = ?, approved_at = ?`
		if to == Withdrawn {
			set = `withdrawn_by = ?, withdrawn_at = ?`
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE organization_runtimes SET status = ?, reason = ?, `+set+` WHERE id = ? AND org_id = ? AND status = ?`),
			to, reason, actor.ID, now, id, actor.OrgID, cur.Status)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return &Invalid{"the runtime changed while you were deciding; reload and try again"}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "recipe.runtime." + action, ResourceType: "recipe-runtime", ResourceID: id,
			Outcome: "succeeded", Reason: recipeID + "@" + recipeVersion + " runtime " + cur.Label + ": " + reason, RequestID: requestID})
	})
	if err != nil {
		return OrgRuntime{}, err
	}
	return s.orgRuntime(ctx, actor.OrgID, recipeID, recipeVersion, id)
}

func refusal(name, why string, err error) error {
	if err != nil {
		return err
	}
	return &Invalid{fmt.Sprintf("the base-image policy now refuses the %s: %s", name, why)}
}

// ListRuntimes returns a built-in recipe version's shipped runtimes and its organization runtimes. Administrators
// see drafts and withdrawn runtimes too; everyone else sees only approved ones.
func (s *Statuses) ListRuntimes(ctx context.Context, actor authz.Principal, recipeID, recipeVersion string) ([]Runtime, []OrgRuntime, error) {
	r, ok := s.cat.Get(recipeID, recipeVersion)
	if !ok {
		return nil, nil, ErrNotFound
	}
	list, err := s.orgRuntimes(ctx, `org_id = ? AND recipe_id = ? AND recipe_version = ?`, actor.OrgID, recipeID, recipeVersion)
	if err != nil {
		return nil, nil, err
	}
	out := []OrgRuntime{}
	for _, o := range list {
		o.Superseded = slices.ContainsFunc(r.Runtimes, func(rt Runtime) bool { return rt.Version == o.Label })
		if o.Status == Approved || actor.IsOrgAdmin() {
			out = append(out, o)
		}
	}
	return slices.Clone(r.Runtimes), out, nil
}

// withOrgRuntimes adds a built-in recipe version's organization runtimes: approved ones are offered, unless a
// shipped runtime has the same label; drafts and withdrawn ones only explain a refusal.
func (s *Statuses) withOrgRuntimes(ctx context.Context, orgID string, recipes []Recipe) error {
	list, err := s.orgRuntimes(ctx, `org_id = ?`, orgID)
	if err != nil || len(list) == 0 {
		return err
	}
	for i := range recipes {
		r := &recipes[i]
		if r.Custom != nil {
			continue
		}
		r.Runtimes = slices.Clone(r.Runtimes)
		shipped := len(r.Runtimes)
		for _, o := range list {
			if o.RecipeID != r.ID || o.RecipeVersion != r.Version {
				continue
			}
			switch {
			case slices.ContainsFunc(r.Runtimes[:shipped], func(rt Runtime) bool { return rt.Version == o.Label }):
			case o.Status == Approved:
				r.Runtimes = append(r.Runtimes, o.runtime())
			default:
				r.Unavailable = append(r.Unavailable, o.runtime())
			}
		}
	}
	return nil
}

func (s *Statuses) orgRuntime(ctx context.Context, orgID, recipeID, recipeVersion, id string) (OrgRuntime, error) {
	list, err := s.orgRuntimes(ctx, `org_id = ? AND recipe_id = ? AND recipe_version = ? AND id = ?`, orgID, recipeID, recipeVersion, id)
	if err != nil {
		return OrgRuntime{}, err
	}
	if len(list) == 0 {
		return OrgRuntime{}, ErrNotFound
	}
	return list[0], nil
}

func (s *Statuses) orgRuntimes(ctx context.Context, where string, args ...any) ([]OrgRuntime, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, recipe_id, recipe_version, label, build_image, run_image, note, status, reason, created_by, created_at, approved_by, approved_at, withdrawn_by, withdrawn_at
		FROM organization_runtimes WHERE `+where+` ORDER BY recipe_id, recipe_version, created_at, id`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrgRuntime{}
	for rows.Next() {
		var o OrgRuntime
		var approvedBy, withdrawnBy sql.NullString
		var approvedAt, withdrawnAt sql.NullTime
		if err := rows.Scan(&o.ID, &o.RecipeID, &o.RecipeVersion, &o.Label, &o.BuildImage, &o.RunImage, &o.Note, &o.Status, &o.Reason, &o.CreatedBy, &o.CreatedAt,
			&approvedBy, &approvedAt, &withdrawnBy, &withdrawnAt); err != nil {
			return nil, err
		}
		o.ApprovedBy, o.WithdrawnBy = approvedBy.String, withdrawnBy.String
		if approvedAt.Valid {
			o.ApprovedAt = &approvedAt.Time
		}
		if withdrawnAt.Valid {
			o.WithdrawnAt = &withdrawnAt.Time
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
