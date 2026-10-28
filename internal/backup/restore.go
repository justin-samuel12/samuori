package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"samuori.invalid/samuori/internal/db"
)

// ErrCorrupt means an archive does not match its manifest, or the restored data does not match the checksums taken
// when the backup was made.
var ErrCorrupt = errors.New("backup: the archive is damaged or does not match its manifest")

// ErrTargetInUse means the restore target already holds data.
var ErrTargetInUse = errors.New("backup: the restore target is not empty")

// RestoreOptions name an empty data directory and an empty database to restore into.
type RestoreOptions struct {
	Identities []age.Identity
	DataDir    string
	DSN        string
	// KeyFile receives the secret store key; by default dataDir/keys/secrets.key.
	KeyFile   string
	PGRestore string
	Temp      string
}

type Result struct {
	Manifest Manifest
	// Migrated is the number of migrations applied after the restore, when this binary is newer than the backup.
	Migrated int
}

// extract decrypts the archive into dir, checking every entry against the manifest that ends it.
func extract(r io.Reader, ids []age.Identity, dir string) (Manifest, error) {
	dec, err := age.Decrypt(r, ids...)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: decrypting: %w", err)
	}
	tr := tar.NewReader(dec)
	got := map[string]File{}
	var m Manifest
	seenManifest := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if seenManifest {
			return Manifest{}, fmt.Errorf("%w: entries after the manifest", ErrCorrupt)
		}
		if h.Typeflag != tar.TypeReg || !validName(h.Name) {
			return Manifest{}, fmt.Errorf("%w: unexpected entry %q", ErrCorrupt, h.Name)
		}
		if h.Name == "manifest.json" {
			raw, err := io.ReadAll(io.LimitReader(tr, 64<<20))
			if err != nil {
				return Manifest{}, err
			}
			if err := json.Unmarshal(raw, &m); err != nil || m.Format != Format {
				return Manifest{}, fmt.Errorf("%w: the manifest is not a %s manifest", ErrCorrupt, Format)
			}
			seenManifest = true
			continue
		}
		if _, dup := got[h.Name]; dup {
			return Manifest{}, fmt.Errorf("%w: %s appears twice", ErrCorrupt, h.Name)
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return Manifest{}, err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return Manifest{}, err
		}
		sum := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, sum), tr)
		f.Close()
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		got[h.Name] = File{Path: h.Name, SHA256: hex.EncodeToString(sum.Sum(nil)), Size: n}
	}
	if !seenManifest {
		return Manifest{}, fmt.Errorf("%w: no manifest", ErrCorrupt)
	}
	if len(got) != len(m.Files) {
		return Manifest{}, fmt.Errorf("%w: %d entries, the manifest lists %d", ErrCorrupt, len(got), len(m.Files))
	}
	for _, f := range m.Files {
		if g, ok := got[f.Path]; !ok || g != f {
			return Manifest{}, fmt.Errorf("%w: %s", ErrCorrupt, f.Path)
		}
	}
	return m, nil
}

func validName(name string) bool {
	if name == "manifest.json" || name == "database/samuori.db" || name == "database/postgres.dump" || name == "material/keys/secrets.key" {
		return true
	}
	for _, prefix := range []string{"material/pki/", "blobs/sha256/"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" && !strings.ContainsAny(rest, "/\\") && rest != "." && rest != ".." {
			return true
		}
	}
	return false
}

