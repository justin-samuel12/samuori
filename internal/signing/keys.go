package signing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"regexp"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

// Secrets keeps private keys as controller-only secrets.
type Secrets interface {
	CreateControllerSecret(ctx context.Context, actor authz.Principal, name, description string, value []byte, requestID string) (string, error)
	ControllerValue(ctx context.Context, orgID, secretID string) ([]byte, error)
}

// KeyInfo is a signing key's public record; the private key never leaves the secret store and the controller, or the
// external key service for a remote key.
type KeyInfo struct {
	ID, Name, PublicKey, State string
	// Backend is local, or the secret provider type holding a remote key; KeyRef names that key.
	Backend, ProviderID, KeyRef string
	CreatedAt                   time.Time
	RetiredAt                   *time.Time
}

// Remote is an external key service: OpenBao Transit, a Vault-compatible Transit engine, or AWS KMS. Private keys
// never leave it; Samuori asks it to sign SHA-256 digests.
type Remote interface {
	// Describe returns the key's public key as PEM, the backend name and the reference to store, which may pin a
	// key version.
	Describe(ctx context.Context, orgID, providerID, ref string) (pemKey, backend, pinned string, err error)
	// Sign returns an ASN.1 ECDSA signature over a SHA-256 digest.
	Sign(ctx context.Context, orgID, providerID, ref string, digest []byte) ([]byte, error)
}

// UseRemote enables keys held by external key services.
func (s *Store) UseRemote(r Remote) { s.remote = r }

var (
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errors.New("not found")
	ErrNoKey     = errors.New("no active signing key; an administrator must create one")
	ErrConflict  = errors.New("conflict")
	keyName      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

type Store struct {
	db      *db.DB
	secrets Secrets
	remote  Remote
	now     func() time.Time
}

func NewStore(d *db.DB, s Secrets) *Store { return &Store{db: d, secrets: s, now: time.Now} }

// Create generates a P-256 key on the controller and makes it the active key; the previous active key is retired
// and stays listed so existing signatures can still be verified.
func (s *Store) Create(ctx context.Context, actor authz.Principal, name, requestID string) (KeyInfo, error) {
	if !actor.IsOrgAdmin() {
		return KeyInfo{}, ErrForbidden
	}
	if !keyName.MatchString(name) {
		return KeyInfo{}, ErrConflict
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return KeyInfo{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return KeyInfo{}, err
	}
	pub, err := PublicPEM(&priv.PublicKey)
	if err != nil {
		return KeyInfo{}, err
	}
	secretID, err := s.secrets.CreateControllerSecret(ctx, actor, "signing-key-"+name, "image signing key "+name, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), requestID)
	if err != nil {
		return KeyInfo{}, err
	}
	k := KeyInfo{ID: ids.New(), Name: name, PublicKey: pub, State: "active", Backend: "local", CreatedAt: s.now().UTC()}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE signing_keys SET state = 'retired', retired_at = ? WHERE org_id = ? AND state = 'active'`), k.CreatedAt, actor.OrgID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO signing_keys (id, org_id, name, public_key, secret_id, state, created_by, created_at) VALUES (?, ?, ?, ?, ?, 'active', ?, ?)`),
			k.ID, actor.OrgID, name, pub, secretID, actor.ID, k.CreatedAt); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "signing_key.create", ResourceType: "signing_key", ResourceID: k.ID, Outcome: "succeeded", Reason: name, RequestID: requestID})
	})
	return k, err
}

