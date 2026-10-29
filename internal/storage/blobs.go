// Package storage holds content-addressed blobs. Every read verifies the content against its digest, so data
// altered after it was written is reported rather than served (TM-17).
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("blob not found")
	ErrCorrupt  = errors.New("blob content does not match its digest")
	ErrTooLarge = errors.New("blob exceeds the size limit")
)

var digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Class is the kind of data a blob holds; each class is routed to one backend (D04).
type Class string

const (
	ClassSource Class = "source"
	ClassImage  Class = "image"
	ClassReport Class = "report"
	ClassLogo   Class = "logo"
	ClassOther  Class = "other"
)

var Classes = []Class{ClassSource, ClassImage, ClassReport, ClassLogo, ClassOther}

// Blobs is a filesystem blob store laid out as <root>/sha256/<2 hex>/<64 hex>. A class routed to a Remote keeps its
// durable copy there and uses the directory as a verified cache; other classes keep the directory as the only copy
// (docs/architecture/registry-and-artifacts.md#storage-backends).
type Blobs struct {
	root   string
	routes map[Class]Remote
}

func NewBlobs(root string) (*Blobs, error) {
	for _, d := range []string{filepath.Join(root, "sha256"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	return &Blobs{root: root, routes: map[Class]Remote{}}, nil
}

func (b *Blobs) path(digest string) (string, error) {
	if !digestRE.MatchString(digest) {
		return "", fmt.Errorf("invalid digest %q", digest)
	}
	hexd := digest[len("sha256:"):]
	return filepath.Join(b.root, "sha256", hexd[:2], hexd), nil
}

// Put stores r as ClassOther; see PutClass.
func (b *Blobs) Put(r io.Reader, max int64) (string, int64, error) {
	return b.PutClass(ClassOther, r, max)
}

// PutClass stores r, returning its digest and size; more than max bytes fails with ErrTooLarge. When the class is
// routed to a Remote, the blob counts as stored only once the remote holds it.
func (b *Blobs) PutClass(c Class, r io.Reader, max int64) (string, int64, error) {
	tmp, err := os.CreateTemp(filepath.Join(b.root, "tmp"), "put-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, max+1))
	if err == nil && n > max {
		err = ErrTooLarge
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	dst, _ := b.path(digest)
	if _, err := os.Stat(dst); err == nil {
		// A repeated upload restarts the garbage-collection grace period: it may not be recorded yet. Local copies
		// outlive the grace period (storage.cacheEvictAfter >= 1h), so a remote's older time never decides.
		now := time.Now()
		if err := os.Chtimes(dst, now, now); err != nil {
			return "", 0, err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return "", 0, err
		}
		if err := os.Chmod(tmp.Name(), 0o440); err != nil {
			return "", 0, err
		}
		if err := os.Rename(tmp.Name(), dst); err != nil {
			return "", 0, err
		}
	}
	if err := b.upload(b.routes[c], digest, dst, n); err != nil {
		return "", 0, err
	}
	return digest, n, nil
}

// Open returns a reader whose final Read reports ErrCorrupt instead of io.EOF if the content does not match.
// File opens a blob for random access without verifying it; callers that read untrusted structure from it verify
// what they use.
func (b *Blobs) File(digest string) (*os.File, error) {
	p, err := b.path(digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		if err := b.fetch(digest, p); err != nil {
			return nil, err
		}
		f, err = os.Open(p)
	}
	return f, err
}

func (b *Blobs) Open(digest string) (io.ReadCloser, error) {
	p, err := b.path(digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		if err := b.fetch(digest, p); err != nil {
			return nil, err
		}
		f, err = os.Open(p)
	}
	if err != nil {
		return nil, err
	}
	return &verifying{f: f, h: sha256.New(), want: digest[len("sha256:"):]}, nil
}

// ReadAll reads and verifies a whole blob of at most max bytes.
func (b *Blobs) ReadAll(digest string, max int64) ([]byte, error) {
	rc, err := b.Open(digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooLarge
	}
	return data, nil
}

type verifying struct {
	f    *os.File
	h    hash.Hash
	want string
}

func (v *verifying) Read(p []byte) (int, error) {
	n, err := v.f.Read(p)
	v.h.Write(p[:n])
	if errors.Is(err, io.EOF) && hex.EncodeToString(v.h.Sum(nil)) != v.want {
		return n, ErrCorrupt
	}
	return n, err
}

func (v *verifying) Close() error { return v.f.Close() }

// Root returns the store's directory.
func (b *Blobs) Root() string { return b.root }

// Walk calls fn once for every blob held locally or by any remote. A remote entry without a time or size takes them
// from the local copy when there is one; a zero time means unknown (see ModTime).
func (b *Blobs) Walk(fn func(digest string, mod time.Time, size int64) error) error {
	seen := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(b.root, "sha256"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		seen["sha256:"+d.Name()] = true
		return fn("sha256:"+d.Name(), info.ModTime(), info.Size())
	})
	if err != nil {
		return err
	}
	for _, r := range b.Remotes() {
		err := r.List(context.Background(), func(key string, mod time.Time, size int64) error {
			hexd, ok := strings.CutPrefix(key, "sha256/")
			if d := "sha256:" + hexd; ok && digestRE.MatchString(d) && !seen[d] {
				seen[d] = true
				return fn(d, mod, size)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ModTimer is implemented by remotes that can report when a blob was stored, for listings that omit it.
type ModTimer interface {
	ModTime(ctx context.Context, key string) (time.Time, error)
}

// ModTime reports when a blob was stored: from the local copy, else from a remote that knows. ok is false when no
// time can be established; garbage collection then keeps the blob.
func (b *Blobs) ModTime(digest string) (time.Time, bool) {
	if p, err := b.path(digest); err == nil {
		if info, err := os.Stat(p); err == nil {
			return info.ModTime(), true
		}
	}
	for _, r := range b.Remotes() {
		if mt, ok := r.(ModTimer); ok {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			t, err := mt.ModTime(ctx, key(digest))
			cancel()
			if err == nil && !t.IsZero() {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func (b *Blobs) Delete(digest string) error {
	p, err := b.path(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, r := range b.Remotes() {
		if err := r.Delete(context.Background(), key(digest)); err != nil {
			return err
		}
	}
	return nil
}

// Size returns a blob's size, fetching it from a remote first when only the remote holds it.
func (b *Blobs) Size(digest string) (int64, error) {
	f, err := b.File(digest)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
