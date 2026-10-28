// Package backup writes and restores encrypted, verifiable backups of a controller (D27, ADR 0035). The database is
// copied with its own tools from one consistent snapshot (SQLite VACUUM INTO, pg_dump with an exported snapshot),
// and per-table checksums taken from that snapshot prove a restore. Archives are age-encrypted tar files.
package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"samuori.invalid/samuori/internal/db"
)

const Format = "samuori-backup/1"

// Manifest describes an archive; it is its last entry, after everything it lists.
type Manifest struct {
	Format        string           `json:"format"`
	CreatedAt     time.Time        `json:"createdAt"`
	Version       string           `json:"samuoriVersion"`
	Dialect       db.Dialect       `json:"dialect"`
	SchemaVersion int64            `json:"schemaVersion"`
	Database      string           `json:"database"`
	Tables        []db.TableReport `json:"tables"`
	Files         []File           `json:"files"`
	Blobs         int              `json:"blobs"`
	BlobBytes     int64            `json:"blobBytes"`
	IncludesKeys  bool             `json:"includesKeys"`
}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Options select what Create copies. Blobs held only by remote storage are never copied: that storage keeps its own
// durable copies (versioning, replication).
type Options struct {
	DSN     string
	DataDir string
	// KeyFile is the secret store key (or cluster key); without it, stored secrets cannot be read after a restore.
	KeyFile    string
	SkipKeys   bool
	SkipBlobs  bool
	PGDump     string
	PGRestore  string
	Version    string
	Recipients []age.Recipient
	Temp       string
	Now        func() time.Time
}

func (o Options) tool(name, set string) string {
	if set != "" {
		return set
	}
	return name
}

// Create writes an encrypted archive to w and returns its manifest.
func Create(ctx context.Context, o Options, w io.Writer) (Manifest, error) {
	if len(o.Recipients) == 0 {
		return Manifest{}, errors.New("backup: no age recipient; archives are always encrypted")
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	tmp, err := os.MkdirTemp(o.Temp, "samuori-backup-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(tmp)
	m := Manifest{Format: Format, CreatedAt: now().UTC(), Version: o.Version, IncludesKeys: !o.SkipKeys}
	dbFile, err := snapshot(ctx, o, tmp, &m)
	if err != nil {
		return Manifest{}, err
	}
	enc, err := age.Encrypt(w, o.Recipients...)
	if err != nil {
		return Manifest{}, err
	}
	tw := tar.NewWriter(enc)
	add := func(name, path string, mode int64) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: st.Size(), Typeflag: tar.TypeReg, ModTime: m.CreatedAt}); err != nil {
			return err
		}
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(tw, h), f); err != nil {
			return err
		}
		m.Files = append(m.Files, File{Path: name, SHA256: hex.EncodeToString(h.Sum(nil)), Size: st.Size()})
		return nil
	}
	if err := add(m.Database, dbFile, 0o600); err != nil {
		return Manifest{}, err
	}
	pki := filepath.Join(o.DataDir, "pki")
	entries, err := os.ReadDir(pki)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, err
	}
	for _, e := range entries {
		if e.Type().IsRegular() && !strings.HasSuffix(e.Name(), ".tmp") {
			if err := add("material/pki/"+e.Name(), filepath.Join(pki, e.Name()), 0o600); err != nil {
				return Manifest{}, err
			}
		}
	}
	if !o.SkipKeys {
		if err := add("material/keys/secrets.key", o.KeyFile, 0o600); err != nil {
			return Manifest{}, fmt.Errorf("backup: the key file: %w", err)
		}
	}
	if !o.SkipBlobs {
		root := filepath.Join(o.DataDir, "blobs", "sha256")
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return err
			}
			before := len(m.Files)
			if err := add("blobs/sha256/"+d.Name(), p, 0o400); err != nil {
				return err
			}
			if got := m.Files[before]; got.SHA256 != d.Name() {
				return fmt.Errorf("backup: blob %s does not match its digest; the store is corrupt", d.Name())
			}
			m.Blobs++
			m.BlobBytes += m.Files[before].Size
			return nil
		})
		if err != nil {
			return Manifest{}, err
		}
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(raw)), Typeflag: tar.TypeReg, ModTime: m.CreatedAt}); err != nil {
		return Manifest{}, err
	}
	if _, err := tw.Write(raw); err != nil {
		return Manifest{}, err
	}
	if err := tw.Close(); err != nil {
		return Manifest{}, err
	}
	return m, enc.Close()
}

// snapshot copies the database consistently into dir and records its tables' checksums from the same snapshot.
func snapshot(ctx context.Context, o Options, dir string, m *Manifest) (string, error) {
	d, err := db.Open(ctx, o.DSN)
	if err != nil {
		return "", err
	}
	defer d.Close()
	m.Dialect = d.Dialect
	if d.Dialect == db.SQLite {
		out := filepath.Join(dir, "samuori.db")
		if _, err := d.SQL.ExecContext(ctx, `VACUUM INTO ?`, out); err != nil {
			return "", fmt.Errorf("backup: copying the SQLite database: %w", err)
		}
		cp, err := db.Open(ctx, "sqlite://"+out)
		if err != nil {
			return "", err
		}
		defer cp.Close()
		if m.SchemaVersion, err = cp.SchemaVersion(ctx); err != nil {
			return "", err
		}
		if m.Tables, err = checksums(ctx, cp, cp.SQL); err != nil {
			return "", err
		}
		m.Database = "database/samuori.db"
		return out, nil
	}
	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var snap string
	if err := tx.QueryRowContext(ctx, `SELECT pg_export_snapshot()`).Scan(&snap); err != nil {
		return "", err
	}
	out := filepath.Join(dir, "postgres.dump")
	if err := pgTool(ctx, o.tool("pg_dump", o.PGDump), o.DSN, "--format=custom", "--no-owner", "--snapshot="+snap, "--file="+out); err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&m.SchemaVersion); err != nil {
		return "", err
	}
	if m.Tables, err = checksums(ctx, d, tx); err != nil {
		return "", err
	}
	m.Database = "database/postgres.dump"
	return out, nil
}

func checksums(ctx context.Context, d *db.DB, q db.Queryer) ([]db.TableReport, error) {
	tables, err := d.Tables(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]db.TableReport, 0, len(tables))
	for _, t := range tables {
		sum, n, err := db.TableChecksum(ctx, q, t)
		if err != nil {
			return nil, err
		}
		out = append(out, db.TableReport{Name: t.Name, Rows: n, Checksum: sum})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// pgTool runs pg_dump or pg_restore against dsn. The password travels in the environment, never on the command line.
func pgTool(ctx context.Context, bin, dsn string, args ...string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return errors.New("backup: the PostgreSQL DSN is not a URL")
	}
	env := os.Environ()
	if u.User != nil {
		if pw, ok := u.User.Password(); ok {
			env = append(env, "PGPASSWORD="+pw)
			u.User = url.User(u.User.Username())
		}
	}
	cmd := exec.CommandContext(ctx, bin, append(args, "--dbname="+u.String())...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return fmt.Errorf("backup: %s failed: %v: %s", filepath.Base(bin), err, msg)
	}
	return nil
}
