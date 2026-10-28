package conversion

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/storage"
)

func openSQLite(t *testing.T, migrate bool) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if migrate {
		if _, err := d.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// seed writes rows whose text keys sort differently under byte order and a linguistic collation, a timestamp with
// microseconds, non-ASCII text and a cluster_material row that must not be copied.
func seed(t *testing.T, d *db.DB) {
	t.Helper()
	at := time.Date(2026, 10, 23, 9, 30, 1, 123456000, time.UTC)
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO organizations (id, name, created_at) VALUES (?, ?, ?)`, []any{"org-1", "Ünïcode Org", at}},
		{`INSERT INTO teams (id, org_id, name, created_at) VALUES (?, ?, ?, ?)`, []any{"team-1", "org-1", "platform", at}},
		{`INSERT INTO controllers (id, hostname, version, started_at, heartbeat_at, stopped_at) VALUES (?, ?, ?, ?, ?, ?)`, []any{"c1", "ctrl1.example", "1.0.0", at, at, at}},
		{`INSERT INTO cluster_material (name, revision, key_id, ciphertext, updated_at, updated_by) VALUES (?, ?, ?, ?, ?, ?)`, []any{"cluster-pki", 3, "k", []byte{1, 2, 3}, at, "c1"}},
	}
	for _, id := range []string{"a.b", "aB", "a_b", "A-1", "ab", "a b"} {
		stmts = append(stmts, struct {
			q    string
			args []any
		}{`INSERT INTO audit_events (id, at, org_id, actor_type, action, resource_type, outcome, reason) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{id, at, "org-1", "system", "test.seed", "test", "succeeded", "reason " + id}})
	}
	for _, s := range stmts {
		if _, err := d.SQL.Exec(d.Rebind(s.q), s.args...); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
}

func counts(t *testing.T, d *db.DB) map[string]int64 {
	t.Helper()
	reports, err := d.RowCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, r := range reports {
		out[r.Name] = r.Rows
	}
	return out
}

