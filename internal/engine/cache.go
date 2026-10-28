package engine

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A build cache travels as a TAR of an OCI layout: oci-layout, index.json and blobs named by their digests
// (docs/architecture/build-cache.md). Anything else is left out when packing and refused when unpacking.
var cacheEntry = regexp.MustCompile(`^(oci-layout|index\.json|blobs/sha256/[a-f0-9]{64})$`)

// ErrCacheInvalid means a cache archive broke the layout rules; the build then runs without it.
var ErrCacheInvalid = errors.New("the build cache is not a valid OCI layout")

// PackCache writes the layout in dir to w in a fixed order with fixed metadata.
func PackCache(dir string, w io.Writer) error {
	var names []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && cacheEntry.MatchString(filepath.ToSlash(rel)) {
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(names)
	tw := tar.NewWriter(w)
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: info.Size(), ModTime: time.Unix(0, 0), Format: tar.FormatPAX})
		}
		if err == nil {
			_, err = io.CopyN(tw, f, info.Size())
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// UnpackCache extracts a cache archive into dir, which must not exist yet. It accepts only layout files, checks
// every blob against the digest in its name and stops past max bytes; on any violation dir is removed.
func UnpackCache(r io.Reader, dir string, max int64) (err error) {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	tr := tar.NewReader(r)
	var total int64
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCacheInvalid, err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir && (name == "blobs/" || name == "blobs/sha256/") {
			continue
		}
		if h.Typeflag != tar.TypeReg || !cacheEntry.MatchString(name) || seen[name] {
			return fmt.Errorf("%w: unexpected entry %q", ErrCacheInvalid, name)
		}
		seen[name] = true
		if total += h.Size; h.Size < 0 || total > max {
			return fmt.Errorf("%w: larger than %d bytes", ErrCacheInvalid, max)
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		h256 := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h256), io.LimitReader(tr, h.Size))
		f.Close()
		if err != nil || n != h.Size {
			return fmt.Errorf("%w: short entry %q", ErrCacheInvalid, name)
		}
		if hexd, ok := strings.CutPrefix(name, "blobs/sha256/"); ok && hex.EncodeToString(h256.Sum(nil)) != hexd {
			return fmt.Errorf("%w: %s does not match its digest", ErrCacheInvalid, name)
		}
	}
	if !seen["index.json"] {
		return fmt.Errorf("%w: no index.json", ErrCacheInvalid)
	}
	return nil
}
