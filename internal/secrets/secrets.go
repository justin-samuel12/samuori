// Package secrets holds the local encrypted secret store and project-scoped bindings (ADRs 0015 and 0029). Values
// are encrypted at rest, never returned by the API and released only to a build attempt that holds a current lease
// (docs/architecture/secrets.md).
package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

const (
	MaxValueBytes = 64 << 10
	ProviderLocal = "local"
	Active        = "active"
	Disabled      = "disabled"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("changed since it was read")
	// ErrUnavailable means a binding exists but cannot be released: disabled, rotated past the plan, or for another
	// usage.
	ErrUnavailable = errors.New("the binding cannot be released")
)

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

type Secret struct {
	ID          string
	Name        string
	Type        string
	Description string
	Version     int
	// ControllerOnly secrets (registry credentials) are never bound to projects or released to workers.
	ControllerOnly bool
	// SamuoriUses names what Samuori keeps a controller-only secret for; empty means nothing refers to it any more.
	SamuoriUses []string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Provider struct {
	ID, Name, Type     string
	Config             ExternalConfig
	CredentialSecretID string
	LastCheck          *ProviderCheck
	CreatedAt          time.Time
}

type Binding struct {
	ID                string
	ProjectID         string
	LogicalID         string
	ProviderID        string
	ProviderType      string
	ProviderReference string
	SecretName        string
	ExpiresAt         *time.Time
	VersionPolicy     string
	Usage             string
	PoolIDs           []string
	Status            string
	Version           int
	Reason            string
	CreatedBy         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	// GrantID is set when a developer or maintainer bound a key an administrator granted to the project.
	GrantID string
	// Encoding is how an external store holds the value: "" (as is) or "base64", decoded before delivery.
	Encoding string
}

// Allows reports whether the binding may be used for a usage (build or test).
func (b Binding) Allows(usage string) bool { return b.Usage == usage || b.Usage == "both" }

type Service struct {
	db       *db.DB
	aead     cipher.AEAD
	keyID    string
	key      []byte
	now      func() time.Time
	external map[string]External
}

// NewService loads the store key from keyDir/secrets.key, creating it on first use. A key readable by group or
// others is refused.
func NewService(d *db.DB, keyDir string) (*Service, error) {
	return open(d, filepath.Join(keyDir, "secrets.key"), true)
}

// NewServiceWithKey uses an existing key file, such as the cluster key every controller shares (ADR 0034).
func NewServiceWithKey(d *db.DB, keyFile string) (*Service, error) {
	return open(d, keyFile, false)
}

func open(d *db.DB, p string, create bool) (*Service, error) {
	keyDir := filepath.Dir(p)
	key, err := os.ReadFile(p)
	if create && errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(keyDir, 0o700); err != nil {
			return nil, err
		}
		err = os.WriteFile(p, key, 0o600)
	} else if err == nil {
		st, serr := os.Stat(p)
		if serr != nil {
			return nil, serr
		}
		if st.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("secrets: %s must not be readable by group or others", p)
		}
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: %s must hold 32 bytes", p)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	return &Service{db: d, aead: aead, keyID: hex.EncodeToString(sum[:8]), key: key, now: time.Now}, nil
}

// Derive returns a 32-byte key for one purpose, derived from the store key, so that every controller holding the
// store key derives the same value and no further key file is needed.
func (s *Service) Derive(purpose string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("samuori-derived-key/" + purpose))
	return mac.Sum(nil)
}

// aad binds a ciphertext to its secret and version so rows cannot be swapped.
func aad(secretID string, version int) []byte {
	return []byte(secretID + "\x00" + strconv.Itoa(version))
}

func (s *Service) seal(secretID string, version int, value []byte) (nonce, ct []byte, err error) {
	nonce = make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, s.aead.Seal(nil, nonce, value, aad(secretID, version)), nil
}

var secretName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

func validValue(typ string, value []byte) error {
	if typ != "text" && typ != "binary" {
		return &Invalid{"type must be text or binary"}
	}
	if len(value) == 0 || len(value) > MaxValueBytes {
		return &Invalid{fmt.Sprintf("the value must be between 1 byte and %d KiB", MaxValueBytes>>10)}
	}
	return nil
}

