package publication

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

// Retired is the terminal state of a publication whose reference should no longer be used. Samuori keeps the record and
// digest history and never deletes tags at the destination.
const Retired = "retired"

// Rollback points some of a published publication's tags at its destination back to its image, for example after
// a newer image under the same tag misbehaves. The image is revalidated first; protected tags may move back, but
// immutable tags never move (docs/architecture/publication.md#rollback).
func (s *Service) Rollback(ctx context.Context, p authz.Principal, roles []authz.Role, sourceID, destinationID string, tags []string, key, requestID string) (Publication, error) {
	src, err := s.Get(ctx, p, roles, sourceID)
	if err != nil {
		return Publication{}, err
	}
	if src.State != Published {
		return Publication{}, &Invalid{"only a published image can be rolled back to"}
	}
	if destinationID != src.DestinationID {
		return Publication{}, &Invalid{"a rollback stays at the destination of the publication it rolls back to"}
	}
	team, err := s.team(ctx, src.ProjectID)
	if err != nil {
		return Publication{}, err
	}
	if !authz.Can(p, publishRoles, authz.Resource{OrgID: src.OrgID, TeamID: team, ProjectID: src.ProjectID}) {
		return Publication{}, ErrForbidden
	}
	var existing string
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM publications WHERE requested_by = ? AND idempotency_key = ?`), p.ID, key).Scan(&existing); err == nil {
		return s.load(ctx, existing)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Publication{}, err
	}
	clean, err := cleanTags(tags)
	if err != nil {
		return Publication{}, err
	}
	if len(clean) == 0 {
		return Publication{}, &Invalid{"name the tags to roll back"}
	}
	for _, t := range clean {
		if !slices.Contains(src.Tags, t) {
			return Publication{}, &Invalid{"tag " + t + " was not published with this image, so it cannot be rolled back to it"}
		}
	}
	d, err := s.destination(ctx, p.OrgID, src.DestinationID)
	if err != nil || !slices.Contains(d.AllowedProjects, src.ProjectID) {
		return Publication{}, ErrForbidden
	}
	if d.ImmutableTags {
		return Publication{}, &Invalid{"this destination's tags are immutable, so they cannot be rolled back"}
	}
	v, err := s.validations.Revalidate(ctx, p, roles, src.ArtifactID, "rollback-"+key)
	if err != nil {
		return Publication{}, err
	}
	pub := Publication{ID: ids.New(), OrgID: p.OrgID, ProjectID: src.ProjectID, Kind: "rollback", ArtifactID: src.ArtifactID, ArtifactDigest: src.ArtifactDigest, DestinationID: d.ID, Tags: clean,
		State: Requested, Gates: []Gate{}, RequestedBy: p.ID, CreatedAt: s.now().UTC(), SourcePublicationID: src.ID, ValidationRunID: v.ID}
	return s.record(ctx, pub, d, p, existing, key, requestID)
}

// Deprecate marks a published publication as discouraged, with a reason, or clears the mark. It stays usable.
func (s *Service) Deprecate(ctx context.Context, p authz.Principal, roles []authz.Role, id string, deprecated bool, reason, requestID string) (Publication, error) {
	if deprecated && len(strings.TrimSpace(reason)) < 10 {
		return Publication{}, &Invalid{"give a reason of at least 10 characters"}
	}
	return s.lifecycle(ctx, p, roles, id, func(tx *sql.Tx, now time.Time) (string, error) {
		var at, why any
		action := "publication.undeprecate"
		if deprecated {
			at, why, action = now, reason, "publication.deprecate"
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE publications SET deprecated_at = ?, deprecation_reason = ?, updated_at = ? WHERE id = ? AND state = 'published'`), at, why, now, id)
		return action, err
	}, reason, requestID)
}

// Retire ends a publication: it can no longer be promoted or rolled back to. The destination is not changed.
func (s *Service) Retire(ctx context.Context, p authz.Principal, roles []authz.Role, id, reason, requestID string) (Publication, error) {
	if len(strings.TrimSpace(reason)) < 10 {
		return Publication{}, &Invalid{"give a reason of at least 10 characters"}
	}
	return s.lifecycle(ctx, p, roles, id, func(tx *sql.Tx, now time.Time) (string, error) {
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE publications SET state = 'retired', retired_at = ?, retired_by = ?, retirement_reason = ?, updated_at = ? WHERE id = ? AND state = 'published'`), now, p.ID, reason, now, id)
		return "publication.retire", err
	}, reason, requestID)
}

func (s *Service) lifecycle(ctx context.Context, p authz.Principal, roles []authz.Role, id string, change func(*sql.Tx, time.Time) (string, error), reason, requestID string) (Publication, error) {
	pub, err := s.Get(ctx, p, roles, id)
	if err != nil {
		return Publication{}, err
	}
	team, err := s.team(ctx, pub.ProjectID)
	if err != nil {
		return Publication{}, err
	}
	if !authz.Can(p, publishRoles, authz.Resource{OrgID: pub.OrgID, TeamID: team, ProjectID: pub.ProjectID}) {
		return Publication{}, ErrForbidden
	}
	if pub.State != Published {
		return Publication{}, &Invalid{"only a published publication can change; this one is " + pub.State}
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		action, err := change(tx, s.now().UTC())
		if err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: action, ResourceType: "publication", ResourceID: id, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Publication{}, err
	}
	return s.load(ctx, id)
}
