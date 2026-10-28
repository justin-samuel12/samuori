package imagesources

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

// TokenTTL bounds how long an issued mirror token stays valid after access is revoked.
const TokenTTL = 5 * time.Minute

// Issuer is the "iss" claim mirrors are configured to trust.
const Issuer = "samuori"

// Service is the "aud" claim: the mirror's token service name.
const Service = "samuori-mirror"

// RotateKey generates a new token-signing key and certificate, retiring the previous key. Mirrors must trust the new
// certificate (TrustBundle) before tokens signed with it are accepted; keep the old one in the bundle until then.
func (s *Store) RotateKey(ctx context.Context, actor authz.Principal, requestID string) (string, error) {
	if !actor.IsOrgAdmin() {
		return "", ErrForbidden
	}
	var orgs int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM organizations WHERE id = ?`), actor.OrgID).Scan(&orgs); err != nil || orgs == 0 {
		return "", errors.Join(ErrNotFound, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "samuori mirror token signer " + actor.OrgID},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	id := ids.New()
	secretID, err := s.secrets.CreateControllerSecret(ctx, actor, "mirror-token-key-"+strings.ToLower(id[16:]), "mirror token signing key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}), requestID)
	if err != nil {
		return "", err
	}
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE mirror_token_keys SET state = 'retired', retired_at = ? WHERE org_id = ? AND state = 'active'`), now, actor.OrgID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO mirror_token_keys (id, org_id, certificate, secret_id, state, created_by, created_at) VALUES (?, ?, ?, ?, 'active', ?, ?)`),
			id, actor.OrgID, cert, secretID, actor.ID, now); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "mirror-token-key.rotate", ResourceType: "mirror-token-key", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
	return cert, err
}

// TrustBundle returns the certificates mirrors should trust: the active key's and, for a rotation window, retired
// keys from the last day.
func (s *Store) TrustBundle(ctx context.Context, orgID string) (string, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT certificate FROM mirror_token_keys WHERE org_id = ? AND (state = 'active' OR retired_at > ?) ORDER BY created_at DESC`), orgID, s.now().Add(-24*time.Hour).UTC())
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return "", err
		}
		b.WriteString(c)
	}
	return b.String(), rows.Err()
}

func (s *Store) signer(ctx context.Context, orgID string) (*ecdsa.PrivateKey, []byte, error) {
	var cert, secretID string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT certificate, secret_id FROM mirror_token_keys WHERE org_id = ? AND state = 'active'`), orgID).Scan(&cert, &secretID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, errors.New("no mirror token key; an administrator must create one")
	}
	if err != nil {
		return nil, nil, err
	}
	raw, err := s.secrets.ControllerValue(ctx, orgID, secretID)
	if err != nil {
		return nil, nil, err
	}
	kb, _ := pem.Decode(raw)
	cb, _ := pem.Decode([]byte(cert))
	if kb == nil || cb == nil {
		return nil, nil, errors.New("the mirror token key is unreadable")
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("the mirror token key is not ECDSA")
	}
	return key, cb.Bytes, nil
}

// Grant is one repository pull the token allows.
type Grant struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// Request is a mirror token request for one build attempt; Scopes are Distribution scopes such as
// repository:payments/base:pull.
type Request struct {
	OrgID, TeamID, Subject string
	Mirror                 string
	Scopes                 []string
}

// Token is an issued mirror token with what it grants and what was refused.
type Token struct {
	Value     string
	ExpiresIn int
	IssuedAt  time.Time
	Granted   []Grant
	Denied    []string
}

// Issue signs a token granting pull on the requested repositories that an active source for this mirror allows the
// team. Everything else, including catalog, push and other mirrors, is left out (SEC-REG-04, SEC-REG-07).
func (s *Store) Issue(ctx context.Context, r Request) (Token, error) {
	sources, err := s.query(ctx, `org_id = ? AND mirror = ? AND status = 'active'`, r.OrgID, strings.ToLower(r.Mirror))
	if err != nil {
		return Token{}, err
	}
	t := Token{IssuedAt: s.now().UTC(), ExpiresIn: int(TokenTTL / time.Second), Granted: []Grant{}}
	for _, scope := range r.Scopes {
		parts := strings.Split(scope, ":")
		ok := len(parts) == 3 && parts[0] == "repository" && parts[2] == "pull" && pattern.MatchString(parts[1])
		if ok {
			ok = false
			for _, src := range sources {
				if src.Allows(r.TeamID, parts[1]) {
					ok = true
					break
				}
			}
		}
		if !ok {
			t.Denied = append(t.Denied, scope)
			continue
		}
		t.Granted = append(t.Granted, Grant{Type: "repository", Name: parts[1], Actions: []string{"pull"}})
	}
	key, der, err := s.signer(ctx, r.OrgID)
	if err != nil {
		return Token{}, err
	}
	jti := make([]byte, 12)
	_, _ = rand.Read(jti)
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("x5c", []string{base64.StdEncoding.EncodeToString(der)})
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, opts)
	if err != nil {
		return Token{}, err
	}
	claims := jwt.Claims{Issuer: Issuer, Subject: r.Subject, Audience: jwt.Audience{Service}, IssuedAt: jwt.NewNumericDate(t.IssuedAt),
		NotBefore: jwt.NewNumericDate(t.IssuedAt.Add(-30 * time.Second)), Expiry: jwt.NewNumericDate(t.IssuedAt.Add(TokenTTL)), ID: base64.RawURLEncoding.EncodeToString(jti)}
	t.Value, err = jwt.Signed(signer).Claims(claims).Claims(map[string]any{"access": t.Granted}).Serialize()
	return t, err
}

// MirrorConfig renders a CNCF Distribution configuration for a source's mirror: a pull-through cache of the upstream
// that trusts only Samuori's tokens. The upstream password is never written here; the operator supplies it as
// REGISTRY_PROXY_PASSWORD from the pull profile's secret.
func (s *Store) MirrorConfig(ctx context.Context, actor authz.Principal, id string) (string, error) {
	if !actor.IsOrgAdmin() {
		return "", ErrForbidden
	}
	src, err := s.Get(ctx, actor.OrgID, id)
	if err != nil {
		return "", err
	}
	user := ""
	if src.PullProfileID != "" {
		_ = s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT username FROM registry_profiles WHERE id = ?`), src.PullProfileID).Scan(&user)
	}
	return fmt.Sprintf(`version: 0.1
storage:
  filesystem:
    rootdirectory: /var/lib/samuori-mirror/%[1]s
http:
  addr: ":5000"
auth:
  token:
    realm: "https://samuori.invalid/mirror-tokens-come-from-workers"
    service: %[2]s
    issuer: %[3]s
    rootcertbundle: /etc/samuori-mirror/trust.pem
proxy:
  remoteurl: https://%[4]s
  username: %[5]q
`, src.Name, Service, Issuer, src.Upstream, user), nil
}
