package secrets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

// External provider types (ADR 0015). Vault is reached through the same KV v2 API as OpenBao and is never bundled.
const (
	ProviderOpenBao = "openbao"
	ProviderVault   = "vault-compat"
	ProviderAWS     = "aws-secrets-manager"
)

// ExternalConfig is an external provider's connection. It never holds a credential: the token, AppRole secret ID or
// access key is a controller-only local secret named by the provider.
type ExternalConfig struct {
	Address    string `json:"address,omitempty"`
	Mount      string `json:"mount,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	CACertPEM  string `json:"caCertPem,omitempty"`
	AuthMethod string `json:"authMethod"`
	RoleID     string `json:"roleId,omitempty"`
	Region     string `json:"region,omitempty"`
	RoleARN    string `json:"roleArn,omitempty"`
	ExternalID string `json:"externalId,omitempty"`
	// Endpoint overrides the AWS endpoint, for VPC endpoints or a local emulator.
	Endpoint string `json:"endpoint,omitempty"`
	// AllowedPrefixes, when set, are the only references that may be bound to projects.
	AllowedPrefixes []string `json:"allowedPrefixes,omitempty"`
}

// External reads values from an external store just in time; values are never cached or written to disk.
type External interface {
	Read(ctx context.Context, typ string, cfg ExternalConfig, credential []byte, reference, version string) ([]byte, error)
	// Check proves connectivity and authentication and describes the identity used, without reading any secret.
	Check(ctx context.Context, typ string, cfg ExternalConfig, credential []byte) (string, error)
}

// isKeySet reports whether a reference names a whole KV v2 secret, whose keys are bound one at a time.
func isKeySet(typ, ref string) bool {
	return (typ == ProviderOpenBao || typ == ProviderVault) && !strings.Contains(ref, "#")
}

// keyNames reads a KV v2 secret once and returns its key names. The values are discarded at once.
func (s *Service) keyNames(ctx context.Context, orgID, providerID, ref, version string) ([]string, error) {
	raw, err := s.readExternal(ctx, orgID, providerID, ref, version)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New("the secret is not a set of keys")
	}
	names := make([]string, 0, len(fields))
	for k, v := range fields {
		clear(v)
		if keyName.MatchString(k) {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("the secret has no keys Samuori can use")
	}
	slices.Sort(names)
	return names, nil
}

// UseExternal registers the adapter that serves an external provider type.
func (s *Service) UseExternal(typ string, e External) {
	if s.external == nil {
		s.external = map[string]External{}
	}
	s.external[typ] = e
}

var (
	kvPath      = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*(#[A-Za-z0-9._-]{1,128})?$`)
	keyName     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	awsSecretID = regexp.MustCompile(`^(arn:aws[a-z-]*:secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:)?[A-Za-z0-9/_+=.@-]{1,512}(#[A-Za-z0-9._-]{1,128})?$`)
	awsVersion  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	mountPath   = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
)

// splitMount separates the KV v2 mount an OpenBao or Vault reference names, as in docker:payments/db#password. A
// reference without one uses the store's mount.
func splitMount(typ, ref string) (mount, rest string) {
	if typ != ProviderOpenBao && typ != ProviderVault {
		return "", ref
	}
	if m, r, ok := strings.Cut(ref, ":"); ok {
		return m, r
	}
	return "", ref
}

func validMount(m string) bool {
	return mountPath.MatchString(m) && !slices.ContainsFunc(strings.Split(m, "/"), func(seg string) bool { return seg == "." || seg == ".." })
}