// Verify decrypts an archive and checks every entry. For a SQLite backup it also recomputes every table checksum.
func Verify(ctx context.Context, r io.Reader, ids []age.Identity, temp string) (Manifest, error) {
	dir, err := os.MkdirTemp(temp, "samuori-verify-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(dir)
	m, err := extract(r, ids, dir)
	if err != nil {
		return Manifest{}, err
	}
	if m.Dialect == db.SQLite {
		d, err := db.Open(ctx, "sqlite://"+filepath.Join(dir, "database", "samuori.db"))
		if err != nil {
			return Manifest{}, err
		}
		defer d.Close()
		if err := matchTables(ctx, d, m); err != nil {
			return Manifest{}, err
		}
	}
	return m, nil
}

func matchTables(ctx context.Context, d *db.DB, m Manifest) error {
	got, err := checksums(ctx, d, d.SQL)
	if err != nil {
		return err
	}
	want := map[string]db.TableReport{}
	for _, t := range m.Tables {
		want[t.Name] = t
	}
	if len(got) != len(want) {
		return fmt.Errorf("%w: %d tables restored, the backup had %d", ErrCorrupt, len(got), len(want))
	}
	for _, t := range got {
		if want[t.Name] != t {
			return fmt.Errorf("%w: table %s (%d rows) does not match the backup (%d rows)", ErrCorrupt, t.Name, t.Rows, want[t.Name].Rows)
		}
	}
	return nil
}

// Restore fills an empty data directory and an empty database from an archive. The database is checked against
// the backup's table checksums before any migration of this newer binary runs. A SQLite backup can be restored into
// SQLite or PostgreSQL; a PostgreSQL backup only into PostgreSQL.
func Restore(ctx context.Context, r io.Reader, o RestoreOptions) (Result, error) {
	if entries, err := os.ReadDir(o.DataDir); err == nil && len(entries) > 0 {
		return Result{}, fmt.Errorf("%w: %s holds files", ErrTargetInUse, o.DataDir)
	}
	dialect, target, err := db.ParseDSN(o.DSN)
	if err != nil {
		return Result{}, err
	}
	tmp, err := os.MkdirTemp(o.Temp, "samuori-restore-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(tmp)
	m, err := extract(r, o.Identities, tmp)
	if err != nil {
		return Result{}, err
	}
	res := Result{Manifest: m}
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return Result{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(filepath.Join(o.DataDir, "pki"))
			_ = os.RemoveAll(filepath.Join(o.DataDir, "keys"))
			_ = os.RemoveAll(filepath.Join(o.DataDir, "blobs"))
		}
	}()
	switch {
	case m.Dialect == db.SQLite && dialect == db.SQLite:
		res.Migrated, err = restoreSQLite(ctx, tmp, o.DSN, target, m)
	case m.Dialect == db.SQLite && dialect == db.Postgres:
		res.Migrated, err = restoreSQLiteIntoPostgres(ctx, tmp, o.DSN, m)
	case m.Dialect == db.Postgres && dialect == db.Postgres:
		res.Migrated, err = restorePostgres(ctx, tmp, o, m)
	default:
		err = errors.New("backup: a PostgreSQL backup restores only into PostgreSQL")
	}
	if err != nil {
		return Result{}, err
	}
	if err := placeFiles(tmp, o, m); err != nil {
		return Result{}, err
	}
	ok = true
	return res, nil
}

func restoreSQLite(ctx context.Context, tmp, dsn, target string, m Manifest) (int, error) {
	path := strings.TrimPrefix(strings.SplitN(target, "?", 2)[0], "file:")
	if _, err := os.Stat(path); err == nil {
		return 0, fmt.Errorf("%w: %s exists", ErrTargetInUse, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	if err := copyFile(filepath.Join(tmp, "database", "samuori.db"), path, 0o600); err != nil {
		return 0, err
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		return 0, err
	}
	defer d.Close()
	if err := matchTables(ctx, d, m); err != nil {
		d.Close()
		_ = os.Remove(path)
		return 0, err
	}
	return d.Migrate(ctx)
}

func restoreSQLiteIntoPostgres(ctx context.Context, tmp, dsn string, m Manifest) (int, error) {
	src, err := db.Open(ctx, "sqlite://"+filepath.Join(tmp, "database", "samuori.db"))
	if err != nil {
		return 0, err
	}
	defer src.Close()
	if err := matchTables(ctx, src, m); err != nil {
		return 0, err
	}
	n, err := src.Migrate(ctx)
	if err != nil {
		return 0, err
	}
	dst, err := db.Open(ctx, dsn)
	if err != nil {
		return 0, err
	}
	defer dst.Close()
	if _, err := dst.Migrate(ctx); err != nil {
		return 0, err
	}
	if _, err := db.Transfer(ctx, src, dst, nil); err != nil {
		return 0, err
	}
	return n, nil
}

func restorePostgres(ctx context.Context, tmp string, o RestoreOptions, m Manifest) (int, error) {
	d, err := db.Open(ctx, o.DSN)
	if err != nil {
		return 0, err
	}
	defer d.Close()
	var tables int
	if err := d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema()`).Scan(&tables); err != nil {
		return 0, err
	}
	if tables > 0 {
		return 0, fmt.Errorf("%w: the database already has %d tables", ErrTargetInUse, tables)
	}
	bin := o.PGRestore
	if bin == "" {
		bin = "pg_restore"
	}
	if err := pgTool(ctx, bin, o.DSN, "--no-owner", "--single-transaction", "--exit-on-error", filepath.Join(tmp, "database", "postgres.dump")); err != nil {
		return 0, err
	}
	if err := matchTables(ctx, d, m); err != nil {
		return 0, err
	}
	return d.Migrate(ctx)
}

func placeFiles(tmp string, o RestoreOptions, m Manifest) error {
	for _, f := range m.Files {
		src := filepath.Join(tmp, filepath.FromSlash(f.Path))
		switch {
		case strings.HasPrefix(f.Path, "material/pki/"):
			mode := os.FileMode(0o600)
			if strings.HasSuffix(f.Path, ".crt") {
				mode = 0o644
			}
			if err := placeIn(src, filepath.Join(o.DataDir, "pki", filepath.Base(f.Path)), mode); err != nil {
				return err
			}
		case f.Path == "material/keys/secrets.key":
			dst := o.KeyFile
			if dst == "" {
				dst = filepath.Join(o.DataDir, "keys", "secrets.key")
			}
			if _, err := os.Stat(dst); err == nil {
				return fmt.Errorf("%w: %s exists", ErrTargetInUse, dst)
			}
			if err := placeIn(src, dst, 0o600); err != nil {
				return err
			}
		case strings.HasPrefix(f.Path, "blobs/sha256/"):
			hexd := filepath.Base(f.Path)
			if err := placeIn(src, filepath.Join(o.DataDir, "blobs", "sha256", hexd[:2], hexd), 0o440); err != nil {
				return err
			}
		}
	}
	return nil
}

func placeIn(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return copyFile(src, dst, mode)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
