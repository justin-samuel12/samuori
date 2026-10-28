package publication

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"samuori.invalid/samuori/internal/signing"
)

// Target is one destination repository with credentials resolved just in time on the controller.
type Target struct {
	Endpoint, Repository string
	PlainHTTP            bool
	AllowPrivate         bool
	Username, Password   string
	Token                string
}

func (t Target) repo() (*remote.Repository, error) {
	r, err := remote.NewRepository(t.Endpoint + "/" + t.Repository)
	if err != nil {
		return nil, err
	}
	r.PlainHTTP = t.PlainHTTP
	cred := auth.EmptyCredential
	switch {
	case t.Token != "":
		cred = auth.Credential{AccessToken: t.Token}
	case t.Username != "":
		cred = auth.Credential{Username: t.Username, Password: t.Password}
	}
	r.Client = &auth.Client{Client: httpClient(t.AllowPrivate), Credential: auth.StaticCredential(t.Endpoint, cred), Cache: auth.NewCache()}
	return r, nil
}

// Pusher copies images to destinations and verifies them there. It runs only on the controller.
type Pusher interface {
	// Current returns the digest a tag points to at the destination, or "" when the tag does not exist.
	Current(ctx context.Context, t Target, tag string) (string, error)
	Push(ctx context.Context, t Target, archive, image string, tags []string) error
	Verify(ctx context.Context, t Target, image string, tags []string) error
	Ping(ctx context.Context, t Target) error
	// PushArtifact uploads a signature or attestation image, tagged unless tag is empty, and returns its digest.
	PushArtifact(ctx context.Context, t Target, a signing.Artifact, tag string) (string, error)
	// FetchTag returns the manifest a tag or digest points to and a reader for that repository's blobs.
	FetchTag(ctx context.Context, t Target, tag string) ([]byte, func(digest string) ([]byte, error), error)
	// Describe returns the descriptor of a manifest at the destination.
	Describe(ctx context.Context, t Target, reference string) (signing.Descriptor, error)
	// Referrers reports whether the registry serves the OCI 1.1 referrers API and, if so, returns the manifests
	// that refer to image.
	Referrers(ctx context.Context, t Target, image string) (bool, [][]byte, error)
}

type OrasPusher struct{}