func (s *Service) CreateSecret(ctx context.Context, actor authz.Principal, name, typ, description string, value []byte, requestID string) (Secret, error) {
	return s.create(ctx, actor, name, typ, description, value, false, requestID)
}

// CreateControllerSecret stores a value that only the controller may use, such as an image signing key; it is
// controller-only from the moment it exists, so it can never be bound to a project.
func (s *Service) CreateControllerSecret(ctx context.Context, actor authz.Principal, name, description string, value []byte, requestID string) (string, error) {
	sec, err := s.create(ctx, actor, name, "binary", description, value, true, requestID)
	return sec.ID, err
}

func (s *Service) create(ctx context.Context, actor authz.Principal, name, typ, description string, value []byte, controllerOnly bool, requestID string) (Secret, error) {
	if !actor.IsOrgAdmin() {
		return Secret{}, ErrForbidden
	}
	if !secretName.MatchString(name) {
		return Secret{}, &Invalid{"the name must be lower-case letters, digits, '.', '_' or '-', starting with a letter or digit"}
	}
	if err := validValue(typ, value); err != nil {
		return Secret{}, err
	}
	id, now := ids.New(), s.now().UTC()
	nonce, ct, err := s.seal(id, 1, value)
	if err != nil {
		return Secret{}, err
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO secrets (id, org_id, name, type, description, current_version, created_by, created_at, updated_at, controller_only) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`),
			id, actor.OrgID, name, typ, description, actor.ID, now, now, controllerOnly); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO secret_versions (secret_id, version, key_id, nonce, ciphertext, created_by, created_at) VALUES (?, 1, ?, ?, ?, ?, ?)`),
			id, s.keyID, nonce, ct, actor.ID, now); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.create", ResourceType: "secret", ResourceID: id, Outcome: "succeeded", Reason: name + " v1", RequestID: requestID})
	})
	if err != nil {
		return Secret{}, err
	}
	return s.secret(ctx, actor.OrgID, id)
}

// Rotate stores a new version. Plans keep the binding version they were admitted with, so a rotation made during a
// build makes that build's later releases fail rather than deliver a value it was not admitted with.
func (s *Service) Rotate(ctx context.Context, actor authz.Principal, id string, ifVersion int, value []byte, requestID string) (Secret, error) {
	if !actor.IsOrgAdmin() {
		return Secret{}, ErrForbidden
	}
	cur, err := s.secret(ctx, actor.OrgID, id)
	if err != nil {
		return Secret{}, err
	}
	if err := validValue(cur.Type, value); err != nil {
		return Secret{}, err
	}
	if cur.Version != ifVersion {
		return Secret{}, ErrConflict
	}
	next, now := cur.Version+1, s.now().UTC()
	nonce, ct, err := s.seal(id, next, value)
	if err != nil {
		return Secret{}, err
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secrets SET current_version = ?, updated_at = ? WHERE id = ? AND org_id = ? AND current_version = ?`), next, now, id, actor.OrgID, cur.Version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO secret_versions (secret_id, version, key_id, nonce, ciphertext, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			id, next, s.keyID, nonce, ct, actor.ID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_bindings SET version = version + 1, updated_by = ?, updated_at = ? WHERE provider_ref = ? AND status = ? AND version_policy = 'latest'`), actor.ID, now, id, Active); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.rotate", ResourceType: "secret", ResourceID: id, Outcome: "succeeded", Reason: fmt.Sprintf("%s v%d", cur.Name, next), RequestID: requestID})
	})
	if err != nil {
		return Secret{}, err
	}
	return s.secret(ctx, actor.OrgID, id)
}

