package dbcredentials

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/db"
)

var (
	quiet = slog.New(slog.NewTextHandler(io.Discard, nil))
	admin = authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	op    = authz.Principal{ID: "op", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Operator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
)

func open(t *testing.T, dsn string) (*db.DB, *cluster.Member) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := cluster.Join(ctx, d, "test", quiet)
	if err != nil {
		t.Fatal(err)
	}
	return d, m
}

func TestPasswordHandling(t *testing.T) {
	t.Run("SEC-DB-02", func(t *testing.T) {
		key := make([]byte, 32)
		dir := t.TempDir()
		d, m := open(t, "sqlite://"+filepath.Join(dir, "samuori.db"))
		s, err := New(d, m, dir, "sqlite://"+filepath.Join(dir, "samuori.db"), key, quiet)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := s.seal(1, "s3cret-value")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(sealed), "s3cret-value") {
			t.Fatal("the sealed password holds the plaintext")
		}
		if _, err := s.open(2, sealed); err == nil {
			t.Error("a sealed password opened under another revision")
		}
		other, _ := New(d, m, dir, "sqlite://x.db", append(make([]byte, 31), 1), quiet)
		if _, err := other.open(1, sealed); err == nil {
			t.Error("a sealed password opened with another cluster key")
		}
		if pw, err := s.open(1, sealed); err != nil || pw != "s3cret-value" {
			t.Errorf("open = %q, %v", pw, err)
		}

		if err := s.Update(context.Background(), op, "pw", "rotated", ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("an operator updated the password: %v", err)
		}
		if _, err := s.Status(context.Background(), op); !errors.Is(err, ErrForbidden) {
			t.Errorf("an operator read the database status: %v", err)
		}
		if err := s.Update(context.Background(), admin, "pw", "rotated", ""); !errors.Is(err, ErrNotPostgres) {
			t.Errorf("SQLite accepted a password: %v", err)
		}

		if err := db.WriteStoredPassword(dir, db.StoredPassword{Revision: 3, Password: "p@ss/word"}); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(filepath.Join(dir, db.PasswordFile)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("stored password file mode = %v, %v", fi.Mode().Perm(), err)
		}
		dsn, err := db.EffectiveDSN("postgres://samuori:old@db.example:5432/samuori?sslmode=verify-full", dir)
		if err != nil {
			t.Fatal(err)
		}
		if cc, err := pgx.ParseConfig(dsn); err != nil || cc.Password != "p@ss/word" || cc.User != "samuori" || cc.Host != "db.example" {
			t.Errorf("effective DSN = %v, %v", cc, err)
		}
		if got := db.Describe(dsn); got.User != "samuori" || got.Database != "samuori" || got.TLS != "verify-full" {
			t.Errorf("describe = %+v", got)
		}
	})
}

// TestRolloutAgainstPostgres needs SAMUORI_TEST_POSTGRES_ADMIN_DSN: a superuser DSN for a disposable server.
func TestRolloutAgainstPostgres(t *testing.T) {
	t.Run("SEC-DB-03", func(t *testing.T) {
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
		for _, q := range []string{`DROP DATABASE IF EXISTS samuori_creds_test`, `DROP ROLE IF EXISTS samuori_creds_test`,
			`CREATE ROLE samuori_creds_test LOGIN PASSWORD 'first-password'`, `CREATE DATABASE samuori_creds_test OWNER samuori_creds_test`} {
			if _, err := su.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		cc, _ := pgx.ParseConfig(adminDSN)
		base := "postgres://samuori_creds_test:first-password@" + cc.Host + ":" + strconv.Itoa(int(cc.Port)) + "/samuori_creds_test?sslmode=disable"

		dirA, dirB := t.TempDir(), t.TempDir()
		dA, mA := open(t, base)
		dB, mB := open(t, base)
		a, _ := New(dA, mA, dirA, base, make([]byte, 32), quiet)
		b, _ := New(dB, mB, dirB, base, make([]byte, 32), quiet)

		if err := a.Update(ctx, admin, "second-password", "rotated at the database", "r1"); err == nil || !strings.HasPrefix(err.Error(), "nothing changed") {
			t.Fatalf("a password the database does not accept was stored: %v", err)
		}
		var n int
		_ = dA.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM database_credentials`).Scan(&n)
		if n != 0 {
			t.Fatal("a refused password left a revision")
		}

		if _, err := su.Exec(ctx, `ALTER ROLE samuori_creds_test PASSWORD 'second-password'`); err != nil {
			t.Fatal(err)
		}
		if err := a.Update(ctx, admin, "second-password", "rotated at the database", "r2"); err != nil {
			t.Fatal(err)
		}
		if sp, ok, _ := db.ReadStoredPassword(dirA); !ok || sp.Password != "second-password" || sp.Revision != 1 {
			t.Fatalf("controller A stored %+v", sp)
		}
		var sealed []byte
		_ = dA.SQL.QueryRowContext(ctx, `SELECT sealed FROM database_credentials WHERE revision = 1`).Scan(&sealed)
		if strings.Contains(string(sealed), "second-password") {
			t.Fatal("the database holds the password in plaintext")
		}
		var reason string
		_ = dA.SQL.QueryRowContext(ctx, `SELECT reason FROM audit_events WHERE action = 'database.password.update'`).Scan(&reason)
		if reason != "rotated at the database" {
			t.Errorf("audit reason = %q", reason)
		}

		b.sync(ctx)
		if sp, ok, _ := db.ReadStoredPassword(dirB); !ok || sp.Password != "second-password" {
			t.Fatalf("controller B did not switch: %+v", sp)
		}
		dB.SQL.SetMaxIdleConns(0)
		dB.SQL.SetMaxIdleConns(2)
		if err := dB.Ping(ctx); err != nil {
			t.Fatalf("controller B cannot open new connections after switching: %v", err)
		}
		st, err := a.Status(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range st.Controllers {
			if c.Status != Verified {
				t.Errorf("controller %s = %s %s", c.Hostname, c.Status, c.Detail)
			}
		}

		if _, err := su.Exec(ctx, `ALTER ROLE samuori_creds_test PASSWORD 'third-password'`); err != nil {
			t.Fatal(err)
		}
		if _, err := dA.SQL.ExecContext(ctx, `INSERT INTO database_credentials (revision, sealed, reason, created_by, created_at) VALUES ($1, $2, 'wrong', 'admin', $3)`,
			2, must(a.seal(2, "not-the-password")), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		b.sync(ctx)
		if sp, _, _ := db.ReadStoredPassword(dirB); sp.Password != "second-password" {
			t.Error("controller B switched to a password the database refuses")
		}
		var status string
		_ = dA.SQL.QueryRowContext(ctx, `SELECT status FROM database_credential_checks WHERE revision = 2 AND controller_id = $1`, mB.ID).Scan(&status)
		if status != Failed {
			t.Errorf("the refused revision was recorded as %q", status)
		}
		if _, err := su.Exec(ctx, `ALTER ROLE samuori_creds_test PASSWORD 'second-password'`); err != nil {
			t.Fatal(err)
		}
	})
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