func (OrasPusher) Current(ctx context.Context, t Target, tag string) (string, error) {
	r, err := t.repo()
	if err != nil {
		return "", err
	}
	d, err := r.Resolve(ctx, tag)
	if errors.Is(err, errdef.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return d.Digest.String(), nil
}

// Push copies the image graph by digest, including referrers such as signatures, then applies the tags.
func (OrasPusher) Push(ctx context.Context, t Target, archive, image string, tags []string) error {
	src, err := oci.NewFromTar(ctx, archive)
	if err != nil {
		return fmt.Errorf("reading the stored image: %w", err)
	}
	dst, err := t.repo()
	if err != nil {
		return err
	}
	desc, err := oras.ExtendedCopy(ctx, src, image, dst, image, oras.DefaultExtendedCopyOptions)
	if err != nil {
		return fmt.Errorf("copying to the destination: %w", err)
	}
	for _, tag := range tags {
		if err := dst.Tag(ctx, desc, tag); err != nil {
			return fmt.Errorf("tagging %s: %w", tag, err)
		}
	}
	return nil
}

// Verify re-reads the destination: the manifest by digest, every child manifest and blob it references, and every
// tag (SEC-REG-02).
func (OrasPusher) Verify(ctx context.Context, t Target, image string, tags []string) error {
	r, err := t.repo()
	if err != nil {
		return err
	}
	root, err := r.Resolve(ctx, image)
	if err != nil {
		return fmt.Errorf("the destination does not have %s: %w", image, err)
	}
	if root.Digest.String() != image {
		return fmt.Errorf("the destination resolved %s to %s", image, root.Digest)
	}
	if err := verifyNode(ctx, r, root, 0); err != nil {
		return err
	}
	for _, tag := range tags {
		d, err := r.Resolve(ctx, tag)
		if err != nil {
			return fmt.Errorf("tag %s: %w", tag, err)
		}
		if d.Digest.String() != image {
			return fmt.Errorf("tag %s points to %s, not %s", tag, d.Digest, image)
		}
	}
	return nil
}

func verifyNode(ctx context.Context, r *remote.Repository, d ocispec.Descriptor, depth int) error {
	if depth > 3 {
		return errors.New("the image graph is too deep")
	}
	raw, err := content.FetchAll(ctx, r, d)
	if err != nil {
		return fmt.Errorf("manifest %s does not verify: %w", d.Digest, err)
	}
	var m struct {
		MediaType string               `json:"mediaType"`
		Manifests []ocispec.Descriptor `json:"manifests"`
		Config    *ocispec.Descriptor  `json:"config"`
		Layers    []ocispec.Descriptor `json:"layers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("manifest %s: %w", d.Digest, err)
	}
	for _, child := range m.Manifests {
		if err := verifyNode(ctx, r, child, depth+1); err != nil {
			return err
		}
	}
	blobs := m.Layers
	if m.Config != nil {
		blobs = append(blobs, *m.Config)
	}
	for _, b := range blobs {
		got, err := r.Blobs().Resolve(ctx, b.Digest.String())
		if err != nil {
			return fmt.Errorf("blob %s is missing at the destination: %w", b.Digest, err)
		}
		if got.Size != b.Size {
			return fmt.Errorf("blob %s has size %d at the destination, expected %d", b.Digest, got.Size, b.Size)
		}
	}
	return nil
}

func (OrasPusher) PushArtifact(ctx context.Context, t Target, a signing.Artifact, tag string) (string, error) {
	r, err := t.repo()
	if err != nil {
		return "", err
	}
	for _, b := range a.Blobs() {
		d := content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayer, b)
		if err := r.Blobs().Push(ctx, d, bytes.NewReader(b)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return "", fmt.Errorf("uploading %s: %w", d.Digest, err)
		}
	}
	var m struct {
		Subject *ocispec.Descriptor `json:"subject"`
	}
	if json.Unmarshal(a.Manifest, &m) == nil && m.Subject != nil {
		// Referrers are pushed only after probing the API; this stops ORAS from also writing the referrers tag schema.
		_ = r.SetReferrersCapability(true)
	}
	md := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, a.Manifest)
	if tag == "" {
		err = r.Manifests().Push(ctx, md, bytes.NewReader(a.Manifest))
	} else {
		err = r.Manifests().PushReference(ctx, md, bytes.NewReader(a.Manifest), tag)
	}
	if err != nil {
		return "", fmt.Errorf("pushing %s: %w", md.Digest, err)
	}
	return md.Digest.String(), nil
}

func (OrasPusher) Describe(ctx context.Context, t Target, reference string) (signing.Descriptor, error) {
	r, err := t.repo()
	if err != nil {
		return signing.Descriptor{}, err
	}
	d, err := r.Resolve(ctx, reference)
	if err != nil {
		return signing.Descriptor{}, err
	}
	return signing.Descriptor{MediaType: d.MediaType, Digest: d.Digest.String(), Size: d.Size}, nil
}

func (OrasPusher) Referrers(ctx context.Context, t Target, image string) (bool, [][]byte, error) {
	r, err := t.repo()
	if err != nil {
		return false, nil, err
	}
	supported, err := referrersAPI(ctx, r, t)
	if err != nil || !supported {
		return false, nil, err
	}
	_ = r.SetReferrersCapability(true)
	subject, err := r.Resolve(ctx, image)
	if err != nil {
		return true, nil, err
	}
	var out [][]byte
	err = r.Referrers(ctx, subject, "", func(refs []ocispec.Descriptor) error {
		for _, d := range refs {
			if len(out) >= 256 || d.Size > 1<<20 {
				continue
			}
			raw, err := content.FetchAll(ctx, r, d)
			if err != nil {
				return err
			}
			out = append(out, raw)
		}
		return nil
	})
	return true, out, err
}

// referrersAPI probes GET /v2/<name>/referrers/<digest>; a registry that serves it answers with an image index.
func referrersAPI(ctx context.Context, r *remote.Repository, t Target) (bool, error) {
	scheme := "https"
	if t.PlainHTTP {
		scheme = "http"
	}
	ref := r.Reference
	ctx = auth.AppendRepositoryScope(ctx, ref, auth.ActionPull)
	u := fmt.Sprintf("%s://%s/v2/%s/referrers/sha256:%s", scheme, ref.Host(), ref.Repository, strings.Repeat("0", 64))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusOK:
		mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
		return strings.TrimSpace(mt) == ocispec.MediaTypeImageIndex, nil
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusBadRequest:
		return false, nil
	default:
		return false, fmt.Errorf("probing the referrers API: HTTP %d", resp.StatusCode)
	}
}

func (OrasPusher) FetchTag(ctx context.Context, t Target, tag string) ([]byte, func(string) ([]byte, error), error) {
	r, err := t.repo()
	if err != nil {
		return nil, nil, err
	}
	d, err := r.Resolve(ctx, tag)
	if err != nil {
		return nil, nil, err
	}
	manifest, err := content.FetchAll(ctx, r, d)
	if err != nil {
		return nil, nil, err
	}
	fetch := func(digest string) ([]byte, error) {
		bd, err := r.Blobs().Resolve(ctx, digest)
		if err != nil {
			return nil, err
		}
		if bd.Size > 16<<20 {
			return nil, fmt.Errorf("blob %s is too large", digest)
		}
		return content.FetchAll(ctx, r.Blobs(), bd)
	}
	return manifest, fetch, nil
}

func (OrasPusher) Ping(ctx context.Context, t Target) error {
	r, err := t.repo()
	if err != nil {
		return err
	}
	_, err = r.Resolve(ctx, "samuori-connection-test-nonexistent")
	if err == nil || errors.Is(err, errdef.ErrNotFound) {
		return nil
	}
	return err
}

// ImageDigest reads the image identity, the single top-level manifest or index, from an OCI archive.
func ImageDigest(archive string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("the archive has no index.json")
		}
		if err != nil {
			return "", err
		}
		if strings.TrimPrefix(h.Name, "./") != "index.json" {
			continue
		}
		var idx struct {
			Manifests []ocispec.Descriptor `json:"manifests"`
		}
		if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&idx); err != nil {
			return "", err
		}
		if len(idx.Manifests) != 1 {
			return "", fmt.Errorf("the archive holds %d top-level images; exactly one is required", len(idx.Manifests))
		}
		return idx.Manifests[0].Digest.String(), nil
	}
}