// LocalProvider returns the organization's built-in local encrypted provider, creating its record on first use.
func (s *Service) LocalProvider(ctx context.Context, orgID string) (Provider, error) {
	p, err := scanProvider(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+providerCols+` FROM secret_providers WHERE org_id = ? AND type = ?`), orgID, ProviderLocal))
	if !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	var orgs int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM organizations WHERE id = ?`), orgID).Scan(&orgs); err != nil {
		return Provider{}, err
	}
	if orgs == 0 {
		return Provider{}, ErrNotFound
	}
	_, err = s.db.SQL.ExecContext(ctx, s.db.Rebind(`INSERT INTO secret_providers (id, org_id, name, type, created_at) VALUES (?, ?, ?, ?, ?)`), ids.New(), orgID, "Local encrypted store", ProviderLocal, s.now().UTC())
	if err != nil && !isUnique(err) {
		return Provider{}, err
	}
	return scanProvider(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+providerCols+` FROM secret_providers WHERE org_id = ? AND type = ?`), orgID, ProviderLocal))
}

const providerCols = `id, name, type, created_at, config, COALESCE(credential_secret_id, ''), COALESCE(last_check, '')`

func scanProvider(sc interface{ Scan(...any) error }) (Provider, error) {
	var p Provider
	var cfg, check string
	if err := sc.Scan(&p.ID, &p.Name, &p.Type, &p.CreatedAt, &cfg, &p.CredentialSecretID, &check); err != nil {
		return p, err
	}
	_ = json.Unmarshal([]byte(cfg), &p.Config)
	if check != "" {
		var c ProviderCheck
		if json.Unmarshal([]byte(check), &c) == nil {
			p.LastCheck = &c
		}
	}
	return p, nil
}

func (s *Service) provider(ctx context.Context, orgID, id string) (Provider, error) {
	p, err := scanProvider(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+providerCols+` FROM secret_providers WHERE id = ? AND org_id = ?`), id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	return p, err
}

func (s *Service) Providers(ctx context.Context, actor authz.Principal) ([]Provider, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	if _, err := s.LocalProvider(ctx, actor.OrgID); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+providerCols+` FROM secret_providers WHERE org_id = ? ORDER BY name`), actor.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Check tests a provider: the local store seals and opens a random value; an external provider proves connectivity
// and authentication. The result is recorded on the provider; detail describes the identity used.
func (s *Service) Check(ctx context.Context, actor authz.Principal, providerID string) (string, error) {
	if !actor.IsOrgAdmin() {
		return "", ErrForbidden
	}
	p, err := s.provider(ctx, actor.OrgID, providerID)
	if err != nil {
		return "", err
	}
	detail, err := "", error(nil)
	if p.Type == ProviderLocal {
		err = s.checkLocal()
	} else {
		detail, err = s.checkExternal(ctx, actor.OrgID, p)
	}
	c := ProviderCheck{Status: "ok", TestedAt: s.now().UTC(), Detail: detail}
	if err != nil {
		c.Status, c.Detail = "failed", clipText(err.Error(), 500)
	}
	s.recordCheck(ctx, p.ID, c)
	return c.Detail, err
}

func clipText(v string, n int) string {
	if len(v) > n {
		return v[:n]
	}
	return v
}

func (s *Service) checkLocal() error {
	probe := make([]byte, 32)
	if _, err := rand.Read(probe); err != nil {
		return err
	}
	nonce, ct, err := s.seal("check", 0, probe)
	if err != nil {
		return err
	}
	out, err := s.aead.Open(nil, nonce, ct, aad("check", 0))
	if err != nil || string(out) != string(probe) {
		return errors.New("the store key failed a round trip")
	}
	return nil
}

// Store writes a local secret by name: a new name creates version 1, an existing one a new version of the same type.
func (s *Service) Store(ctx context.Context, actor authz.Principal, providerID, name, typ string, value []byte, requestID string) (Secret, error) {
	if !actor.IsOrgAdmin() {
		return Secret{}, ErrForbidden
	}
	p, err := s.provider(ctx, actor.OrgID, providerID)
	if err != nil {
		return Secret{}, err
	}
	if p.Type != ProviderLocal {
		return Secret{}, &Invalid{"values can be stored only in the local encrypted provider"}
	}
	cur, err := scanSecret(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+secretCols+` FROM secrets WHERE org_id = ? AND name = ?`), actor.OrgID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return s.CreateSecret(ctx, actor, name, typ, "", value, requestID)
	}
	if err != nil {
		return Secret{}, err
	}
	if cur.Type != typ {
		return Secret{}, &Invalid{"a secret with this name exists with type " + cur.Type}
	}
	return s.Rotate(ctx, actor, cur.ID, cur.Version, value, requestID)
}

const secretCols = `id, name, type, description, current_version, controller_only, created_by, created_at, updated_at`

func scanSecret(sc interface{ Scan(...any) error }) (Secret, error) {
	var x Secret
	err := sc.Scan(&x.ID, &x.Name, &x.Type, &x.Description, &x.Version, &x.ControllerOnly, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt)
	return x, err
}

// MarkControllerOnly reserves a secret for controller use, such as registry push credentials. A secret bound to a
// project cannot be reserved, and a reserved secret can never be bound (SEC-WRK-08).
func (s *Service) MarkControllerOnly(ctx context.Context, tx *sql.Tx, orgID, secretID string) error {
	var bound int
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM secret_bindings b JOIN secret_providers p ON p.id = b.provider_id WHERE p.type = 'local' AND b.provider_ref = ? AND b.status = 'active'`), secretID).Scan(&bound); err != nil {
		return err
	}
	if bound > 0 {
		return &Invalid{"the secret is bound to a project; build secrets cannot be registry credentials"}
	}
	res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secrets SET controller_only = ? WHERE id = ? AND org_id = ?`), true, secretID, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return &Invalid{"the secret does not exist in this organization"}
	}
	return nil
}

// ControllerValue returns the current value of a controller-only secret, for the controller's own use.
func (s *Service) ControllerValue(ctx context.Context, orgID, secretID string) ([]byte, error) {
	sec, err := s.secret(ctx, orgID, secretID)
	if err != nil {
		return nil, err
	}
	if !sec.ControllerOnly {
		return nil, ErrUnavailable
	}
	var keyID string
	var nonce, ct []byte
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT key_id, nonce, ciphertext FROM secret_versions WHERE secret_id = ? AND version = ?`), sec.ID, sec.Version).Scan(&keyID, &nonce, &ct); err != nil {
		return nil, err
	}
	if keyID != s.keyID {
		return nil, errors.New("secrets: the value was encrypted with a different store key")
	}
	return s.aead.Open(nil, nonce, ct, aad(sec.ID, sec.Version))
}

