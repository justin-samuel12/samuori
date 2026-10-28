package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Remote is durable object storage holding blobs under keys sha256/<64 hex>. It never decides what a blob is: every
// copy fetched back is verified against its digest before it is used.
type Remote interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string, w io.Writer) error
	Exists(ctx context.Context, key string) (bool, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, fn func(key string, mod time.Time, size int64) error) error
	// Describe names the backend for diagnostics, without credentials.
	Describe() string
	Check(ctx context.Context) error
}

// Kinder is implemented by remotes that name their backend kind, such as s3 or oci.
type Kinder interface{ Kind() string }

// Kind names a remote's backend kind; remotes that do not say are object storage.
func Kind(r Remote) string {
	if k, ok := r.(Kinder); ok {
		return k.Kind()
	}
	return "s3"
}

// ErrRemoteNotFound is returned by Remote.Get for a missing key.
var ErrRemoteNotFound = errors.New("not found in object storage")

// UseRemote routes every class to r.
func (b *Blobs) UseRemote(r Remote) {
	for _, c := range Classes {
		b.Route(c, r)
	}
}

// Route sends new blobs of class c to r, or keeps them local only when r is nil.
func (b *Blobs) Route(c Class, r Remote) {
	if r == nil {
		delete(b.routes, c)
		return
	}
	b.routes[c] = r
}

// RouteOf returns the remote a class is routed to, or nil for local only.
func (b *Blobs) RouteOf(c Class) Remote { return b.routes[c] }

// Remotes returns each distinct routed remote once, in class order.
func (b *Blobs) Remotes() []Remote {
	var out []Remote
	for _, c := range Classes {
		if r := b.routes[c]; r != nil && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

func key(digest string) string { return "sha256/" + digest[len("sha256:"):] }

const remoteTimeout = 30 * time.Minute

// upload copies a newly stored blob to object storage. A Put that cannot be made durable fails.
func (b *Blobs) upload(remote Remote, digest, path string, size int64) error {
	if remote == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
	defer cancel()
	if ok, err := remote.Exists(ctx, key(digest)); err == nil && ok {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := remote.Put(ctx, key(digest), f, size); err != nil {
		return fmt.Errorf("storing %s in object storage: %w", digest, err)
	}
	return nil
}

// fetch fills the local cache from the first remote holding the blob, verifying the digest before the copy becomes
// visible. A corrupted copy is never used; another remote may still hold a good one.
func (b *Blobs) fetch(digest, dst string) error {
	remotes := b.Remotes()
	if len(remotes) == 0 {
		return ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
	defer cancel()
	result := ErrNotFound
	for _, r := range remotes {
		err := b.fetchFrom(ctx, r, digest, dst)
		if err == nil || (!errors.Is(err, ErrNotFound) && !errors.Is(err, ErrCorrupt)) {
			return err
		}
		if errors.Is(err, ErrCorrupt) {
			result = ErrCorrupt
		}
	}
	return result
}

func (b *Blobs) fetchFrom(ctx context.Context, r Remote, digest, dst string) error {
	tmp, err := os.CreateTemp(filepath.Join(b.root, "tmp"), "fetch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	err = r.Get(ctx, key(digest), io.MultiWriter(tmp, h))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, ErrRemoteNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("reading %s from %s: %w", digest, r.Describe(), err)
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		return ErrCorrupt
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o440); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// EvictCache removes local copies written or fetched more than olderThan ago that a remote holds. Copies no remote
// holds, such as blobs of classes kept local, are never removed.
func (b *Blobs) EvictCache(olderThan time.Duration) (int, error) {
	remotes := b.Remotes()
	if len(remotes) == 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-olderThan)
	n := 0
	err := filepath.WalkDir(filepath.Join(b.root, "sha256"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			return err
		}
		held := false
		for _, r := range remotes {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			ok, err := r.Exists(ctx, "sha256/"+d.Name())
			cancel()
			if err != nil {
				return err
			}
			if held = ok; held {
				break
			}
		}
		if !held {
			return nil
		}
		if err := os.Remove(p); err == nil {
			n++
		}
		return nil
	})
	return n, err
}
