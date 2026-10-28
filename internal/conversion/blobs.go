package conversion

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
	"regexp"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/storage"
)

// BlobReport counts blobs: all of them, their bytes, and how many were copied rather than found already present
// with the right digest.
type BlobReport struct {
	Blobs   int
	Bytes   int64
	Copied  int
	Missing int64
}

var hexDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func localPath(root, hexd string) string { return filepath.Join(root, "sha256", hexd[:2], hexd) }

func walkLocal(root string, fn func(hexd, path string, size int64) error) error {
	err := filepath.WalkDir(filepath.Join(root, "sha256"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !hexDigest.MatchString(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(d.Name(), p, info.Size())
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// LocalBlobs counts the blobs in a local blob store (storage.Blobs layout).
func LocalBlobs(root string) (BlobReport, error) {
	var r BlobReport
	err := walkLocal(root, func(_, _ string, size int64) error {
		r.Blobs++
		r.Bytes += size
		return nil
	})
	return r, err
}

// RemoteBlobs counts the blobs a remote holds and the bytes of those the local store at root lacks.
func RemoteBlobs(ctx context.Context, remote storage.Remote, root string) (BlobReport, error) {
	var r BlobReport
	err := remote.List(ctx, func(key string, _ time.Time, size int64) error {
		hexd, ok := strings.CutPrefix(key, "sha256/")
		if !ok || !hexDigest.MatchString(hexd) {
			return nil
		}
		r.Blobs++
		r.Bytes += size
		if st, err := os.Stat(localPath(root, hexd)); err != nil || st.Size() != size {
			r.Missing += size
		}
		return nil
	})
	return r, err
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func remoteDigest(ctx context.Context, remote storage.Remote, key string) (string, int64, error) {
	h := sha256.New()
	cw := &counter{w: h}
	if err := remote.Get(ctx, key, cw); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// PushBlobs copies every local blob to remote and reads each one back, comparing digest and size. A local blob
// whose content does not match its name, or a remote copy that differs, stops the copy.
func PushBlobs(ctx context.Context, root string, remote storage.Remote, progress func(done int)) (BlobReport, error) {
	var r BlobReport
	err := walkLocal(root, func(hexd, path string, size int64) error {
		got, n, err := hashFile(path)
		if err != nil {
			return err
		}
		if got != hexd || n != size {
			return fmt.Errorf("the local blob sha256:%s is corrupt (its content hashes to sha256:%s); restore it from a backup first", hexd, got)
		}
		key := "sha256/" + hexd
		ok, err := remote.Exists(ctx, key)
		if err != nil {
			return fmt.Errorf("checking sha256:%s in %s: %w", hexd, remote.Describe(), err)
		}
		if !ok {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			err = remote.Put(ctx, key, f, size)
			f.Close()
			if err != nil {
				return fmt.Errorf("storing sha256:%s in %s: %w", hexd, remote.Describe(), err)
			}
			r.Copied++
		}
		back, m, err := remoteDigest(ctx, remote, key)
		if err != nil {
			return fmt.Errorf("reading sha256:%s back from %s: %w", hexd, remote.Describe(), err)
		}
		if back != hexd || m != size {
			return fmt.Errorf("%s holds sha256:%s with other content (%d bytes, sha256:%s); remove that object and convert again", remote.Describe(), hexd, m, back)
		}
		r.Blobs++
		r.Bytes += size
		if progress != nil {
			progress(r.Blobs)
		}
		return nil
	})
	return r, err
}

// PullBlobs copies every blob remote holds into the local store at root, keeping a local copy only when its digest
// matches. Each download is hashed and its size compared with the listing before it takes its place. own gives new
// files and directories to the controller's user.
func PullBlobs(ctx context.Context, remote storage.Remote, root string, own func(path string) error, progress func(done int)) (BlobReport, error) {
	type entry struct {
		hexd string
		size int64
	}
	var list []entry
	err := remote.List(ctx, func(key string, _ time.Time, size int64) error {
		if hexd, ok := strings.CutPrefix(key, "sha256/"); ok && hexDigest.MatchString(hexd) {
			list = append(list, entry{hexd, size})
		}
		return nil
	})
	if err != nil {
		return BlobReport{}, fmt.Errorf("listing %s: %w", remote.Describe(), err)
	}
	tmpDir := filepath.Join(root, "tmp")
	if err := mkdirOwned(tmpDir, own); err != nil {
		return BlobReport{}, err
	}
	var r BlobReport
	for _, e := range list {
		dst := localPath(root, e.hexd)
		if got, n, err := hashFile(dst); err == nil && got == e.hexd && n == e.size {
			r.Blobs++
			r.Bytes += e.size
			continue
		}
		if err := download(ctx, remote, e.hexd, e.size, tmpDir, dst, own); err != nil {
			return r, err
		}
		r.Blobs++
		r.Bytes += e.size
		r.Copied++
		if progress != nil {
			progress(r.Blobs)
		}
	}
	return r, nil
}

func download(ctx context.Context, remote storage.Remote, hexd string, size int64, tmpDir, dst string, own func(string) error) error {
	tmp, err := os.CreateTemp(tmpDir, "convert-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	cw := &counter{w: io.MultiWriter(tmp, h)}
	err = remote.Get(ctx, "sha256/"+hexd, cw)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("reading sha256:%s from %s: %w", hexd, remote.Describe(), err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hexd || cw.n != size {
		return fmt.Errorf("%s holds sha256:%s with other content (%d bytes, sha256:%s)", remote.Describe(), hexd, cw.n, got)
	}
	if err := mkdirOwned(filepath.Dir(dst), own); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o440); err != nil {
		return err
	}
	if err := own(tmp.Name()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func mkdirOwned(dir string, own func(string) error) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for d := dir; ; d = filepath.Dir(d) {
		if err := own(d); err != nil {
			return err
		}
		if filepath.Base(d) == "sha256" || filepath.Base(d) == "tmp" || d == filepath.Dir(d) {
			return nil
		}
	}
}
