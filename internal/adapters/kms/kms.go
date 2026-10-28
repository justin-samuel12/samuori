// Package kms implements signing.Remote: image signing keys held by OpenBao Transit, a Vault-compatible Transit engine
// or AWS KMS. It connects through the organization's secret providers, so connection details and credentials are
// managed in one place and private keys never leave the key service.
package kms

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"samuori.invalid/samuori/internal/adapters/secretstore/awssm"
	"samuori.invalid/samuori/internal/adapters/secretstore/openbao"
	"samuori.invalid/samuori/internal/secrets"
)

type Remote struct {
	Secrets *secrets.Service
	Bao     *openbao.Client
}

func (r *Remote) Describe(ctx context.Context, orgID, providerID, ref string) (string, string, string, error) {
	p, cred, err := r.Secrets.Connection(ctx, orgID, providerID)
	if err != nil {
		return "", "", "", err
	}
	switch p.Type {
	case secrets.ProviderOpenBao, secrets.ProviderVault:
		typ, version, pub, err := r.Bao.TransitKey(ctx, p.Config, cred, ref)
		if err != nil {
			return "", "", "", err
		}
		if typ != "ecdsa-p256" {
			return "", "", "", fmt.Errorf("the Transit key type is %s; ecdsa-p256 is required", typ)
		}
		return pub, p.Type + "-transit", fmt.Sprintf("%s#%d", ref, version), nil
	case secrets.ProviderAWS:
		client, err := r.kms(ctx, p, cred)
		if err != nil {
			return "", "", "", err
		}
		out, err := client.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(ref)})
		if err != nil {
			return "", "", "", awssm.Describe(err)
		}
		// Some KMS implementations report only the deprecated CustomerMasterKeySpec; the curve is checked again when
		// the public key is parsed.
		spec := string(out.KeySpec)
		if spec == "" {
			spec = string(out.CustomerMasterKeySpec)
		}
		if spec != string(types.KeySpecEccNistP256) || out.KeyUsage != types.KeyUsageTypeSignVerify {
			return "", "", "", fmt.Errorf("the KMS key is %s for %s; an ECC_NIST_P256 signing key is required", spec, out.KeyUsage)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: out.PublicKey})), "aws-kms", aws.ToString(out.KeyId), nil
	}
	return "", "", "", errors.New("this provider type holds no signing keys")
}

func (r *Remote) Sign(ctx context.Context, orgID, providerID, ref string, digest []byte) ([]byte, error) {
	p, cred, err := r.Secrets.Connection(ctx, orgID, providerID)
	if err != nil {
		return nil, err
	}
	switch p.Type {
	case secrets.ProviderOpenBao, secrets.ProviderVault:
		return r.Bao.TransitSign(ctx, p.Config, cred, ref, digest)
	case secrets.ProviderAWS:
		client, err := r.kms(ctx, p, cred)
		if err != nil {
			return nil, err
		}
		out, err := client.Sign(ctx, &kms.SignInput{KeyId: aws.String(ref), Message: digest, MessageType: types.MessageTypeDigest, SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256})
		if err != nil {
			return nil, awssm.Describe(err)
		}
		return out.Signature, nil
	}
	return nil, errors.New("this provider type holds no signing keys")
}

func (r *Remote) kms(ctx context.Context, p secrets.Provider, cred []byte) (*kms.Client, error) {
	cfg, err := awssm.AWSConfig(ctx, p.Config.Region, p.Config.AuthMethod, p.Config.RoleARN, p.Config.ExternalID, p.Config.Endpoint, cred)
	if err != nil {
		return nil, err
	}
	return kms.NewFromConfig(cfg), nil
}
