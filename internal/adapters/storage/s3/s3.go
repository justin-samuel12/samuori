// Package s3 implements storage.Remote on Amazon S3 or an S3-compatible service with the AWS SDK for Go v2.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"samuori.invalid/samuori/internal/adapters/secretstore/awssm"
	"samuori.invalid/samuori/internal/storage"
)

// Config locates the bucket. Credentials come from the controller's AWS identity, an assumed role, or a stored
// access key, as for other AWS adapters.
type Config struct {
	Bucket, Prefix, Region, Endpoint string
	// PathStyle addresses buckets as endpoint/bucket, which most S3-compatible services need.
	PathStyle                       bool
	AuthMethod, RoleARN, ExternalID string
	Credential                      []byte
}

type Remote struct {
	cfg    Config
	client *s3.Client
}

func New(ctx context.Context, cfg Config) (*Remote, error) {
	if cfg.Bucket == "" || cfg.Region == "" {
		return nil, errors.New("object storage needs a bucket and a region")
	}
	method := cfg.AuthMethod
	if method == "" {
		method = "workload-identity"
	}
	awsCfg, err := awssm.AWSConfig(ctx, cfg.Region, method, cfg.RoleARN, cfg.ExternalID, cfg.Endpoint, cfg.Credential)
	if err != nil {
		return nil, err
	}
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.PathStyle
		o.DisableLogOutputChecksumValidationSkipped = true
	})
	return &Remote{cfg: cfg, client: client}, nil
}

func (r *Remote) object(key string) *string {
	if r.cfg.Prefix == "" {
		return aws.String(key)
	}
	return aws.String(r.cfg.Prefix + "/" + key)
}

func (r *Remote) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: r.object(key), Body: body, ContentLength: aws.Int64(size)})
	return describe(err)
}

func (r *Remote) Get(ctx context.Context, key string, w io.Writer) error {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: r.object(key)})
	if err != nil {
		var nf *types.NoSuchKey
		if errors.As(err, &nf) {
			return storage.ErrRemoteNotFound
		}
		return describe(err)
	}
	defer out.Body.Close()
	_, err = io.Copy(w, out.Body)
	return err
}

func (r *Remote) Exists(ctx context.Context, key string) (bool, error) {
	_, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: r.object(key)})
	if err == nil {
		return true, nil
	}
	var api smithy.APIError
	if errors.As(err, &api) && (api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchKey") {
		return false, nil
	}
	return false, describe(err)
}

func (r *Remote) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: r.object(key)})
	return describe(err)
}

func (r *Remote) List(ctx context.Context, fn func(key string, mod time.Time, size int64) error) error {
	prefix := "sha256/"
	if r.cfg.Prefix != "" {
		prefix = r.cfg.Prefix + "/" + prefix
	}
	p := s3.NewListObjectsV2Paginator(r.client, &s3.ListObjectsV2Input{Bucket: aws.String(r.cfg.Bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return describe(err)
		}
		for _, o := range page.Contents {
			k := strings.TrimPrefix(aws.ToString(o.Key), r.cfg.Prefix+"/")
			if err := fn(k, aws.ToTime(o.LastModified), aws.ToInt64(o.Size)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Remote) Describe() string {
	where := "s3://" + r.cfg.Bucket
	if r.cfg.Prefix != "" {
		where += "/" + r.cfg.Prefix
	}
	if r.cfg.Endpoint != "" {
		where += " at " + r.cfg.Endpoint
	}
	return where
}

func (r *Remote) Check(ctx context.Context) error {
	_, err := r.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(r.cfg.Bucket)})
	if err != nil {
		return fmt.Errorf("bucket %s: %w", r.cfg.Bucket, describe(err))
	}
	return nil
}

func describe(err error) error {
	if err == nil {
		return nil
	}
	return awssm.Describe(err)
}