// CreateRemote registers a key held by an external key service and makes it the active key. The key must be ECDSA
// P-256, and a test signature must verify with its public key before it is accepted.
func (s *Store) CreateRemote(ctx context.Context, actor authz.Principal, name, providerID, ref, requestID string) (KeyInfo, error) {
	if !actor.IsOrgAdmin() {
		return KeyInfo{}, ErrForbidden
	}
	if !keyName.MatchString(name) {
		return KeyInfo{}, ErrConflict
	}
	if s.remote == nil {
		return KeyInfo{}, errors.New("signing: external key services are not configured")
	}
	pemKey, backend, pinned, err := s.remote.Describe(ctx, actor.OrgID, providerID, ref)
	if err != nil {
		return KeyInfo{}, &Invalid{"the key could not be read: " + clip(err.Error())}
	}
	pub, err := ParsePublic(pemKey)
	if err != nil || pub.Curve.Params().Name != "P-256" {
		return KeyInfo{}, &Invalid{"the key must be ECDSA P-256 (ecdsa-p256 in Transit, ECC_NIST_P256 in KMS)"}
	}
	challenge := make([]byte, 32)
	_, _ = rand.Read(challenge)
	sig, err := s.remote.Sign(ctx, actor.OrgID, providerID, pinned, challenge)
	if err != nil {
		return KeyInfo{}, &Invalid{"the key service refused a test signature: " + clip(err.Error())}
	}
	if !ecdsa.VerifyASN1(pub, challenge, sig) {
		return KeyInfo{}, &Invalid{"the key service's test signature does not verify with its public key"}
	}
	pubPEM, err := PublicPEM(pub)
	if err != nil {
		return KeyInfo{}, err
	}
	k := KeyInfo{ID: ids.New(), Name: name, PublicKey: pubPEM, State: "active", Backend: backend, ProviderID: providerID, KeyRef: pinned, CreatedAt: s.now().UTC()}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE signing_keys SET state = 'retired', retired_at = ? WHERE org_id = ? AND state = 'active'`), k.CreatedAt, actor.OrgID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO signing_keys (id, org_id, name, public_key, secret_id, state, created_by, created_at, backend, provider_id, key_ref) VALUES (?, ?, ?, ?, '', 'active', ?, ?, ?, ?, ?)`),
			k.ID, actor.OrgID, name, pubPEM, actor.ID, k.CreatedAt, backend, providerID, pinned); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "signing_key.create", ResourceType: "signing_key", ResourceID: k.ID, Outcome: "succeeded", Reason: name + " in " + backend + " " + pinned, RequestID: requestID})
	})
	return k, err
}

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

func clip(v string) string {
	if len(v) > 300 {
		return v[:300]
	}
	return v
}

// remoteSigner signs through the external key service with the context of the publication that asked.
type remoteSigner struct {
	ctx                       context.Context
	remote                    Remote
	orgID, providerID, keyRef string
}

func (r remoteSigner) Sign(data []byte) ([]byte, error) {
	sum := sha256.Sum256(data)
	return r.remote.Sign(r.ctx, r.orgID, r.providerID, r.keyRef, sum[:])
}

func (r remoteSigner) Capabilities() Capabilities {
	return Capabilities{KeyBased: true, Attestations: []string{SLSAProvenanceV1}}
}

// Retire stops a key from signing; its public key stays available for verification.
func (s *Store) Retire(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE signing_keys SET state = 'retired', retired_at = ? WHERE id = ? AND org_id = ? AND state = 'active'`), s.now().UTC(), id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "signing_key.retire", ResourceType: "signing_key", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}

// List returns every key of the organization; public keys are not secret.
func (s *Store) List(ctx context.Context, orgID string) ([]KeyInfo, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, name, public_key, state, created_at, retired_at, backend, COALESCE(provider_id, ''), COALESCE(key_ref, '') FROM signing_keys WHERE org_id = ? ORDER BY created_at DESC`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyInfo
	for rows.Next() {
		var k KeyInfo
		var retired sql.NullTime
		if err := rows.Scan(&k.ID, &k.Name, &k.PublicKey, &k.State, &k.CreatedAt, &retired, &k.Backend, &k.ProviderID, &k.KeyRef); err != nil {
			return nil, err
		}
		if retired.Valid {
			k.RetiredAt = &retired.Time
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Active returns a signer for the organization's active key: a local key loaded for the controller's own use, or a
// handle that signs through the external key service with ctx.
func (s *Store) Active(ctx context.Context, orgID string) (Signer, KeyInfo, error) {
	var info KeyInfo
	var secretID string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id, name, public_key, secret_id, created_at, backend, COALESCE(provider_id, ''), COALESCE(key_ref, '') FROM signing_keys WHERE org_id = ? AND state = 'active'`), orgID).
		Scan(&info.ID, &info.Name, &info.PublicKey, &secretID, &info.CreatedAt, &info.Backend, &info.ProviderID, &info.KeyRef)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, KeyInfo{}, ErrNoKey
	}
	if err != nil {
		return nil, KeyInfo{}, err
	}
	info.State = "active"
	if info.Backend != "local" {
		if s.remote == nil {
			return nil, KeyInfo{}, errors.New("signing: the active key is held by an external key service that is not configured")
		}
		return remoteSigner{ctx, s.remote, orgID, info.ProviderID, info.KeyRef}, info, nil
	}
	raw, err := s.secrets.ControllerValue(ctx, orgID, secretID)
	if err != nil {
		return nil, KeyInfo{}, err
	}
	k, err := ParseKey(info.ID, raw)
	return k, info, err
}

// PublicKey returns the public key of a key by ID, active or retired.
func (s *Store) PublicKey(ctx context.Context, orgID, id string) (*ecdsa.PublicKey, error) {
	var pemText string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT public_key FROM signing_keys WHERE org_id = ? AND id = ?`), orgID, id).Scan(&pemText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return ParsePublic(pemText)
}
