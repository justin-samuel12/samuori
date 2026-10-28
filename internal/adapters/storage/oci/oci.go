// Package oci keeps Samuori's blobs in an OCI registry repository (D04, docs/architecture/registry-and-artifacts.md).
// Each blob is pushed with a small OCI 1.1 artifact manifest that references it and is tagged sha256-<hex>, so the
// registry's own garbage collection never removes a stored blob and the tag list enumerates the store.
package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"samuori.invalid/samuori/internal/netguard"
	"samuori.invalid/samuori/internal/storage"
)

const (
	ArtifactType      = "application/vnd.samuori.blob.v1"
	blobMediaType     = "application/octet-stream"
	createdAnnotation = ocispec.AnnotationCreated
)

type Config struct {
	// Repository is host[:port]/path, without a scheme.
	Repository          string
	Username, Password  string
	PlainHTTP           bool
	AllowPrivateNetwork bool
}

type Store struct {
	repo *remote.Repository
	name string
	now  func() time.Time
}

var hexKey = regexp.MustCompile(`^sha256/([a-f0-9]{64})$`)

func New(cfg Config) (*Store, error) {
	if strings.Contains(cfg.Repository, "://") {
		return nil, errors.New("give the repository as host[:port]/path, without a scheme")
	}
	repo, err := remote.NewRepository(cfg.Repository)
	if err != nil {
		return nil, fmt.Errorf("the repository is not valid: %w", err)
	}
	if cfg.PlainHTTP {
		host, _, err := net.SplitHostPort(repo.Reference.Registry)
		if err != nil {
			host = repo.Reference.Registry
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("plain HTTP is allowed only to a loopback registry")
		}
	}
	repo.PlainHTTP = cfg.PlainHTTP
	client := netguard.Client(cfg.AllowPrivateNetwork, 30*time.Minute, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cred := auth.EmptyCredential
	if cfg.Username != "" || cfg.Password != "" {
		cred = auth.Credential{Username: cfg.Username, Password: cfg.Password}
	}
	repo.Client = &auth.Client{Client: client, Credential: auth.StaticCredential(repo.Reference.Registry, cred), Cache: auth.NewCache()}
	return &Store{repo: repo, name: cfg.Repository, now: time.Now}, nil
}

func (s *Store) Kind() string     { return "oci" }
func (s *Store) Describe() string { return "oci://" + s.name }

func split(key string) (digest.Digest, string, error) {
	m := hexKey.FindStringSubmatch(key)
	if m == nil {
		return "", "", fmt.Errorf("invalid key %q", key)
	}
	return digest.Digest("sha256:" + m[1]), "sha256-" + m[1], nil
}

func notFound(err error) bool {
	var resp *errcode.ErrorResponse
	return errors.Is(err, errdef.ErrNotFound) || (errors.As(err, &resp) && resp.StatusCode == http.StatusNotFound)
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	d, tag, err := split(key)
	if err != nil {
		return err
	}
	blob := ocispec.Descriptor{MediaType: blobMediaType, Digest: d, Size: size}
	if ok, err := s.repo.Blobs().Exists(ctx, blob); err != nil || !ok {
		if err := s.repo.Blobs().Push(ctx, blob, r); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return err
		}
	}
	manifest, err := oras.PackManifest(ctx, s.repo, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Layers:              []ocispec.Descriptor{blob},
		ManifestAnnotations: map[string]string{createdAnnotation: s.now().UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return err
	}
	return s.repo.Tag(ctx, manifest, tag)
}

func (s *Store) Get(ctx context.Context, key string, w io.Writer) error {
	d, _, err := split(key)
	if err != nil {
		return err
	}
	desc, err := s.repo.Blobs().Resolve(ctx, d.String())
	if notFound(err) {
		return storage.ErrRemoteNotFound
	}
	if err != nil {
		return err
	}
	rc, err := s.repo.Blobs().Fetch(ctx, desc)
	if notFound(err) {
		return storage.ErrRemoteNotFound
	}
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(w, rc)
	return err
}

// Exists reports a blob held durably: its tagged manifest exists, not merely a blob the registry may collect.
func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	_, tag, err := split(key)
	if err != nil {
		return false, err
	}
	_, err = s.repo.Resolve(ctx, tag)
	if notFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Delete removes the blob's manifest; the registry reclaims the blob in its own garbage collection.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, tag, err := split(key)
	if err != nil {
		return err
	}
	desc, err := s.repo.Resolve(ctx, tag)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	err = s.repo.Delete(ctx, desc)
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) && (resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusUnsupportedMediaType) {
		return errors.New("the registry does not allow deleting manifests; enable deletes for this repository")
	}
	if err != nil && !notFound(err) {
		return err
	}
	// Some registries keep a tag after its manifest is deleted by digest; distribution-spec 1.1 allows deleting it.
	if _, err := s.repo.Resolve(ctx, tag); notFound(err) {
		return nil
	}
	return s.deleteTag(ctx, tag)
}

func (s *Store) deleteTag(ctx context.Context, tag string) error {
	ref := s.repo.Reference
	scheme := "https"
	if s.repo.PlainHTTP {
		scheme = "http"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, ref.Registry, ref.Repository, tag), nil)
	if err != nil {
		return err
	}
	resp, err := s.repo.Client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound:
		return nil
	case resp.StatusCode == http.StatusMethodNotAllowed:
		return errors.New("the registry kept the tag of a deleted blob and does not allow deleting tags")
	}
	return fmt.Errorf("deleting tag %s: HTTP %d", tag, resp.StatusCode)
}

// List reports keys from the tag list; times and sizes are not in tag lists, so they are zero (see ModTime).
func (s *Store) List(ctx context.Context, fn func(key string, mod time.Time, size int64) error) error {
	return s.repo.Tags(ctx, "", func(tags []string) error {
		for _, t := range tags {
			if hexd, ok := strings.CutPrefix(t, "sha256-"); ok && hexKey.MatchString("sha256/"+hexd) {
				if err := fn("sha256/"+hexd, time.Time{}, 0); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ModTime reads the time recorded in the blob's manifest.
func (s *Store) ModTime(ctx context.Context, key string) (time.Time, error) {
	_, tag, err := split(key)
	if err != nil {
		return time.Time{}, err
	}
	desc, rc, err := s.repo.FetchReference(ctx, tag)
	if err != nil {
		return time.Time{}, err
	}
	defer rc.Close()
	var m ocispec.Manifest
	if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&m); err != nil {
		return time.Time{}, err
	}
	if desc.MediaType != ocispec.MediaTypeImageManifest || m.ArtifactType != ArtifactType {
		return time.Time{}, errors.New("the tag does not hold a Samuori blob manifest")
	}
	return time.Parse(time.RFC3339, m.Annotations[createdAnnotation])
}

// Check confirms the repository answers with the configured credentials.
func (s *Store) Check(ctx context.Context) error {
	err := s.repo.Tags(ctx, "", func([]string) error { return nil })
	if notFound(err) {
		return nil
	}
	return err
}
