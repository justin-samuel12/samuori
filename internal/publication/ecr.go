package publication

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// ECRConfig is how an ECR profile obtains AWS credentials. Identity-based methods come first; a stored access key is
// accepted but reported as long-lived (docs/architecture/registry-auth.md).
type ECRConfig struct {
	AuthMethod string `json:"authMethod"`
	RoleARN    string `json:"roleArn,omitempty"`
	ExternalID string `json:"externalId,omitempty"`
	// APIEndpoint overrides the ECR and STS API endpoint, for VPC endpoints or a local emulator. Registry pushes always
	// go to the registry's own ECR endpoint.
	APIEndpoint string `json:"apiEndpoint,omitempty"`
}

// ECRConnection is everything an ECR adapter needs for one profile. Credential is a controller-only secret value.
type ECRConnection struct {
	CacheKey          string
	AccountID, Region string
	Config            ECRConfig
	Credential        []byte
}

// TestStep is one step of an ECR connection test, so a failure names where it happened (INT-REG-02).
type TestStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Repository is an ECR repository.
type Repository struct {
	Name          string    `json:"name"`
	URI           string    `json:"uri"`
	ImmutableTags bool      `json:"immutableTags"`
	ScanOnPush    bool      `json:"scanOnPush"`
	CreatedAt     time.Time `json:"createdAt"`
}

// ECR obtains registry tokens and manages repositories. Tokens stay in controller memory.
type ECR interface {
	Token(ctx context.Context, c ECRConnection) (username, password string, err error)
	Test(ctx context.Context, c ECRConnection, repositories []string) ([]TestStep, error)
	Repositories(ctx context.Context, c ECRConnection) ([]Repository, error)
	CreateRepository(ctx context.Context, c ECRConnection, name string, immutableTags, scanOnPush bool) (Repository, error)
}

// UseECR enables ECR registries.
func (s *Service) UseECR(e ECR) { s.ecr = e }

var (
	accountID   = regexp.MustCompile(`^[0-9]{12}$`)
	awsRegion   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	ecrEndpoint = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr(-fips)?\.[a-z0-9-]+\.amazonaws\.com(\.cn)?$`)
)

// ecrRegistry checks an ECR registry and derives its endpoint, which must be the account's own ECR endpoint: the
// registry token is an AWS credential and is never sent anywhere else.
func ecrRegistry(r *Registry) error {
	if !accountID.MatchString(r.AccountID) || !awsRegion.MatchString(r.Region) {
		return &Invalid{"an ECR registry needs a 12-digit account ID and a region such as eu-west-1"}
	}
	if r.Endpoint == "" {
		r.Endpoint = r.AccountID + ".dkr.ecr." + r.Region + ".amazonaws.com"
	}
	if !ecrEndpoint.MatchString(r.Endpoint) || r.Endpoint[:12] != r.AccountID {
		return &Invalid{"the endpoint must be the account's ECR registry, such as 123456789012.dkr.ecr.eu-west-1.amazonaws.com"}
	}
	r.PlainHTTP, r.AllowPrivate = false, false
	return nil
}

func (c *ECRConfig) validate(hasSecret bool) error {
	if hasSecret && c.AuthMethod != "access-key" {
		return &Invalid{"only the access-key method takes a stored credential"}
	}
	switch c.AuthMethod {
	case "workload-identity":
	case "assume-role":
		if !regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$`).MatchString(c.RoleARN) {
			return &Invalid{"assume-role needs a role ARN such as arn:aws:iam::123456789012:role/samuori-push"}
		}
	case "access-key":
		if !hasSecret {
			return &Invalid{"access-key needs a secret holding ACCESS_KEY_ID:SECRET_ACCESS_KEY"}
		}
	case "roles-anywhere":
		return &Invalid{"IAM Roles Anywhere is not available yet: it cannot be verified without a live AWS account"}
	default:
		return &Invalid{"the authentication method must be workload-identity, assume-role or access-key"}
	}
	if len(c.ExternalID) > 1224 {
		return &Invalid{"the external ID is too long"}
	}
	if c.APIEndpoint != "" {
		u, err := url.Parse(c.APIEndpoint)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname()))) {
			return &Invalid{"the API endpoint must be an https URL (http only on loopback, for testing)"}
		}
	}
	return nil
}

