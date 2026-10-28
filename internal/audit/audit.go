// Package audit appends audit events inside the caller's transaction (TM-90).
package audit

import (
	"context"
	"database/sql"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

type Event struct {
	Actor        authz.Principal
	Action       string
	ResourceType string
	ResourceID   string
	Outcome      string
	Reason       string
	RequestID    string
}

func Append(ctx context.Context, d *db.DB, tx *sql.Tx, e Event) error {
	_, err := tx.ExecContext(ctx, d.Rebind(`INSERT INTO audit_events
(id, at, org_id, actor_type, actor_id, action, resource_type, resource_id, outcome, reason, request_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		ids.New(), time.Now().UTC(), nullable(e.Actor.OrgID), string(e.Actor.Kind), nullable(e.Actor.ID),
		e.Action, e.ResourceType, nullable(e.ResourceID), e.Outcome, nullable(e.Reason), nullable(e.RequestID))
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
