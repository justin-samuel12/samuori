// Package tags resolves public image tags to manifest digests on the controller, anonymously, for the base-images
// policy's tag pinning (ADR 0049).
package tags

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"samuori.invalid/samuori/internal/netguard"
)

// Resolver asks the registry for a tag's manifest with a HEAD request; for a multi-platform image that is the
// index. Private addresses are refused unless AllowPrivate is set (SEC-NET-03).
type Resolver struct {
	Timeout      time.Duration
	AllowPrivate bool
	PlainHTTP    bool
}

func New() *Resolver { return &Resolver{Timeout: 20 * time.Second} }

// Endpoint is the registry host that serves a reference's domain; Docker Hub's API lives on registry-1.docker.io.
func Endpoint(domain string) string {
	if domain == "docker.io" || domain == "index.docker.io" {
		return "registry-1.docker.io"
	}
	return domain
}

func (r *Resolver) ResolveTag(ctx context.Context, ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("invalid image reference: %w", err)
	}
	if _, ok := named.(reference.Digested); ok {
		return "", errors.New("the reference already has a digest")
	}
	tagged, ok := reference.TagNameOnly(named).(reference.Tagged)
	if !ok {
		return "", errors.New("the reference has no tag")
	}
	repo, err := remote.NewRepository(Endpoint(reference.Domain(named)) + "/" + reference.Path(named))
	if err != nil {
		return "", err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	repo.PlainHTTP = r.PlainHTTP
	repo.Client = &auth.Client{Client: netguard.Client(r.AllowPrivate, timeout, 5), Credential: auth.StaticCredential(repo.Reference.Registry, auth.EmptyCredential), Cache: auth.NewCache()}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	desc, err := repo.Resolve(ctx, tagged.Tag())
	switch {
	case errors.Is(err, errdef.ErrNotFound):
		return "", fmt.Errorf("the tag %s does not exist in %s", tagged.Tag(), reference.FamiliarName(named))
	case err != nil:
		return "", err
	}
	if desc.Digest.Algorithm() != digest.SHA256 || desc.Digest.Validate() != nil {
		return "", fmt.Errorf("the registry returned an unusable digest %q", desc.Digest)
	}
	return desc.Digest.String(), nil
}