func (s *Service) secret(ctx context.Context, orgID, id string) (Secret, error) {
	x, err := scanSecret(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+secretCols+` FROM secrets WHERE id = ? AND org_id = ?`), id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Secret{}, ErrNotFound
	}
	return x, err
}

// ListSecrets returns metadata only; values never leave the store through it.
func (s *Service) ListSecrets(ctx context.Context, actor authz.Principal) ([]Secret, error) {
	if !canReview(actor) {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+secretCols+` FROM secrets WHERE org_id = ? ORDER BY name`), actor.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Secret{}
	for rows.Next() {
		x, err := scanSecret(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	uses, err := s.controllerUses(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].ControllerOnly {
			out[i].SamuoriUses = uses[out[i].ID]
		}
	}
	return out, nil
}

func canReview(p authz.Principal) bool {
	return authz.Can(p, []authz.Role{authz.Administrator, authz.SecurityReviewer}, authz.Resource{OrgID: p.OrgID})
}

var logicalID = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

type BindRequest struct {
	ProjectID         string
	LogicalID         string
	ProviderID        string
	ProviderReference string
	Usage             string
	VersionPolicy     string
	PoolIDs           []string
	ExpiresAt         *time.Time
	Reason            string
	Encoding          string
}

// Bind maps a project's logical secret ID to a secret in a provider. One active binding per project and ID exists at
// a time; binding again replaces it. Only administrators bind directly; developers bind granted keys (BindGrant).
func (s *Service) Bind(ctx context.Context, actor authz.Principal, req BindRequest, requestID string) (Binding, error) {
	if !actor.IsOrgAdmin() {
		return Binding{}, ErrForbidden
	}
	if !logicalID.MatchString(req.LogicalID) {
		return Binding{}, &Invalid{"the logical ID must match the spec's secret ID pattern"}
	}
	if req.Usage == "" {
		req.Usage = "build"
	}
	if !slices.Contains([]string{"build", "test", "both"}, req.Usage) {
		return Binding{}, &Invalid{"usage must be build, test or both"}
	}
	if req.Reason == "" {
		return Binding{}, &Invalid{"a reason is required"}
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(s.now()) {
		return Binding{}, &Invalid{"the expiry must be in the future"}
	}
	t, err := s.resolve(ctx, actor.OrgID, req.ProjectID, req.ProviderID, req.ProviderReference, req.VersionPolicy, req.PoolIDs)
	if err != nil {
		return Binding{}, err
	}
	if isKeySet(t.prov.Type, t.ref) {
		return Binding{}, &Invalid{"a binding needs one key: add #key to the reference, or grant the set and let the project choose its keys"}
	}
	if t.encoding, err = s.checkEncoding(ctx, actor.OrgID, t, req.Encoding); err != nil {
		return Binding{}, err
	}
	return s.insertBinding(ctx, actor, req.ProjectID, req.LogicalID, t, req.Usage, req.ExpiresAt, req.Reason, "", requestID)
}

// target is a validated provider reference: what a binding or a grant points at.
type target struct {
	prov          Provider
	ref, label    string
	versionPolicy string
	poolIDs       []string
	encoding      string
}

const EncodingBase64 = "base64"

// checkEncoding accepts a value encoding only for external stores, after reading the value once to prove it decodes.
func (s *Service) checkEncoding(ctx context.Context, orgID string, t target, encoding string) (string, error) {
	switch {
	case encoding == "":
		return "", nil
	case encoding != EncodingBase64:
		return "", &Invalid{"the encoding must be empty or base64"}
	case t.prov.Type == ProviderLocal:
		return "", &Invalid{"base64 decoding applies to external stores; store binary values in Samuori as binary secrets"}
	}
	v, err := s.readExternal(ctx, orgID, t.prov.ID, t.ref, t.versionPolicy)
	if err != nil {
		return "", &Invalid{"the provider could not read that reference: " + clipText(err.Error(), 300)}
	}
	if _, err := decodeValue(v, encoding); err != nil {
		return "", &Invalid{"the value at that reference is not valid base64"}
	}
	return encoding, nil
}

// decodeValue turns a stored value into the bytes builds receive. Base64 may be padded or not and wrapped in lines.
func decodeValue(v []byte, encoding string) ([]byte, error) {
	if encoding != EncodingBase64 {
		return v, nil
	}
	clean := bytes.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, v)
	out, err := base64.StdEncoding.DecodeString(string(clean))
	if err != nil {
		out, err = base64.RawStdEncoding.DecodeString(string(clean))
	}
	if err != nil {
		return nil, err
	}
	if len(out) == 0 || len(out) > MaxValueBytes {
		return nil, fmt.Errorf("the decoded value must be between 1 byte and %d KiB", MaxValueBytes>>10)
	}
	return out, nil
}

// resolve checks that the project, provider, reference, version policy and pools exist in the organization. An
// external reference is read once to prove it resolves; the value is discarded at once.
func (s *Service) resolve(ctx context.Context, orgID, projectID, providerID, reference, versionPolicy string, poolIDs []string) (target, error) {
	prov, err := s.provider(ctx, orgID, providerID)
	if errors.Is(err, ErrNotFound) {
		return target{}, &Invalid{"the secret provider does not exist in this organization"}
	}
	if err != nil {
		return target{}, err
	}
	t := target{prov: prov, ref: reference, label: reference, versionPolicy: versionPolicy, poolIDs: poolIDs}
	if prov.Type == ProviderLocal {
		sec, err := s.secret(ctx, orgID, reference)
		if errors.Is(err, ErrNotFound) {
			return target{}, &Invalid{"the provider reference must be the ID of a local secret in this organization"}
		}
		if err != nil {
			return target{}, err
		}
		if sec.ControllerOnly {
			return target{}, &Invalid{"this secret is reserved for the controller (registry credentials) and cannot be given to builds"}
		}
		switch versionPolicy {
		case "", "latest":
			t.versionPolicy = "latest"
		default:
			n, err := strconv.Atoi(versionPolicy)
			if err != nil || n < 1 || n > sec.Version {
				return target{}, &Invalid{"the version policy must be latest or an existing version number"}
			}
		}
		t.ref, t.label = sec.ID, sec.Name
	} else {
		if err := validReference(prov.Type, prov.Config, reference); err != nil {
			return target{}, err
		}
		if err := validVersion(prov.Type, versionPolicy); err != nil {
			return target{}, err
		}
		if versionPolicy == "" {
			t.versionPolicy = "latest"
		}
		if _, err := s.readExternal(ctx, orgID, prov.ID, reference, t.versionPolicy); err != nil {
			return target{}, &Invalid{"the provider could not read that reference: " + clipText(err.Error(), 300)}
		}
	}
	var exists int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM projects WHERE id = ? AND org_id = ?`), projectID, orgID).Scan(&exists); err != nil {
		return target{}, err
	}
	if exists == 0 {
		return target{}, ErrNotFound
	}
	if t.poolIDs == nil {
		t.poolIDs = []string{}
	}
	for _, p := range t.poolIDs {
		var n int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM worker_pools WHERE id = ? AND org_id = ?`), p, orgID).Scan(&n); err != nil {
			return target{}, err
		}
		if n == 0 {
			return target{}, &Invalid{"unknown worker pool " + p}
		}
	}
	return t, nil
}

