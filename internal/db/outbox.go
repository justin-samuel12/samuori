package db

import (
	"context"
	"database/sql"
	"time"
)

type OutboxMessage struct {
	Seq     int64
	MsgID   string
	Subject string
	Payload []byte
}

// EnqueueOutbox must be called inside the transaction that changes the state the message announces.
func (d *DB) EnqueueOutbox(ctx context.Context, tx *sql.Tx, msgID, subject string, payload []byte) error {
	_, err := tx.ExecContext(ctx, d.Rebind(`INSERT INTO outbox (msg_id, subject, payload, created_at) VALUES (?, ?, ?, ?)`),
		msgID, subject, payload, time.Now().UTC())
	return err
}

func (d *DB) PendingOutbox(ctx context.Context, limit int) ([]OutboxMessage, error) {
	rows, err := d.SQL.QueryContext(ctx, d.Rebind(`SELECT seq, msg_id, subject, payload FROM outbox WHERE published_at IS NULL ORDER BY seq LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		if err := rows.Scan(&m.Seq, &m.MsgID, &m.Subject, &m.Payload); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *DB) MarkOutboxPublished(ctx context.Context, msgID string) error {
	_, err := d.SQL.ExecContext(ctx, d.Rebind(`UPDATE outbox SET published_at = ? WHERE msg_id = ? AND published_at IS NULL`), time.Now().UTC(), msgID)
	return err
}
