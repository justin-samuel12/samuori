package clusterjoin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

// MaxTTL bounds join tokens: they hand out the cluster's key and credentials.
const MaxTTL = time.Hour

var (
	ErrForbidden = errors.New("only administrators manage controller join tokens")
	ErrNotFound  = errors.New("join token not found")
	// ErrInvalid covers unknown, expired, used and revoked tokens alike, so a caller learns nothing about which.
	ErrInvalid = errors.New("the join token is invalid, expired, used or revoked")
)

const tokenPrefix = "samuorij_"

type Token struct {
	ID        string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	UsedHost  string
	RevokedAt *time.Time
}

type Store struct {
	db  *db.DB
	now func() time.Time
}

func NewStore(d *db.DB) *Store { return &Store{db: d, now: time.Now} }

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Create issues a single-use token; its value is returned once and only its hash is stored.
func (s *Store) Create(ctx context.Context, actor authz.Principal, ttl time.Duration, requestID string) (Token, string, error) {
	if !actor.IsOrgAdmin() {
		return Token{}, "", ErrForbidden
	}
	if ttl <= 0 || ttl > MaxTTL {
		ttl = MaxTTL
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Token{}, "", err
	}
	value := tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	now := s.now().UTC()
	t := Token{ID: ids.New(), CreatedBy: actor.ID, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO controller_join_tokens (id, token_hash, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`),
			t.ID, hash(value), t.CreatedBy, t.CreatedAt, t.ExpiresAt); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "controller.join-token.create", ResourceType: "controller-join-token", ResourceID: t.ID,
			Outcome: "succeeded", Reason: "expires " + t.ExpiresAt.Format(time.RFC3339), RequestID: requestID})
	})
	return t, value, err
}

func (s *Store) List(ctx context.Context, actor authz.Principal) ([]Token, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT id, created_by, created_at, expires_at, used_at, used_host, revoked_at FROM controller_join_tokens ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var used, revoked sql.NullTime
		var host sql.NullString
		if err := rows.Scan(&t.ID, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &used, &host, &revoked); err != nil {
			return nil, err
		}
		t.CreatedAt, t.ExpiresAt = t.CreatedAt.UTC(), t.ExpiresAt.UTC()
		if used.Valid {
			u := used.Time.UTC()
			t.UsedAt = &u
		}
		if revoked.Valid {
			r := revoked.Time.UTC()
			t.RevokedAt = &r
		}
		t.UsedHost = host.String
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Revoke(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE controller_join_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL AND used_at IS NULL`), s.now().UTC(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "controller.join-token.revoke", ResourceType: "controller-join-token", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}

// Redeem consumes a token for host and opens a pending join request bound to secretHash, the SHA-256 of a secret
// only the joining host holds. Exactly one caller can redeem a token; every refusal looks the same.
func (s *Store) Redeem(ctx context.Context, value, host, remoteAddr, secretHash, requestID string) (Request, error) {
	if !strings.HasPrefix(value, tokenPrefix) || len(value) > 128 || !ValidHost(host) || !validSecretHash(secretHash) {
		return Request{}, ErrInvalid
	}
	code, err := verificationCode()
	if err != nil {
		return Request{}, err
	}
	now := s.now().UTC()
	q := Request{ID: ids.New(), Host: host, RemoteAddr: remoteAddr, Code: code, State: StatePending, CreatedAt: now, ExpiresAt: now.Add(RequestTTL)}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE controller_join_tokens SET used_at = ?, used_host = ? WHERE token_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`),
			now, host, hash(value), now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrInvalid
		}
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM controller_join_tokens WHERE token_hash = ?`), hash(value)).Scan(&q.TokenID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO controller_join_requests (id, token_id, host, remote_addr, code, secret_hash, state, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			q.ID, q.TokenID, q.Host, q.RemoteAddr, q.Code, secretHash, q.State, q.CreatedAt, q.ExpiresAt); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{Kind: authz.System}, Action: "controller.join.request", ResourceType: "controller-join-request", ResourceID: q.ID,
			Outcome: "succeeded", Reason: "controller " + host + " from " + remoteAddr + " redeemed token " + q.TokenID + ", code " + code, RequestID: requestID})
	})
	if err != nil {
		return Request{}, err
	}
	return q, nil
}