func (s *Service) insertBinding(ctx context.Context, actor authz.Principal, projectID, logical string, t target, usage string, expires *time.Time, reason, grantID, requestID string) (Binding, error) {
	pools, _ := json.Marshal(t.poolIDs)
	id, now := ids.New(), s.now().UTC()
	var grant any
	if grantID != "" {
		grant = grantID
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_bindings SET status = ?, version = version + 1, reason = ?, updated_by = ?, updated_at = ? WHERE project_id = ? AND logical_id = ? AND status = ?`),
			Disabled, "replaced by "+id, actor.ID, now, projectID, logical, Active); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO secret_bindings (id, org_id, project_id, logical_id, provider_id, provider_ref, version_policy, usage, pool_ids, status, version, reason, expires_at, created_by, created_at, updated_by, updated_at, grant_id, value_encoding)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)`),
			id, actor.OrgID, projectID, logical, t.prov.ID, t.ref, t.versionPolicy, usage, string(pools), Active, reason, utc(expires), actor.ID, now, actor.ID, now, grant, t.encoding); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.bind", ResourceType: "secret-binding", ResourceID: id, Outcome: "succeeded",
			Reason: fmt.Sprintf("project %s, %s -> %s %s (%s)%s", projectID, logical, t.prov.Name, t.label, usage, encodingNote(t.encoding)), RequestID: requestID})
	})
	if err != nil {
		return Binding{}, err
	}
	return s.binding(ctx, actor.OrgID, id)
}

