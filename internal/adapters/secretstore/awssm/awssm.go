// Package awssm reads secrets from AWS Secrets Manager with the AWS SDK for Go v2. Identity-based credentials come
// first: the controller's workload identity, optionally assuming a role with an external ID. Stored access keys are
// accepted but reported as long-lived.
package awssm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"samuori.invalid/samuori/internal/secrets"
)

type Client struct{}

func New() *Client { return &Client{} }

// AWSConfig resolves credentials for a provider configuration. It is shared with other AWS adapters.
func AWSConfig(ctx context.Context, region, method, roleARN, externalID, endpoint string, credential []byte) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if method == "access-key" {
		id, secret, ok := strings.Cut(strings.TrimSpace(string(credential)), ":")
		if !ok || id == "" || secret == "" {
			return aws.Config{}, errors.New("the access key must be ACCESS_KEY_ID:SECRET_ACCESS_KEY")
		}
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(id, secret, "")))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("loading the AWS identity: %w", err)
	}
	if endpoint != "" {
		cfg.BaseEndpoint = aws.String(endpoint)
	}
	if method == "assume-role" {
		provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), roleARN, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = "samuori-controller"
			o.Duration = 15 * time.Minute
			if externalID != "" {
				o.ExternalID = aws.String(externalID)
			}
		})
		cfg.Credentials = aws.NewCredentialsCache(provider)
	}
	return cfg, nil
}

func (c *Client) config(ctx context.Context, pc secrets.ExternalConfig, credential []byte) (aws.Config, error) {
	return AWSConfig(ctx, pc.Region, pc.AuthMethod, pc.RoleARN, pc.ExternalID, pc.Endpoint, credential)
}

var stage = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func (c *Client) Read(ctx context.Context, _ string, pc secrets.ExternalConfig, credential []byte, reference, version string) ([]byte, error) {
	cfg, err := c.config(ctx, pc, credential)
	if err != nil {
		return nil, err
	}
	id, key, _ := strings.Cut(reference, "#")
	in := &secretsmanager.GetSecretValueInput{SecretId: aws.String(id)}
	switch {
	case version == "":
	case stage.MatchString(version):
		in.VersionStage = aws.String(version)
	default:
		in.VersionId = aws.String(version)
	}
	out, err := secretsmanager.NewFromConfig(cfg).GetSecretValue(ctx, in)
	if err != nil {
		return nil, Describe(err)
	}
	value := out.SecretBinary
	if out.SecretString != nil {
		value = []byte(*out.SecretString)
	}
	if key == "" {
		return value, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object, so #%s cannot be selected", id, key)
	}
	raw, ok := fields[key]
	if !ok {
		return nil, fmt.Errorf("%s has no key %s", id, key)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []byte(s), nil
	}
	return raw, nil
}

func (c *Client) Check(ctx context.Context, _ string, pc secrets.ExternalConfig, credential []byte) (string, error) {
	cfg, err := c.config(ctx, pc, credential)
	if err != nil {
		return "", err
	}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("identity: %w", Describe(err))
	}
	return fmt.Sprintf("authenticated as %s in account %s, region %s", aws.ToString(id.Arn), aws.ToString(id.Account), pc.Region), nil
}

// Describe turns an AWS error into a short cause without request IDs or credentials.
func Describe(err error) error {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "ResourceNotFoundException":
			return errors.New("not found")
		case "AccessDeniedException", "AccessDenied":
			return errors.New("permission denied")
		}
		return fmt.Errorf("%s: %s", api.ErrorCode(), api.ErrorMessage())
	}
	return err
}
