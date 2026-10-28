package imagesources

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// InUse refuses a delete while something still depends on the source.
type InUse struct{ Uses []string }

func (e *InUse) Error() string { return "the image source is in use: " + strings.Join(e.Uses, "; ") }

// Update changes a source's name, repository patterns, teams and pull profile with the same checks as Create. The
// upstream and mirror stay fixed because they name the mirror deployment. A narrower source applies to new tokens at
// once; tokens already issued expire within TokenTTL.
func (s *Store) Update(ctx context.Context, actor authz.Principal, id string, change Source, requestID string) (Source, error) {
	if !actor.IsOrgAdmin() {
		return Source{}, ErrForbidden
	}
	old, err := s.Get(ctx, actor.OrgID, id)
	if err != nil {
		return Source{}, err
	}
	src := old
	src.Name, src.Patterns, src.TeamIDs, src.PullProfileID = change.Name, change.Patterns, change.TeamIDs, change.PullProfileID
	if err := s.validate(ctx, actor.OrgID, &src); err != nil {
		return Source{}, err
	}
	src.UpdatedAt = s.now().UTC()
	patterns, _ := json.Marshal(src.Patterns)
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE image_sources SET name = ?, repository_patterns = ?, team_ids = ?, pull_profile_id = ?, updated_at = ? WHERE id = ? AND org_id = ?`),
			src.Name, string(patterns), teamsJSON(src.TeamIDs), nullable(src.PullProfileID), src.UpdatedAt, id, actor.OrgID)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(err.Error(), "duplicate key") {
				return ErrConflict
			}
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "image-source.update", ResourceType: "image-source", ResourceID: id, Outcome: "succeeded",
			Reason: fmt.Sprintf("name %s -> %s; patterns %s -> %s; teams %s -> %s; pull profile %q -> %q", old.Name, src.Name,
				strings.Join(old.Patterns, ","), strings.Join(src.Patterns, ","), strings.Join(old.TeamIDs, ","), strings.Join(src.TeamIDs, ","),
				old.PullProfileID, src.PullProfileID), RequestID: requestID})
	})
	if err != nil {
		return Source{}, err
	}
	return s.Get(ctx, actor.OrgID, id)
}

// unfinishedStates are build states whose attempts may still ask for mirror tokens.
var unfinishedStates = []any{"queued", "assigned", "preparing", "building", "committing"}

// Delete removes a source. It is refused while an unfinished build of one of its teams has a plan naming the source's
// upstream, because that build may still pull through the mirror (docs/architecture/registry-auth.md).
func (s *Store) Delete(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	src, err := s.Get(ctx, actor.OrgID, id)
	if err != nil {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		args := append([]any{actor.OrgID, "%" + src.Upstream + "/%"}, unfinishedStates...)
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(unfinishedStates)), ", ")
		rows, err := tx.QueryContext(ctx, s.db.Rebind(`SELECT r.id, r.team_id FROM build_runs r JOIN build_plans p ON p.digest = r.plan_digest
			WHERE r.org_id = ? AND p.plan LIKE ? AND r.state IN (`+marks+`) ORDER BY r.id`), args...)
		if err != nil {
			return err
		}
		var uses []string
		for rows.Next() {
			var run, team string
			if err := rows.Scan(&run, &team); err != nil {
				rows.Close()
				return err
			}
			if slices.Contains(src.TeamIDs, team) {
				uses = append(uses, "unfinished build "+run)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(uses) > 0 {
			return &InUse{uses}
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM image_sources WHERE id = ? AND org_id = ?`), id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "image-source.delete", ResourceType: "image-source", ResourceID: id, Outcome: "succeeded",
			Reason: fmt.Sprintf("%s (%s via %s) for teams %s", src.Name, src.Upstream, src.Mirror, strings.Join(src.TeamIDs, ",")), RequestID: requestID})
	})
}