func encodingNote(e string) string {
	if e == "" {
		return ""
	}
	return ", decoded from " + e
}

// Disable stops a binding at once: admission refuses it and releases for running builds fail.
func (s *Service) Disable(ctx context.Context, actor authz.Principal, id, reason, requestID string) (Binding, error) {
	return s.act(ctx, actor, id, "disable", reason, requestID)
}

// RotateBinding gives the binding a new version, for example after its external value changed. Builds admitted with
// the previous version can no longer receive it, and cache scopes keyed on binding versions change (OD-17).
func (s *Service) RotateBinding(ctx context.Context, actor authz.Principal, id, reason, requestID string) (Binding, error) {
	return s.act(ctx, actor, id, "rotate", reason, requestID)
}

func (s *Service) act(ctx context.Context, actor authz.Principal, id, action, reason, requestID string) (Binding, error) {
	if !actor.IsOrgAdmin() {
		return Binding{}, ErrForbidden
	}
	if reason == "" {
		return Binding{}, &Invalid{"a reason is required"}
	}
	status, auditAction := Disabled, "secret.unbind"
	if action == "rotate" {
		status, auditAction = Active, "secret.rebind"
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_bindings SET status = ?, version = version + 1, reason = ?, updated_by = ?, updated_at = ? WHERE id = ? AND org_id = ? AND status = ?`),
			status, reason, actor.ID, s.now().UTC(), id, actor.OrgID, Active)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: auditAction, ResourceType: "secret-binding", ResourceID: id, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Binding{}, err
	}
	return s.binding(ctx, actor.OrgID, id)
}

const bindingCols = `b.id, b.project_id, b.logical_id, b.provider_id, p.type, b.provider_ref, COALESCE(s.name, ''), b.version_policy, b.usage, b.pool_ids, b.status, b.version, b.reason, b.expires_at, b.created_by, b.created_at, b.updated_at, COALESCE(b.grant_id, ''), b.value_encoding`

const bindingFrom = ` FROM secret_bindings b JOIN secret_providers p ON p.id = b.provider_id LEFT JOIN secrets s ON p.type = 'local' AND s.id = b.provider_ref`

func scanBinding(sc interface{ Scan(...any) error }) (Binding, error) {
	var b Binding
	var pools string
	var expires sql.NullTime
	if err := sc.Scan(&b.ID, &b.ProjectID, &b.LogicalID, &b.ProviderID, &b.ProviderType, &b.ProviderReference, &b.SecretName, &b.VersionPolicy, &b.Usage, &pools, &b.Status, &b.Version, &b.Reason, &expires, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt, &b.GrantID, &b.Encoding); err != nil {
		return Binding{}, err
	}
	if expires.Valid {
		b.ExpiresAt = &expires.Time
	}
	b.PoolIDs = []string{}
	_ = json.Unmarshal([]byte(pools), &b.PoolIDs)
	return b, nil
}

func (s *Service) binding(ctx context.Context, orgID, id string) (Binding, error) {
	b, err := scanBinding(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+bindingCols+bindingFrom+` WHERE b.id = ? AND b.org_id = ?`), id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	return b, err
}

func (s *Service) Binding(ctx context.Context, actor authz.Principal, id string) (Binding, error) {
	if !canReview(actor) {
		return Binding{}, ErrForbidden
	}
	return s.binding(ctx, actor.OrgID, id)
}

func (s *Service) ListBindings(ctx context.Context, actor authz.Principal, projectID string) ([]Binding, error) {
	if !canReview(actor) {
		return nil, ErrForbidden
	}
	return s.query(ctx, `b.org_id = ? AND b.project_id = ?`, actor.OrgID, projectID)
}

func (s *Service) query(ctx context.Context, where string, args ...any) ([]Binding, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+bindingCols+bindingFrom+` WHERE `+where+` ORDER BY b.logical_id, b.created_at DESC`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Binding{}
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ActiveFor returns the project's active bindings by logical ID. Bindings of other projects are never returned
// (SEC-SCR-02).
func (s *Service) ActiveFor(ctx context.Context, orgID, projectID string) (map[string]Binding, error) {
	list, err := s.query(ctx, `b.org_id = ? AND b.project_id = ? AND b.status = ?`, orgID, projectID, Active)
	if err != nil {
		return nil, err
	}
	out := map[string]Binding{}
	for _, b := range list {
		if !b.Expired(s.now()) {
			out[b.LogicalID] = b
		}
	}
	return out, nil
}

func (b Binding) Expired(now time.Time) bool { return b.ExpiresAt != nil && !now.Before(*b.ExpiresAt) }

func utc(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// Pools returns each binding's pool restriction; an empty list allows every eligible pool.
func (s *Service) Pools(ctx context.Context, orgID string, bindingIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, id := range bindingIDs {
		b, err := s.binding(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		out[id] = b.PoolIDs
	}
	return out, nil
}

// Value decrypts the value for a binding at the version a plan was admitted with. It is the only way a value leaves
// the store, and callers check the lease first.
func (s *Service) Value(ctx context.Context, orgID, bindingID string, bindingVersion int, usage string) ([]byte, error) {
	b, err := s.binding(ctx, orgID, bindingID)
	if err != nil {
		return nil, err
	}
	if b.Status != Active || b.Version != bindingVersion || !b.Allows(usage) || b.Expired(s.now()) {
		return nil, ErrUnavailable
	}
	if b.ProviderType != ProviderLocal {
		v, err := s.readExternal(ctx, orgID, b.ProviderID, b.ProviderReference, b.VersionPolicy)
		if err != nil {
			return nil, err
		}
		return decodeValue(v, b.Encoding)
	}
	version := 0
	if b.VersionPolicy == "latest" {
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT current_version FROM secrets WHERE id = ?`), b.ProviderReference).Scan(&version); err != nil {
			return nil, err
		}
	} else {
		version, _ = strconv.Atoi(b.VersionPolicy)
	}
	var keyID string
	var nonce, ct []byte
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT key_id, nonce, ciphertext FROM secret_versions WHERE secret_id = ? AND version = ?`), b.ProviderReference, version).Scan(&keyID, &nonce, &ct); err != nil {
		return nil, err
	}
	if keyID != s.keyID {
		return nil, errors.New("secrets: the value was encrypted with a different store key")
	}
	return s.aead.Open(nil, nonce, ct, aad(b.ProviderReference, version))
}

func isUnique(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}
