package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/ids"
)

// Lag keeps dispatch behind transactions that may still commit audit records with an earlier timestamp.
const Lag = 10 * time.Second

func (d *Dispatcher) cursor(ctx context.Context, tx *sql.Tx) (time.Time, string, error) {
	var at time.Time
	var id string
	err := tx.QueryRowContext(ctx, `SELECT at, event FROM notification_cursor WHERE id = 1`).Scan(&at, &id)
	if errors.Is(err, sql.ErrNoRows) {
		// The first run starts now: past events are history, not news.
		at = d.now().UTC().Add(-Lag)
		_, err = tx.ExecContext(ctx, d.db.Rebind(`INSERT INTO notification_cursor (id, at, event) VALUES (1, ?, '')`), at)
		return at, "", err
	}
	return at.UTC(), id, err
}

// Dispatch processes the next batch of audit records and returns how many it read.
func (d *Dispatcher) Dispatch(ctx context.Context) (int, error) {
	n := 0
	err := d.db.InTx(ctx, func(tx *sql.Tx) error {
		at, last, err := d.cursor(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, d.db.Rebind(`SELECT id, at, COALESCE(org_id, ''), action, resource_type, COALESCE(resource_id, ''), outcome FROM audit_events
			WHERE (at > ? OR (at = ? AND id > ?)) AND at <= ? AND org_id IS NOT NULL ORDER BY at, id LIMIT 200`), at, at, last, d.now().UTC().Add(-Lag))
		if err != nil {
			return err
		}
		var records []audit.Record
		for rows.Next() {
			var r audit.Record
			if err := rows.Scan(&r.ID, &r.At, &r.OrgID, &r.Action, &r.ResourceType, &r.ResourceID, &r.Outcome); err != nil {
				rows.Close()
				return err
			}
			records = append(records, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range records {
			e, err := d.classify(ctx, tx, r)
			if errors.Is(err, errSkip) {
				e, err = nil, nil
			}
			if err != nil {
				return err
			}
			if e != nil {
				if err := d.route(ctx, tx, e); err != nil {
					return err
				}
			}
			at, last = r.At, r.ID
		}
		n = len(records)
		if n == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, d.db.Rebind(`UPDATE notification_cursor SET at = ?, event = ? WHERE id = 1`), at, last)
		return err
	})
	return n, err
}

// route writes inbox entries and one delivery per matching enabled channel.
func (d *Dispatcher) route(ctx context.Context, tx *sql.Tx, e *event) error {
	raw, err := json.Marshal(e.msg)
	if err != nil {
		return err
	}
	now := d.now().UTC()
	seen := map[string]bool{}
	for _, u := range e.recipients {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		if _, err := tx.ExecContext(ctx, d.db.Rebind(`INSERT INTO notifications (id, org_id, user_id, event_id, event_type, message, created_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`),
			ids.New(), e.orgID, u, e.msg.ID, e.msg.Type, string(raw), now); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, d.db.Rebind(`SELECT r.channel_id, r.events, COALESCE(r.team_id, ''), COALESCE(r.project_id, '') FROM notification_rules r
		JOIN notification_channels c ON c.id = r.channel_id WHERE r.org_id = ? AND c.enabled = ?`), e.orgID, true)
	if err != nil {
		return err
	}
	var channels []string
	for rows.Next() {
		var channel, events, team, project string
		if err := rows.Scan(&channel, &events, &team, &project); err != nil {
			rows.Close()
			return err
		}
		var types []string
		_ = json.Unmarshal([]byte(events), &types)
		if slices.Contains(types, e.msg.Type) && (team == "" || team == e.teamID) && (project == "" || project == e.projectID) && !slices.Contains(channels, channel) {
			channels = append(channels, channel)
		}
	}
	rows.Close()
	for _, ch := range channels {
		if _, err := tx.ExecContext(ctx, d.db.Rebind(`INSERT INTO notification_deliveries (id, org_id, channel_id, event_id, event_type, message, state, next_attempt_at, created_at) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?) ON CONFLICT DO NOTHING`),
			ids.New(), e.orgID, ch, e.msg.ID, e.msg.Type, string(raw), now, now); err != nil {
			return err
		}
	}
	return nil
}

// Run dispatches until ctx ends.
func (d *Dispatcher) Run(ctx context.Context, every time.Duration, onError func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for {
			n, err := d.Dispatch(ctx)
			if err != nil {
				onError(err)
			}
			if err != nil || n < 200 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
