// Package operations records long-running requests so callers can poll for their outcome by a durable ID.
package operations

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

const (
	Pending   = "pending"
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
)

var ErrNotFound = errors.New("operation not found")

type Operation struct {
	ID, Kind, Status         string
	ResourceType, ResourceID string
	ErrorCode, ErrorMessage  string
	PrincipalID, OrgID       string
	CreatedAt, UpdatedAt     time.Time
}

type Store struct{ db *db.DB }

func NewStore(d *db.DB) *Store { return &Store{db: d} }

// Begin returns the operation already recorded for (principal, kind, key), or creates a pending one in tx.
func (s *Store) Begin(ctx context.Context, tx *sql.Tx, p authz.Principal, kind, key string) (Operation, bool, error) {
	op, err := s.byKey(ctx, tx, p.ID, kind, key)
	if err == nil {
		return op, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Operation{}, false, err
	}
	now := time.Now().UTC()
	op = Operation{ID: ids.New(), Kind: kind, Status: Pending, PrincipalID: p.ID, OrgID: p.OrgID, CreatedAt: now, UpdatedAt: now}
	_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO operations (id, org_id, principal_id, kind, status, idempotency_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		op.ID, op.OrgID, op.PrincipalID, kind, Pending, key, now, now)
	return op, true, err
}

// Finish records the terminal outcome; a failure carries a user-safe code and message.
func (s *Store) Finish(ctx context.Context, tx *sql.Tx, id, status, resourceType, resourceID, code, message string) error {
	_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE operations SET status = ?, resource_type = ?, resource_id = ?, error_code = ?, error_message = ?, updated_at = ? WHERE id = ?`),
		status, nullable(resourceType), nullable(resourceID), nullable(code), nullable(message), time.Now().UTC(), id)
	return err
}

// Discard removes an operation whose request was rejected without effect, so its key can be used again.
func (s *Store) Discard(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM operations WHERE id = ? AND status = ?`), id, Pending)
	return err
}

func (s *Store) SetStatus(ctx context.Context, tx *sql.Tx, id, status string) error {
	_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE operations SET status = ?, updated_at = ? WHERE id = ?`), status, time.Now().UTC(), id)
	return err
}

// Get returns an operation visible to p: its own, or any in the organization for organization administrators.
func (s *Store) Get(ctx context.Context, p authz.Principal, id string) (Operation, error) {
	op, err := scan(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectOp+` WHERE id = ? AND org_id = ?`), id, p.OrgID))
	if err != nil {
		return Operation{}, err
	}
	if op.PrincipalID != p.ID && !p.IsOrgAdmin() {
		return Operation{}, ErrNotFound
	}
	return op, nil
}

func (s *Store) byKey(ctx context.Context, tx *sql.Tx, principal, kind, key string) (Operation, error) {
	return scan(tx.QueryRowContext(ctx, s.db.Rebind(selectOp+` WHERE principal_id = ? AND kind = ? AND idempotency_key = ?`), principal, kind, key))
}

const selectOp = `SELECT id, org_id, principal_id, kind, status, resource_type, resource_id, error_code, error_message, created_at, updated_at FROM operations`

func scan(row *sql.Row) (Operation, error) {
	var op Operation
	var rt, rid, code, msg sql.NullString
	err := row.Scan(&op.ID, &op.OrgID, &op.PrincipalID, &op.Kind, &op.Status, &rt, &rid, &code, &msg, &op.CreatedAt, &op.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	op.ResourceType, op.ResourceID, op.ErrorCode, op.ErrorMessage = rt.String, rid.String, code.String, msg.String
	return op, err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
