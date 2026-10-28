package audit

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/db"
)

// Record is a stored audit event.
type Record struct {
	ID           string    `json:"id"`
	At           time.Time `json:"at"`
	OrgID        string    `json:"orgId,omitempty"`
	ActorType    string    `json:"actorType"`
	ActorID      string    `json:"actorId,omitempty"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resourceType"`
	ResourceID   string    `json:"resourceId,omitempty"`
	Outcome      string    `json:"outcome"`
	Reason       string    `json:"reason,omitempty"`
	RequestID    string    `json:"requestId,omitempty"`
}

// Filter selects an organization's events. Action ending in "*" matches a prefix; Text matches the reason.
type Filter struct {
	OrgID                                     string
	ActorID, Action, ResourceType, ResourceID string
	Outcome, RequestID, Text                  string
	Since, Until                              *time.Time
	Cursor                                    string
	Limit                                     int
}

var ErrBadCursor = errors.New("audit: invalid cursor")

const columns = `SELECT id, at, COALESCE(org_id, ''), actor_type, COALESCE(actor_id, ''), action, resource_type, COALESCE(resource_id, ''), outcome, COALESCE(reason, ''), COALESCE(request_id, '') FROM audit_events`

func (f Filter) where() (string, []any) {
	q, args := ` WHERE org_id = ?`, []any{f.OrgID}
	eq := func(col, v string) {
		if v != "" {
			q += ` AND ` + col + ` = ?`
			args = append(args, v)
		}
	}
	eq("actor_id", f.ActorID)
	eq("resource_type", f.ResourceType)
	eq("resource_id", f.ResourceID)
	eq("outcome", f.Outcome)
	eq("request_id", f.RequestID)
	if prefix, ok := strings.CutSuffix(f.Action, "*"); ok {
		q += ` AND action LIKE ? ESCAPE '\'`
		args = append(args, escapeLike(prefix)+"%")
	} else {
		eq("action", f.Action)
	}
	if f.Text != "" {
		q += ` AND LOWER(COALESCE(reason, '')) LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(strings.ToLower(f.Text))+"%")
	}
	if f.Since != nil {
		q += ` AND at >= ?`
		args = append(args, f.Since.UTC())
	}
	if f.Until != nil {
		q += ` AND at < ?`
		args = append(args, f.Until.UTC())
	}
	return q, args
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.At, &r.OrgID, &r.ActorType, &r.ActorID, &r.Action, &r.ResourceType, &r.ResourceID, &r.Outcome, &r.Reason, &r.RequestID); err != nil {
			return nil, err
		}
		r.At = r.At.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// Search returns events newest first and a cursor for the next page, or "" at the end.
func Search(ctx context.Context, d *db.DB, f Filter) ([]Record, string, error) {
	if f.OrgID == "" {
		return nil, "", errors.New("audit: an organization is required")
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 50
	}
	q, args := f.where()
	if f.Cursor != "" {
		at, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		q += ` AND (at < ? OR (at = ? AND id < ?))`
		args = append(args, at, at, id)
	}
	rows, err := d.SQL.QueryContext(ctx, d.Rebind(columns+q+` ORDER BY at DESC, id DESC LIMIT ?`), append(args, f.Limit+1)...)
	if err != nil {
		return nil, "", err
	}
	out, err := scanRecords(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > f.Limit {
		out = out[:f.Limit]
		last := out[len(out)-1]
		next = encodeCursor(last.At, last.ID)
	}
	return out, next, nil
}

// Export calls fn for matching events, oldest first, stopping after max events; truncated reports that more
// matched.
func Export(ctx context.Context, d *db.DB, f Filter, max int, fn func(Record) error) (truncated bool, err error) {
	if f.OrgID == "" {
		return false, errors.New("audit: an organization is required")
	}
	base, baseArgs := f.where()
	var lastAt time.Time
	lastID, n := "", 0
	for {
		q, args := base, slices.Clone(baseArgs)
		if lastID != "" {
			q += ` AND (at > ? OR (at = ? AND id > ?))`
			args = append(args, lastAt, lastAt, lastID)
		}
		rows, err := d.SQL.QueryContext(ctx, d.Rebind(columns+q+` ORDER BY at, id LIMIT 1000`), args...)
		if err != nil {
			return false, err
		}
		page, err := scanRecords(rows)
		if err != nil {
			return false, err
		}
		for _, r := range page {
			if n == max {
				return true, nil
			}
			if err := fn(r); err != nil {
				return false, err
			}
			lastAt, lastID, n = r.At, r.ID, n+1
		}
		if len(page) < 1000 {
			return false, nil
		}
	}
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	at, err := time.Parse(time.RFC3339Nano, ts)
	if !ok || err != nil || id == "" {
		return time.Time{}, "", ErrBadCursor
	}
	return at.UTC(), id, nil
}
