package clusterjoin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// RequestTTL bounds how long a join request waits for a decision and for its bundle to be collected.
const RequestTTL = time.Hour

const (
	StatePending   = "pending"
	StateAccepted  = "accepted"
	StateRejected  = "rejected"
	StateCompleted = "completed"
	StateExpired   = "expired"
)

var (
	ErrDecided  = errors.New("the join request was already decided or has expired")
	ErrRejected = errors.New("an administrator rejected the join request")
	ErrExpired  = errors.New("the join request expired before it was accepted and collected")
)

// Request is a controller asking to join; the bundle is sent only after an administrator accepts it
// (docs/operators/high-availability.md#join).
type Request struct {
	ID         string
	TokenID    string
	Host       string
	RemoteAddr string
	Code       string
	State      string
	DecidedBy  string
	DecidedAt  *time.Time
	Reason     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func verificationCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, codeAlphabet[int(c)%len(codeAlphabet)])
	}
	return string(out), nil
}

func validSecretHash(h string) bool {
	if len(h) != 64 || strings.ToLower(h) != h {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

func (s *Store) expire(ctx context.Context) error {
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE controller_join_requests SET state = 'expired' WHERE state IN ('pending', 'accepted') AND expires_at <= ?`), s.now().UTC())
	return err
}

func scanRequest(scan func(...any) error) (Request, error) {
	var q Request
	var decided sql.NullTime
	if err := scan(&q.ID, &q.TokenID, &q.Host, &q.RemoteAddr, &q.Code, &q.State, &q.DecidedBy, &decided, &q.Reason, &q.CreatedAt, &q.ExpiresAt); err != nil {
		return Request{}, err
	}
	q.CreatedAt, q.ExpiresAt = q.CreatedAt.UTC(), q.ExpiresAt.UTC()
	if decided.Valid {
		d := decided.Time.UTC()
		q.DecidedAt = &d
	}
	return q, nil
}

const requestColumns = `id, token_id, host, remote_addr, code, state, decided_by, decided_at, reason, created_at, expires_at`

func (s *Store) ListRequests(ctx context.Context, actor authz.Principal) ([]Request, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	if err := s.expire(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT `+requestColumns+` FROM controller_join_requests ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		q, err := scanRequest(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// Decide accepts or rejects a pending request. It never sends anything itself; the host collects the bundle.
func (s *Store) Decide(ctx context.Context, actor authz.Principal, id string, accept bool, reason, requestID string) (Request, error) {
	if !actor.IsOrgAdmin() {
		return Request{}, ErrForbidden
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	state, action := StateRejected, "controller.join.reject"
	if accept {
		state, action = StateAccepted, "controller.join.accept"
	}
	now := s.now().UTC()
	var q Request
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		q, err = scanRequest(tx.QueryRowContext(ctx, s.db.Rebind(`SELECT `+requestColumns+` FROM controller_join_requests WHERE id = ?`), id).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE controller_join_requests SET state = ?, decided_by = ?, decided_at = ?, reason = ? WHERE id = ? AND state = 'pending' AND expires_at > ?`),
			state, actor.ID, now, reason, id, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrDecided
		}
		q.State, q.DecidedBy, q.DecidedAt, q.Reason = state, actor.ID, &now, reason
		// A removed host coming back: its removal stops being applied, but its old identity stays refused.
		if accept {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE controller_removals SET state = 'rejoined', updated_at = ? WHERE host = ? AND state = 'removed'`), now, q.Host); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: action, ResourceType: "controller-join-request", ResourceID: id,
			Outcome: "succeeded", Reason: strings.TrimSpace("controller " + q.Host + ", code " + q.Code + " " + reason), RequestID: requestID})
	})
	return q, err
}

// Poll reports a request to the host holding its secret. A wrong secret is indistinguishable from an unknown id.
// For an accepted request, bundle builds the host's bundle and the request is completed in the same step, so the
// bundle is handed out at most once.
func (s *Store) Poll(ctx context.Context, id, secret string, bundle func(host string) (Bundle, error), requestID string) (Request, *Bundle, error) {
	if id == "" || len(secret) < 32 || len(secret) > 128 {
		return Request{}, nil, ErrInvalid
	}
	if err := s.expire(ctx); err != nil {
		return Request{}, nil, err
	}
	var stored string
	row := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+requestColumns+`, secret_hash FROM controller_join_requests WHERE id = ?`), id)
	q, err := scanRequest(func(dest ...any) error { return row.Scan(append(dest, &stored)...) })
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, nil, ErrInvalid
	}
	if err != nil {
		return Request{}, nil, err
	}
	if subtle.ConstantTimeCompare([]byte(hash(secret)), []byte(stored)) != 1 {
		return Request{}, nil, ErrInvalid
	}
	switch q.State {
	case StatePending:
		return q, nil, nil
	case StateRejected:
		return q, nil, ErrRejected
	case StateExpired:
		return q, nil, ErrExpired
	case StateAccepted:
	default:
		return Request{}, nil, ErrInvalid
	}
	b, err := bundle(q.Host)
	if err != nil {
		return Request{}, nil, err
	}
	now := s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE controller_join_requests SET state = 'completed' WHERE id = ? AND state = 'accepted' AND expires_at > ?`), id, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrInvalid
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{Kind: authz.System}, Action: "controller.join", ResourceType: "controller-join-request", ResourceID: id,
			Outcome: "succeeded", Reason: "controller " + q.Host + " collected its join bundle", RequestID: requestID})
	})
	if err != nil {
		return Request{}, nil, err
	}
	q.State = StateCompleted
	return q, &b, nil
}

const codePrefix = "samuoric_"

type joinCode struct {
	URL   string `json:"url"`
	Pin   string `json:"pin"`
	Token string `json:"token"`
}

// EncodeCode joins the controller URL, its certificate pin and a token into one value for `cluster join --code`.
func EncodeCode(url, pin, token string) string {
	raw, _ := json.Marshal(joinCode{URL: url, Pin: pin, Token: token})
	return codePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func DecodeCode(code string) (url, pin, token string, err error) {
	code = strings.TrimSpace(code)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, codePrefix))
	var c joinCode
	if err != nil || !strings.HasPrefix(code, codePrefix) || json.Unmarshal(raw, &c) != nil ||
		!strings.HasPrefix(c.URL, "https://") || !strings.HasPrefix(c.Pin, "sha256:") || !strings.HasPrefix(c.Token, tokenPrefix) {
		return "", "", "", errors.New("the join code is malformed; copy it again from the portal")
	}
	return c.URL, c.Pin, c.Token, nil
}