// ecrConnection resolves a profile's credential just in time.
func (s *Service) ecrConnection(ctx context.Context, orgID string, r Registry, p Profile) (ECRConnection, error) {
	if s.ecr == nil {
		return ECRConnection{}, errors.New("ECR is not available on this controller")
	}
	c := ECRConnection{AccountID: r.AccountID, Region: r.Region, Config: p.ECR}
	cfg, _ := json.Marshal(p.ECR)
	cfgSum := sha256.Sum256(cfg)
	key := p.ID + "|" + r.AccountID + "|" + r.Region + "|" + hex.EncodeToString(cfgSum[:8])
	if p.SecretID != "" {
		v, err := s.secrets.ControllerValue(ctx, orgID, p.SecretID)
		if err != nil {
			return ECRConnection{}, errors.New("the profile's credential is unavailable")
		}
		c.Credential = v
		sum := sha256.Sum256(v)
		key += "|" + hex.EncodeToString(sum[:8])
	}
	c.CacheKey = key
	return c, nil
}

// ECRRepositories lists the repositories a profile can see in its registry.
func (s *Service) ECRRepositories(ctx context.Context, actor authz.Principal, registryID, profileID string) ([]Repository, error) {
	r, p, err := s.ecrPair(ctx, actor, registryID, profileID)
	if err != nil {
		return nil, err
	}
	c, err := s.ecrConnection(ctx, actor.OrgID, r, p)
	if err != nil {
		return nil, err
	}
	return s.ecr.Repositories(ctx, c)
}

// CreateECRRepository creates a repository; it is administrator-only and audited (D03). Give the profile a role that
// may only create repositories.
func (s *Service) CreateECRRepository(ctx context.Context, actor authz.Principal, registryID, profileID, name string, immutable, scanOnPush bool, requestID string) (Repository, error) {
	r, p, err := s.ecrPair(ctx, actor, registryID, profileID)
	if err != nil {
		return Repository{}, err
	}
	if !repository.MatchString(name) || len(name) > 256 {
		return Repository{}, &Invalid{"the repository must be a lower-case path such as team/app"}
	}
	c, err := s.ecrConnection(ctx, actor.OrgID, r, p)
	if err != nil {
		return Repository{}, err
	}
	repo, err := s.ecr.CreateRepository(ctx, c, name, immutable, scanOnPush)
	outcome, reason := "succeeded", fmt.Sprintf("%s (immutable tags %t, scan on push %t)", name, immutable, scanOnPush)
	if err != nil {
		outcome, reason = "failed", name+": "+clipText(err.Error(), 300)
	}
	if aerr := s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "ecr-repository.create", ResourceType: "registry", ResourceID: r.ID, Outcome: outcome, Reason: reason, RequestID: requestID})
	}); aerr != nil && err == nil {
		err = aerr
	}
	return repo, err
}

func (s *Service) ecrPair(ctx context.Context, actor authz.Principal, registryID, profileID string) (Registry, Profile, error) {
	if !actor.IsOrgAdmin() {
		return Registry{}, Profile{}, ErrForbidden
	}
	r, err := s.registry(ctx, actor.OrgID, registryID)
	if err != nil {
		return Registry{}, Profile{}, err
	}
	p, err := s.profile(ctx, actor.OrgID, profileID)
	if err != nil {
		return Registry{}, Profile{}, err
	}
	if r.Kind != "ecr" || p.Type != "ecr" || p.RegistryID != r.ID {
		return Registry{}, Profile{}, &Invalid{"choose an ECR profile of this ECR registry"}
	}
	return r, p, nil
}
