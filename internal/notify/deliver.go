package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"samuori.invalid/samuori/internal/ids"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// Backoff is the wait before each retry; a delivery that fails every attempt is dead-lettered.
var Backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

// claimFor bounds how long a claimed delivery is hidden from other senders while it is attempted.
const claimFor = 5 * time.Minute

type Delivery struct {
	ID, ChannelID, EventID, EventType string
	State                             string
	Attempts                          int
	NextAttemptAt                     time.Time
	LastError                         string
	CreatedAt                         time.Time
	DeliveredAt                       *time.Time
}

// DeliverDue attempts the due deliveries and returns how many it attempted.
func (s *Service) DeliverDue(ctx context.Context) (int, error) {
	now := s.now().UTC()
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id FROM notification_deliveries WHERE state = 'pending' AND next_attempt_at <= ? ORDER BY next_attempt_at LIMIT 50`), now)
	if err != nil {
		return 0, err
	}
	var due []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, id)
	}
	rows.Close()
	n := 0
	for _, id := range due {
		claim := ids.New()
		res, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE notification_deliveries SET next_attempt_at = ?, claim = ? WHERE id = ? AND state = 'pending' AND next_attempt_at <= ?`), now.Add(claimFor), claim, id, now)
		if err != nil {
			return n, err
		}
		if c, _ := res.RowsAffected(); c != 1 {
			continue
		}
		n++
		if err := s.attempt(ctx, id, claim); err != nil {
			return n, err
		}
	}
	return n, nil
}

// attempt sends one delivery. Its result is recorded only under the claim it was sent with, so a slow attempt cannot
// overwrite a later one; delivery remains at least once if a controller stops between sending and recording.
func (s *Service) attempt(ctx context.Context, id, claim string) error {
	var orgID, channelID, raw string
	var attempts int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT org_id, channel_id, message, attempts FROM notification_deliveries WHERE id = ?`), id).Scan(&orgID, &channelID, &raw, &attempts); err != nil {
		return err
	}
	var m Message
	_ = json.Unmarshal([]byte(raw), &m)
	c, err := s.channel(ctx, orgID, channelID)
	if err == nil {
		err = s.send(ctx, c, m)
	}
	now := s.now().UTC()
	attempts++
	if err == nil {
		_, uerr := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE notification_deliveries SET state = 'delivered', attempts = ?, delivered_at = ?, last_error = NULL WHERE id = ? AND claim = ?`), attempts, now, id, claim)
		return uerr
	}
	if attempts > len(Backoff) {
		return s.db.InTx(ctx, func(tx *sql.Tx) error {
			res, uerr := tx.ExecContext(ctx, s.db.Rebind(`UPDATE notification_deliveries SET state = 'dead', attempts = ?, last_error = ? WHERE id = ? AND claim = ?`), attempts, clip(err.Error()), id, claim)
			if uerr != nil {
				return uerr
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil
			}
			return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{OrgID: orgID, Kind: authz.System}, Action: "notification.dead-letter", ResourceType: "notification-channel", ResourceID: channelID, Outcome: "failed", Reason: clip(err.Error())})
		})
	}
	_, uerr := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE notification_deliveries SET attempts = ?, next_attempt_at = ?, last_error = ? WHERE id = ? AND claim = ?`), attempts, now.Add(Backoff[attempts-1]), clip(err.Error()), id, claim)
	return uerr
}

// Deliveries lists recent deliveries of the organization, optionally in one state.
func (s *Service) Deliveries(ctx context.Context, actor authz.Principal, state string, limit int) ([]Delivery, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	q, args := `SELECT id, channel_id, event_id, event_type, state, attempts, next_attempt_at, COALESCE(last_error, ''), created_at, delivered_at FROM notification_deliveries WHERE org_id = ?`, []any{actor.OrgID}
	if state != "" {
		q, args = q+` AND state = ?`, append(args, state)
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(q+` ORDER BY created_at DESC LIMIT ?`), append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		var delivered sql.NullTime
		if err := rows.Scan(&d.ID, &d.ChannelID, &d.EventID, &d.EventType, &d.State, &d.Attempts, &d.NextAttemptAt, &d.LastError, &d.CreatedAt, &delivered); err != nil {
			return nil, err
		}
		if delivered.Valid {
			d.DeliveredAt = &delivered.Time
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Retry returns a dead-lettered delivery to the queue with a fresh attempt budget; it is audited.
func (s *Service) Retry(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE notification_deliveries SET state = 'pending', attempts = 0, next_attempt_at = ? WHERE id = ? AND org_id = ? AND state = 'dead'`), s.now().UTC(), id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "notification.retry", ResourceType: "notification-delivery", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}

// RunDelivery attempts due deliveries until ctx ends.
func (s *Service) RunDelivery(ctx context.Context, every time.Duration, onError func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := s.DeliverDue(ctx); err != nil && !errors.Is(err, context.Canceled) {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Notification is an inbox entry.
type Notification struct {
	ID        string
	Message   Message
	ReadAt    *time.Time
	CreatedAt time.Time
}

// Inbox lists a user's notifications, newest first.
func (s *Service) Inbox(ctx context.Context, p authz.Principal, unreadOnly bool, limit int) ([]Notification, error) {
	q := `SELECT id, message, read_at, created_at FROM notifications WHERE user_id = ? AND org_id = ?`
	if unreadOnly {
		q += ` AND read_at IS NULL`
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(q+` ORDER BY created_at DESC LIMIT ?`), p.ID, p.OrgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var raw string
		var read sql.NullTime
		if err := rows.Scan(&n.ID, &raw, &read, &n.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &n.Message)
		if read.Valid {
			n.ReadAt = &read.Time
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkRead marks a user's notifications read: one by ID, or all when id is empty.
func (s *Service) MarkRead(ctx context.Context, p authz.Principal, id string) error {
	q, args := `UPDATE notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL`, []any{s.now().UTC(), p.ID}
	if id != "" {
		q, args = q+` AND id = ?`, append(args, id)
	}
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(q), args...)
	return err
}
