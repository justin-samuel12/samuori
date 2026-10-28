// Package ecr implements publication.ECR with the AWS SDK for Go v2: registry tokens from GetAuthorizationToken,
// cached in memory and renewed well before they expire, a step-by-step connection test, and repository listing and
// creation.
package ecr

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"samuori.invalid/samuori/internal/adapters/secretstore/awssm"
	"samuori.invalid/samuori/internal/publication"
)

// RenewBefore is how long before expiry a cached token is replaced. ECR tokens last twelve hours.
const RenewBefore = time.Hour

type Client struct {
	Now func() time.Time

	mu      sync.Mutex
	tokens  map[string]token
	fetches int
}

type token struct {
	user, pass string
	expires    time.Time
}

func New() *Client { return &Client{Now: time.Now, tokens: map[string]token{}} }

func (c *Client) config(ctx context.Context, conn publication.ECRConnection) (aws.Config, error) {
	cfg := conn.Config
	return awssm.AWSConfig(ctx, conn.Region, cfg.AuthMethod, cfg.RoleARN, cfg.ExternalID, cfg.APIEndpoint, conn.Credential)
}

func (c *Client) Token(ctx context.Context, conn publication.ECRConnection) (string, string, error) {
	c.mu.Lock()
	t, ok := c.tokens[conn.CacheKey]
	c.mu.Unlock()
	if ok && c.Now().Add(RenewBefore).Before(t.expires) {
		return t.user, t.pass, nil
	}
	cfg, err := c.config(ctx, conn)
	if err != nil {
		return "", "", err
	}
	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	c.mu.Lock()
	c.fetches++
	c.mu.Unlock()
	if err != nil {
		return "", "", awssm.Describe(err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", "", errors.New("ECR returned no authorization token")
	}
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(out.AuthorizationData[0].AuthorizationToken))
	if err != nil {
		return "", "", errors.New("the ECR authorization token is not base64")
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok || user == "" || pass == "" {
		return "", "", errors.New("the ECR authorization token is malformed")
	}
	t = token{user, pass, c.Now().Add(12 * time.Hour)}
	if e := out.AuthorizationData[0].ExpiresAt; e != nil {
		t.expires = *e
	}
	c.mu.Lock()
	c.tokens[conn.CacheKey] = t
	c.mu.Unlock()
	return user, pass, nil
}

func (c *Client) Test(ctx context.Context, conn publication.ECRConnection, repositories []string) ([]publication.TestStep, error) {
	var steps []publication.TestStep
	add := func(step, status, detail string) {
		steps = append(steps, publication.TestStep{Step: step, Status: status, Detail: detail})
	}
	cfg, err := c.config(ctx, conn)
	if err != nil {
		add("identity", "failed", err.Error())
		return steps, nil
	}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		label := "identity"
		if conn.Config.AuthMethod == "assume-role" {
			label = "role assumption"
		}
		add(label, "failed", awssm.Describe(err).Error())
		return steps, nil
	}
	add("identity", "ok", fmt.Sprintf("%s in account %s", aws.ToString(id.Arn), aws.ToString(id.Account)))
	if aws.ToString(id.Account) == conn.AccountID {
		add("account", "ok", "the identity belongs to the registry's account "+conn.AccountID)
	} else {
		add("account", "info", fmt.Sprintf("the identity is in account %s and the registry in %s; repository policies must allow cross-account access", aws.ToString(id.Account), conn.AccountID))
	}
	client := ecr.NewFromConfig(cfg)
	if _, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{}); err != nil {
		add("registry token in "+conn.Region, "failed", awssm.Describe(err).Error())
		return steps, nil
	}
	add("registry token in "+conn.Region, "ok", "")
	for _, repo := range repositories {
		if strings.ContainsAny(repo, "*?[") {
			add("repository "+repo, "info", "a pattern; individual repositories are checked when used")
			continue
		}
		_, err := client.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RegistryId: aws.String(conn.AccountID), RepositoryNames: []string{repo}})
		if err != nil {
			add("repository "+repo, "failed", awssm.Describe(err).Error())
			continue
		}
		add("repository "+repo, "ok", "visible; push permission is proven only by a publication")
	}
	return steps, nil
}

func (c *Client) Repositories(ctx context.Context, conn publication.ECRConnection) ([]publication.Repository, error) {
	cfg, err := c.config(ctx, conn)
	if err != nil {
		return nil, err
	}
	var out []publication.Repository
	p := ecr.NewDescribeRepositoriesPaginator(ecr.NewFromConfig(cfg), &ecr.DescribeRepositoriesInput{RegistryId: aws.String(conn.AccountID)})
	for p.HasMorePages() && len(out) < 5000 {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, awssm.Describe(err)
		}
		for _, r := range page.Repositories {
			out = append(out, repository(r))
		}
	}
	return out, nil
}

func (c *Client) CreateRepository(ctx context.Context, conn publication.ECRConnection, name string, immutable, scanOnPush bool) (publication.Repository, error) {
	cfg, err := c.config(ctx, conn)
	if err != nil {
		return publication.Repository{}, err
	}
	mutability := types.ImageTagMutabilityMutable
	if immutable {
		mutability = types.ImageTagMutabilityImmutable
	}
	out, err := ecr.NewFromConfig(cfg).CreateRepository(ctx, &ecr.CreateRepositoryInput{RepositoryName: aws.String(name), RegistryId: aws.String(conn.AccountID),
		ImageTagMutability: mutability, ImageScanningConfiguration: &types.ImageScanningConfiguration{ScanOnPush: scanOnPush}})
	if err != nil {
		return publication.Repository{}, awssm.Describe(err)
	}
	return repository(*out.Repository), nil
}

func repository(r types.Repository) publication.Repository {
	out := publication.Repository{Name: aws.ToString(r.RepositoryName), URI: aws.ToString(r.RepositoryUri), ImmutableTags: r.ImageTagMutability == types.ImageTagMutabilityImmutable}
	if r.ImageScanningConfiguration != nil {
		out.ScanOnPush = r.ImageScanningConfiguration.ScanOnPush
	}
	if r.CreatedAt != nil {
		out.CreatedAt = *r.CreatedAt
	}
	return out
}
