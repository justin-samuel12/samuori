// Package dbtest gives each test its own fully migrated SQLite database. The first call in a test binary runs every
// migration into a template; each call writes a private copy of it (docs/developers/testing.md#fast-setup).
package dbtest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"samuori.invalid/samuori/internal/db"
)

var template = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "samuori-dbtest-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "template.db")
	ctx := context.Background()
	d, err := db.Open(ctx, "sqlite://"+path)
	if err != nil {
		return nil, err
	}
	_, err = d.Migrate(ctx)
	if err == nil {
		_, err = d.SQL.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	}
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + side); !errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("dbtest: the template's " + side + " file outlived its close")
		}
	}
	return os.ReadFile(path)
})

// SQLite writes a migrated database to dir/samuori.db and returns its DSN. It refuses to replace an existing
// database, so a test that reopens one keeps its data.
func SQLite(t testing.TB, dir string) string {
	t.Helper()
	data, err := template()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "samuori.db")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return "sqlite://" + path
}