// validReference checks a reference's shape for the provider type and the provider's allowed prefixes.
func validReference(typ string, cfg ExternalConfig, ref string) error {
	mount, ref := splitMount(typ, ref)
	if mount != "" && !validMount(mount) {
		return &Invalid{"the KV v2 mount must be a mount path such as secret"}
	}
	switch typ {
	case ProviderOpenBao, ProviderVault:
		path, _, _ := strings.Cut(ref, "#")
		if !kvPath.MatchString(ref) || slices.ContainsFunc(strings.Split(path, "/"), func(seg string) bool { return seg == "." || seg == ".." }) {
			return &Invalid{"the reference must be a KV v2 path, such as payments/db for its set of keys, or a path and key such as payments/db#password"}
		}
	case ProviderAWS:
		if !awsSecretID.MatchString(ref) {
			return &Invalid{"the reference must be a secret name or ARN, optionally with #key for a JSON secret"}
		}
	}
	if len(cfg.AllowedPrefixes) > 0 && !slices.ContainsFunc(cfg.AllowedPrefixes, func(p string) bool { return strings.HasPrefix(ref, p) }) {
		return &Invalid{"this provider does not allow binding that reference"}
	}
	return nil
}

func validVersion(typ, v string) error {
	if v == "" || v == "latest" {
		return nil
	}
	switch typ {
	case ProviderOpenBao, ProviderVault:
		if n, err := fmt.Sscanf(v, "%d", new(int)); n != 1 || err != nil || strings.HasPrefix(v, "0") || strings.ContainsAny(v, "-+ ") {
			return &Invalid{"the version policy must be latest or a KV version number"}
		}
	case ProviderAWS:
		if !awsVersion.MatchString(v) {
			return &Invalid{"the version policy must be latest, a version ID or a staging label"}
		}
	}
	return nil
}

