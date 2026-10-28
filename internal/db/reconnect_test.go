package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestLostConnectionClassification(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	cases := []struct {
		name   string
		ctx    context.Context
		err    error
		closed bool
		want   bool
	}{
		{"terminated backend", ctx, &pgconn.PgError{Severity: "FATAL", Code: "57P01"}, true, true},
		{"crash shutdown", ctx, fmt.Errorf("query: %w", &pgconn.PgError{Code: "57P02"}), true, true},
		{"server starting", ctx, &pgconn.PgError{Code: "57P03"}, false, true},
		{"connection exception", ctx, &pgconn.PgError{Code: "08006"}, true, true},
		{"socket gone", ctx, fmt.Errorf("receive message: %w", io.ErrUnexpectedEOF), true, true},
		{"unique violation", ctx, &pgconn.PgError{Code: "23505"}, false, false},
		{"syntax error on an open connection", ctx, errors.New("syntax error"), false, false},
		{"cancelled request", cancelled, io.ErrUnexpectedEOF, true, false},
		{"deadline", ctx, fmt.Errorf("q: %w", context.DeadlineExceeded), true, false},
	}
	for _, c := range cases {
		if got := lost(c.ctx, c.err, c.closed); got != c.want {
			t.Errorf("%s: lost = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOnlyPlainSelectsAreRetried(t *testing.T) {
	for q, want := range map[string]bool{
		"SELECT 1":                         true,
		"\n\t select id FROM build_runs":   true,
		"SELECTED":                         false,
		"INSERT INTO t VALUES (1)":         false,
		"UPDATE t SET a = 1":               false,
		"WITH x AS (DELETE FROM t) SELECT": false,
		"DELETE FROM t":                    false,
		"":                                 false,
	} {
		if got := readOnly(q); got != want {
			t.Errorf("readOnly(%q) = %v, want %v", q, got, want)
		}
	}
}

// TestRecoversAfterTerminatedConnections needs SAMUORI_TEST_POSTGRES_ADMIN_DSN: a superuser DSN for a disposable server.
// Terminating the pool's backends is what a PostgreSQL restart or failover does to it.
func TestRecoversAfterTerminatedConnections(t *testing.T) {
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
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("application_name", "samuori-reconnect-test")
	u.RawQuery = q.Encode()
	d, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SQL.SetMaxIdleConns(4)

	warm := func() {
		t.Helper()
		var tx [4]interface{ Rollback() error }
		for i := range tx {
			x, err := d.SQL.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			tx[i] = x
		}
		for _, x := range tx {
			_ = x.Rollback()
		}
	}
	kill := func() {
		t.Helper()
		var n int
		if err := su.QueryRow(ctx, `SELECT COUNT(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE application_name = 'samuori-reconnect-test'`).Scan(&n); err != nil || n == 0 {
			t.Fatalf("terminated %d backends: %v", n, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	warm()
	kill()
	var one int
	if err := d.SQL.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("a read after the backends were terminated failed: %v", err)
	}

	warm()
	kill()
	if err := d.InTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
	}); err != nil {
		t.Fatalf("a transaction after the backends were terminated failed: %v", err)
	}
}

func TestMissingCAFileIsNamed(t *testing.T) {
	_, err := Open(context.Background(), "postgres://samuori:pw@db.example:5432/samuori?sslmode=verify-full&sslrootcert=/nonexistent/rds-ca.pem")
	if err == nil || !strings.Contains(err.Error(), "sslrootcert=/nonexistent/rds-ca.pem") || strings.Contains(err.Error(), "pw") {
		t.Fatalf("error = %v; want it to name the missing CA file and not the password", err)
	}
}