func TestTargetMustBeEmptyAtTheSameVersion(t *testing.T) {
	ctx := context.Background()
	src := openSQLite(t, true)
	seed(t, src)
	version, err := CheckSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}

	full := openSQLite(t, true)
	if _, err := full.SQL.Exec(`INSERT INTO organizations (id, name, created_at) VALUES ('o', 'other', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err := PrepareTarget(ctx, full, version, true); !errors.Is(err, db.ErrNotEmpty) {
		t.Errorf("a non-empty target was accepted: %v", err)
	}
	if _, err := CopyDatabase(ctx, src, full, nil); !errors.Is(err, db.ErrNotEmpty) {
		t.Errorf("a copy into a non-empty target ran: %v", err)
	}
	if got := counts(t, full)["organizations"]; got != 1 {
		t.Errorf("the refused target changed: %d organizations", got)
	}

	foreign := openSQLite(t, false)
	if _, err := foreign.SQL.Exec(`CREATE TABLE unrelated (id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := PrepareTarget(ctx, foreign, version, true); !errors.Is(err, db.ErrNotEmpty) {
		t.Errorf("a database holding other tables was accepted: %v", err)
	}

	older := openSQLite(t, false)
	if err := older.MigrateTo(ctx, version-1); err != nil {
		t.Fatal(err)
	}
	if err := PrepareTarget(ctx, older, version, true); !errors.Is(err, db.ErrVersionMismatch) {
		t.Errorf("a target at another schema version was accepted: %v", err)
	}
	if _, err := CopyDatabase(ctx, src, older, nil); !errors.Is(err, db.ErrVersionMismatch) {
		t.Errorf("a copy across schema versions ran: %v", err)
	}
	if _, err := CheckSource(ctx, older); !errors.Is(err, ErrSchemaBehind) {
		t.Errorf("a source behind this release was accepted: %v", err)
	}

	fresh := openSQLite(t, false)
	if err := PrepareTarget(ctx, fresh, version, false); err != nil {
		t.Fatalf("a fresh target was refused: %v", err)
	}
	if v, _ := fresh.SchemaVersion(ctx); v != 0 {
		t.Errorf("a dry check migrated the target to %d", v)
	}
}

func TestSingleServerNeedsOneController(t *testing.T) {
	ctx := context.Background()
	d := openSQLite(t, true)
	now := time.Now().UTC()
	insert := func(id, host string) {
		if _, err := d.SQL.Exec(`INSERT INTO controllers (id, hostname, version, started_at, heartbeat_at, stopped_at) VALUES (?, ?, '1', ?, ?, ?)`, id, host, now, now, now); err != nil {
			t.Fatal(err)
		}
	}
	insert("a1", "ctrl1")
	insert("a2", "ctrl1")
	if err := SoleController(ctx, d, "ctrl1"); err != nil {
		t.Fatalf("one controller host with two process rows was refused: %v", err)
	}
	if err := SoleController(ctx, d, "ctrl2"); err == nil || !strings.Contains(err.Error(), "ctrl1") {
		t.Errorf("conversion on a host that is not the remaining controller: %v", err)
	}
	insert("b1", "ctrl2")
	err := SoleController(ctx, d, "ctrl1")
	if err == nil || !strings.Contains(err.Error(), "remove ctrl2") {
		t.Fatalf("two controllers were accepted: %v", err)
	}
	if running, err := Running(ctx, d); err != nil || len(running) != 0 {
		t.Errorf("stopped controllers reported running: %v, %v", running, err)
	}
}

// TestRoundTripThroughPostgres needs SAMUORI_TEST_POSTGRES_ADMIN_DSN: a superuser DSN for a disposable server.
func TestRoundTripThroughPostgres(t *testing.T) {
	adminDSN := os.Getenv("SAMUORI_TEST_POSTGRES_ADMIN_DSN")
	if adminDSN == "" {
		t.Skip("set SAMUORI_TEST_POSTGRES_ADMIN_DSN to a disposable PostgreSQL superuser DSN")
	}
	ctx := context.Background()
	su, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer su.Close(ctx)
	for _, q := range []string{`DROP DATABASE IF EXISTS samuori_convert_test`, `CREATE DATABASE samuori_convert_test`} {
		if _, err := su.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	cc, _ := pgx.ParseConfig(adminDSN)
	u := "postgres://" + cc.User
	if cc.Password != "" {
		u += ":" + cc.Password
	}
	pg, err := db.Open(ctx, u+"@"+cc.Host+":"+strconv.Itoa(int(cc.Port))+"/samuori_convert_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	src := openSQLite(t, true)
	seed(t, src)
	version, err := CheckSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareTarget(ctx, pg, version, true); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyDatabase(ctx, src, pg, nil); err != nil {
		t.Fatalf("SQLite to PostgreSQL: %v", err)
	}
	if err := PrepareTarget(ctx, pg, version, true); !errors.Is(err, db.ErrNotEmpty) {
		t.Errorf("the filled PostgreSQL database was accepted as a target: %v", err)
	}

	back := openSQLite(t, false)
	if err := PrepareTarget(ctx, back, version, true); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyDatabase(ctx, pg, back, nil); err != nil {
		t.Fatalf("PostgreSQL to SQLite: %v", err)
	}

	want, mid, got := counts(t, src), counts(t, pg), counts(t, back)
	for table, n := range want {
		if table == "cluster_material" {
			if mid[table] != 0 || got[table] != 0 {
				t.Errorf("cluster_material was copied: %d, %d rows", mid[table], got[table])
			}
			continue
		}
		if mid[table] != n || got[table] != n {
			t.Errorf("%s: %d rows in SQLite, %d in PostgreSQL, %d after the round trip", table, n, mid[table], got[table])
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d tables after the round trip, %d before", len(got), len(want))
	}
	var name string
	var created time.Time
	if err := back.SQL.QueryRow(`SELECT name, created_at FROM organizations WHERE id = 'org-1'`).Scan(&name, &created); err != nil {
		t.Fatal(err)
	}
	if name != "Ünïcode Org" || !created.Equal(time.Date(2026, 10, 23, 9, 30, 1, 123456000, time.UTC)) {
		t.Errorf("round trip changed a row: %q %v", name, created)
	}
}

type memRemote struct{ objects map[string][]byte }

func (m *memRemote) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	m.objects[key] = b
	return err
}
func (m *memRemote) Get(_ context.Context, key string, w io.Writer) error {
	b, ok := m.objects[key]
	if !ok {
		return storage.ErrRemoteNotFound
	}
	_, err := w.Write(b)
	return err
}
func (m *memRemote) Exists(_ context.Context, key string) (bool, error) {
	_, ok := m.objects[key]
	return ok, nil
}
func (m *memRemote) Delete(_ context.Context, key string) error { delete(m.objects, key); return nil }
func (m *memRemote) List(_ context.Context, fn func(string, time.Time, int64) error) error {
	for k, v := range m.objects {
		if err := fn(k, time.Time{}, int64(len(v))); err != nil {
			return err
		}
	}
	return nil
}
func (m *memRemote) Describe() string            { return "memory" }
func (m *memRemote) Check(context.Context) error { return nil }

func TestBlobCopiesAreCompared(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	local, err := storage.NewBlobs(root)
	if err != nil {
		t.Fatal(err)
	}
	digest, _, err := local.Put(bytes.NewReader([]byte("layer one")), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := local.Put(bytes.NewReader([]byte("layer two")), 1<<20); err != nil {
		t.Fatal(err)
	}
	remote := &memRemote{objects: map[string][]byte{}}
	own := func(string) error { return nil }
	pushed, err := PushBlobs(ctx, root, remote, nil)
	if err != nil || pushed.Blobs != 2 || pushed.Copied != 2 {
		t.Fatalf("push = %+v, %v", pushed, err)
	}
	other := t.TempDir()
	pulled, err := PullBlobs(ctx, remote, other, own, nil)
	if err != nil || pulled.Blobs != 2 || pulled.Copied != 2 {
		t.Fatalf("pull = %+v, %v", pulled, err)
	}

	key := "sha256/" + strings.TrimPrefix(digest, "sha256:")
	remote.objects[key] = []byte("tampered")
	if _, err := PushBlobs(ctx, root, remote, nil); err == nil {
		t.Error("a remote object with other content under a local blob's digest was accepted")
	}
	if _, err := PullBlobs(ctx, remote, t.TempDir(), own, nil); err == nil {
		t.Error("a remote object that does not match its digest was downloaded")
	}

	if err := os.Chmod(filepath.Join(other, "sha256", key[7:9], key[7:]), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "sha256", key[7:9], key[7:]), []byte("corrupt"), 0o440); err != nil {
		t.Fatal(err)
	}
	if _, err := PushBlobs(ctx, other, &memRemote{objects: map[string][]byte{}}, nil); err == nil {
		t.Error("a corrupt local blob was uploaded")
	}
}