func (c *ExternalConfig) validate(typ string, hasCredential bool) error {
	switch typ {
	case ProviderOpenBao, ProviderVault:
		u, err := url.Parse(c.Address)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname()))) {
			return &Invalid{"the address must be an https URL (http only on loopback, for testing)"}
		}
		if c.Mount == "" {
			c.Mount = "secret"
		}
		if !validMount(c.Mount) {
			return &Invalid{"the mount must be a KV v2 mount path such as secret"}
		}
		switch c.AuthMethod {
		case "token":
			if !hasCredential {
				return &Invalid{"a token is required"}
			}
		case "approle":
			c.RoleID = strings.TrimSpace(c.RoleID)
			// Without a secret ID the role must be created with bind_secret_id=false and bound to the controllers'
			// addresses (docs/architecture/secrets.md#external-providers-wp27).
			if c.RoleID == "" {
				return &Invalid{"AppRole needs a role ID; the secret ID is optional"}
			}
		default:
			return &Invalid{"the authentication method must be token or approle"}
		}
	case ProviderAWS:
		if !regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`).MatchString(c.Region) {
			return &Invalid{"a region such as eu-west-1 is required"}
		}
		switch c.AuthMethod {
		case "workload-identity":
			if hasCredential {
				return &Invalid{"workload identity takes no stored credential"}
			}
		case "assume-role":
			if !strings.HasPrefix(c.RoleARN, "arn:aws") {
				return &Invalid{"assume-role needs a role ARN"}
			}
		case "access-key":
			if !hasCredential {
				return &Invalid{"access-key needs the key as ACCESS_KEY_ID:SECRET_ACCESS_KEY"}
			}
		default:
			return &Invalid{"the authentication method must be workload-identity, assume-role or access-key"}
		}
		if c.Endpoint != "" {
			if u, err := url.Parse(c.Endpoint); err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname()))) {
				return &Invalid{"the endpoint must be an https URL (http only on loopback, for testing)"}
			}
		}
	default:
		return &Invalid{"the type must be openbao, vault-compat or aws-secrets-manager"}
	}
	return nil
}

func loopbackHost(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

// CreateProvider adds an external provider. The credential, if any, is stored as a controller-only local secret and
// never returned.
func (s *Service) CreateProvider(ctx context.Context, actor authz.Principal, name, typ string, cfg ExternalConfig, credential []byte, requestID string) (Provider, error) {
	if !actor.IsOrgAdmin() {
		return Provider{}, ErrForbidden
	}
	if !secretName.MatchString(name) || len(name) > 63 {
		return Provider{}, &Invalid{"the name must be lower-case letters, digits, '.', '_' or '-'"}
	}
	if err := cfg.validate(typ, len(credential) > 0); err != nil {
		return Provider{}, err
	}
	raw, _ := json.Marshal(cfg)
	credID := ""
	if len(credential) > 0 {
		id, err := s.CreateControllerSecret(ctx, actor, "secret-provider-"+name, "credential of secret provider "+name, credential, requestID)
		if err != nil {
			return Provider{}, err
		}
		credID = id
	}
	p := Provider{ID: ids.New(), Name: name, Type: typ, Config: cfg, CredentialSecretID: credID, CreatedAt: s.now().UTC()}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO secret_providers (id, org_id, name, type, created_at, config, credential_secret_id) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			p.ID, actor.OrgID, name, typ, p.CreatedAt, string(raw), nullString(credID)); err != nil {
			if isUnique(err) {
				return &Invalid{"a provider with that name exists"}
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret-provider.create", ResourceType: "secret-provider", ResourceID: p.ID, Outcome: "succeeded", Reason: typ + " " + name + " (" + cfg.AuthMethod + ")", RequestID: requestID})
	})
	return p, err
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// ProviderCheck is the outcome of a provider connection test.
type ProviderCheck struct {
	Status   string    `json:"status"`
	TestedAt time.Time `json:"testedAt"`
	Detail   string    `json:"detail,omitempty"`
}

func (s *Service) checkExternal(ctx context.Context, orgID string, p Provider) (string, error) {
	e, cred, err := s.externalFor(ctx, orgID, p)
	if err != nil {
		return "", err
	}
	return e.Check(ctx, p.Type, p.Config, cred)
}

// Connection returns an external provider and its credential for the controller's own use, such as signing with a
// key held by the provider. Callers must never pass the credential to workers or responses.
func (s *Service) Connection(ctx context.Context, orgID, providerID string) (Provider, []byte, error) {
	p, err := s.provider(ctx, orgID, providerID)
	if err != nil {
		return Provider{}, nil, err
	}
	if p.Type == ProviderLocal {
		return Provider{}, nil, &Invalid{"the local store holds no signing keys; choose an OpenBao, Vault or AWS provider"}
	}
	var cred []byte
	if p.CredentialSecretID != "" {
		if cred, err = s.ControllerValue(ctx, orgID, p.CredentialSecretID); err != nil {
			return Provider{}, nil, errors.New("the provider credential is unavailable")
		}
	}
	return p, cred, nil
}

func (s *Service) externalFor(ctx context.Context, orgID string, p Provider) (External, []byte, error) {
	e := s.external[p.Type]
	if e == nil {
		return nil, nil, fmt.Errorf("no adapter for %s is configured", p.Type)
	}
	var cred []byte
	if p.CredentialSecretID != "" {
		v, err := s.ControllerValue(ctx, orgID, p.CredentialSecretID)
		if err != nil {
			return nil, nil, errors.New("the provider credential is unavailable")
		}
		cred = v
	}
	return e, cred, nil
}

// readExternal fetches a binding's value from its external provider.
func (s *Service) readExternal(ctx context.Context, orgID, providerID, reference, version string) ([]byte, error) {
	p, err := s.provider(ctx, orgID, providerID)
	if err != nil {
		return nil, err
	}
	e, cred, err := s.externalFor(ctx, orgID, p)
	if err != nil {
		return nil, err
	}
	if version == "latest" {
		version = ""
	}
	cfg := p.Config
	if mount, rest := splitMount(p.Type, reference); mount != "" {
		cfg.Mount, reference = mount, rest
	}
	v, err := e.Read(ctx, p.Type, cfg, cred, reference, version)
	if err != nil {
		return nil, err
	}
	if len(v) == 0 || len(v) > MaxValueBytes {
		return nil, fmt.Errorf("the value must be between 1 byte and %d KiB", MaxValueBytes>>10)
	}
	return v, nil
}

func (s *Service) recordCheck(ctx context.Context, id string, c ProviderCheck) {
	raw, _ := json.Marshal(c)
	_, _ = s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE secret_providers SET last_check = ? WHERE id = ?`), string(raw), id)
}
