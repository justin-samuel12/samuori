package cluster

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/db"
)

// SharedDir mirrors a directory of controller material, such as the internal CA, through the database so that every
// controller holds the same files. The copy is encrypted with the cluster key and carries a revision; a write based
// on an older revision is refused. Files are small and the directory is flat.
type SharedDir struct {
	Name string
	Dir  string
	By   string

	db    *db.DB
	aead  cipher.AEAD
	keyID string

	mu       sync.Mutex
	revision int64
}

// ErrKeyMismatch means the cluster's material was written with another cluster key than this controller's.
var ErrKeyMismatch = errors.New("cluster: this controller's cluster key differs from the one the cluster uses")

// ErrStale means another controller changed the material since this controller last read it.
var ErrStale = errors.New("cluster: shared material changed on another controller; reload and retry")

// NewSharedDir mirrors dir under name, encrypted with the 32-byte cluster key.
func NewSharedDir(d *db.DB, name, dir, by string, key []byte) (*SharedDir, error) {
	if len(key) != 32 {
		return nil, errors.New("cluster: the cluster key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	return &SharedDir{Name: name, Dir: dir, By: by, db: d, aead: aead, keyID: hex.EncodeToString(sum[:8])}, nil
}

func (s *SharedDir) aad() []byte { return []byte("samuori-cluster-material/" + s.Name) }

// Pull replaces the local files with the cluster's copy when it is newer, reporting whether anything changed. With no
// cluster copy yet it reports false and leaves the directory alone.
func (s *SharedDir) Pull(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rev int64
	var keyID string
	var sealed []byte
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT revision, key_id, ciphertext FROM cluster_material WHERE name = ?`), s.Name).Scan(&rev, &keyID, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if keyID != s.keyID {
		return false, ErrKeyMismatch
	}
	if rev == s.revision {
		return false, nil
	}
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return false, errors.New("cluster: shared material is truncated")
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], s.aad())
	if err != nil {
		return false, fmt.Errorf("cluster: shared material %s does not decrypt: %w", s.Name, err)
	}
	files, err := untar(plain)
	if err != nil {
		return false, err
	}
	if err := s.write(files); err != nil {
		return false, err
	}
	s.revision = rev
	return true, nil
}

// Push stores the local files as the next revision. It fails with ErrStale when the cluster copy moved on since the
// last Pull or Push, and does nothing when the files already match.
func (s *SharedDir) Push(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.read()
	if err != nil {
		return err
	}
	plain, err := tarOf(files)
	if err != nil {
		return err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := s.aead.Seal(nonce, nonce, plain, s.aad())
	now := time.Now().UTC()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var rev int64
		var keyID string
		var current []byte
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT revision, key_id, ciphertext FROM cluster_material WHERE name = ?`), s.Name).Scan(&rev, &keyID, &current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if s.revision != 0 {
				return ErrStale
			}
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO cluster_material (name, revision, key_id, ciphertext, updated_at, updated_by) VALUES (?, 1, ?, ?, ?, ?)`),
				s.Name, s.keyID, sealed, now, s.By); err != nil {
				return ErrStale
			}
			s.revision = 1
			return nil
		case err != nil:
			return err
		}
		if keyID != s.keyID {
			return ErrKeyMismatch
		}
		if rev != s.revision {
			return ErrStale
		}
		if n := s.aead.NonceSize(); len(current) >= n {
			if old, err := s.aead.Open(nil, current[:n], current[n:], s.aad()); err == nil && bytes.Equal(old, plain) {
				return nil
			}
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE cluster_material SET revision = ?, ciphertext = ?, updated_at = ?, updated_by = ? WHERE name = ? AND revision = ?`),
			rev+1, sealed, now, s.By, s.Name, rev)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrStale
		}
		s.revision = rev + 1
		return nil
	})
}

// Revision is the cluster revision the local files correspond to; zero before the first Pull or Push.
func (s *SharedDir) Revision() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func shareable(name string) bool {
	return !strings.HasSuffix(name, ".tmp") && !strings.HasPrefix(name, ".")
}

func (s *SharedDir) read() (map[string][]byte, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !shareable(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files[e.Name()] = b
	}
	return files, nil
}

// write makes the directory hold exactly files. Each file is replaced by rename, so readers never see a partial file.
func (s *SharedDir) write(files map[string][]byte) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	for name, body := range files {
		if filepath.Base(name) != name || !shareable(name) {
			return fmt.Errorf("cluster: shared material holds an invalid name %q", name)
		}
		mode := os.FileMode(0o600)
		if strings.HasSuffix(name, ".crt") || strings.HasSuffix(name, ".crt.retired") {
			mode = 0o644
		}
		tmp := filepath.Join(s.Dir, name+".tmp")
		if err := os.WriteFile(tmp, body, mode); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(s.Dir, name)); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, keep := files[e.Name()]; !keep && e.Type().IsRegular() && shareable(e.Name()) {
			if err := os.Remove(filepath.Join(s.Dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func tarOf(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o600, Size: int64(len(files[n])), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return nil, err
		}
	}
	err := tw.Close()
	return b.Bytes(), err
}

func untar(raw []byte) (map[string][]byte, error) {
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
			return nil, fmt.Errorf("cluster: unexpected entry %q in shared material", h.Name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, err
		}
		files[h.Name] = b
	}
}
