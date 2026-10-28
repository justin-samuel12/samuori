// Package db opens the metadata database (SQLite or PostgreSQL) and applies migrations (ADR 0017).
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	_ "modernc.org/sqlite"

	"samuori.invalid/samuori/migrations"
)

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

type DB struct {
	SQL     *sql.DB
	Dialect Dialect

	password atomic.Pointer[string]
}

// ParseDSN accepts sqlite:///absolute/path.db, sqlite://relative.db or postgres://...; errors never echo the DSN.
func ParseDSN(dsn string) (Dialect, string, error) {
	switch {
	case strings.HasPrefix(dsn, "sqlite://"):
		path := strings.TrimPrefix(dsn, "sqlite://")
		if path == "" {
			return "", "", errors.New("database: sqlite DSN has no path")
		}
		q := url.Values{}
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "foreign_keys(1)")
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_txlock", "immediate")
		return SQLite, "file:" + filepath.Clean(path) + "?" + q.Encode(), nil
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		if _, err := url.Parse(dsn); err != nil {
			return "", "", errors.New("database: postgres DSN is not a valid URL")
		}
		return Postgres, dsn, nil
	}
	return "", "", errors.New("database: DSN must start with sqlite:// or postgres://")
}

func Open(ctx context.Context, dsn string) (*DB, error) {
	dialect, driverDSN, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	d := &DB{Dialect: dialect}
	var sqlDB *sql.DB
	if dialect == SQLite {
		if sqlDB, err = sql.Open("sqlite", driverDSN); err != nil {
			return nil, fmt.Errorf("database: open %s: %w", dialect, err)
		}
	} else {
		pc, err := pgx.ParseConfig(driverDSN)
		if err != nil {
			if ca := sslRootCert(driverDSN); ca != "" {
				if _, serr := os.Stat(ca); serr != nil {
					return nil, fmt.Errorf("database: the CA file the DSN names (sslrootcert=%s) cannot be read: %v", ca, serr)
				}
			}
			return nil, errors.New("database: the PostgreSQL DSN is not valid")
		}
		// Times come back in UTC, as from SQLite, whatever the server's or this host's time zone.
		sqlDB = sql.OpenDB(reconnecting{stdlib.GetConnector(*pc, stdlib.OptionBeforeConnect(func(_ context.Context, cc *pgx.ConnConfig) error {
			if pw := d.password.Load(); pw != nil {
				cc.Password = *pw
			}
			return nil
		}), stdlib.OptionAfterConnect(func(_ context.Context, c *pgx.Conn) error {
			c.TypeMap().RegisterType(&pgtype.Type{Name: "timestamptz", OID: pgtype.TimestamptzOID, Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC}})
			return nil
		}))})
	}
	if dialect == SQLite {
		sqlDB.SetMaxOpenConns(1)
	} else {
		sqlDB.SetMaxOpenConns(20)
		sqlDB.SetConnMaxIdleTime(5 * time.Minute)
		// Bounded lifetime moves connections to a new primary after a failover that left old ones open.
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("database: connect to %s: %w", dialect, redact(err, dsn))
	}
	d.SQL = sqlDB
	return d, nil
}

// SetPassword makes new PostgreSQL connections use password; open connections keep the one they signed in with.
func (d *DB) SetPassword(password string) { d.password.Store(&password) }

func (d *DB) Close() error { return d.SQL.Close() }

func (d *DB) Ping(ctx context.Context) error { return d.SQL.PingContext(ctx) }

// ErrSchemaNewer means the database was migrated by a newer release than this one; running against it could lose
// data, so the controller refuses to start (D28).
var ErrSchemaNewer = errors.New("database: the schema is newer than this release supports")

func (d *DB) provider() (*goose.Provider, error) {
	sub, err := fs.Sub(migrations.FS, string(d.Dialect))
	if err != nil {
		return nil, err
	}
	gd := goose.DialectPostgres
	var opts []goose.ProviderOption
	if d.Dialect == SQLite {
		gd = goose.DialectSQLite3
	} else {
		// Controllers starting together take turns: only one migrates at a time.
		locker, err := lock.NewPostgresSessionLocker()
		if err != nil {
			return nil, err
		}
		opts = append(opts, goose.WithSessionLocker(locker))
	}
	p, err := goose.NewProvider(gd, d.SQL, sub, opts...)
	if err != nil {
		return nil, fmt.Errorf("database: migrations: %w", err)
	}
	return p, nil
}

// Schema reports the applied schema version and the newest one this release knows.
func (d *DB) Schema(ctx context.Context) (applied, known int64, err error) {
	p, err := d.provider()
	if err != nil {
		return 0, 0, err
	}
	sources := p.ListSources()
	if len(sources) > 0 {
		known = sources[len(sources)-1].Version
	}
	applied, err = p.GetDBVersion(ctx)
	return applied, known, err
}

// Migrate applies all pending migrations for the database's dialect and returns how many ran. It refuses a database
// whose schema is newer than this release knows.
func (d *DB) Migrate(ctx context.Context) (int, error) {
	p, err := d.provider()
	if err != nil {
		return 0, err
	}
	sources := p.ListSources()
	if applied, err := p.GetDBVersion(ctx); err == nil && len(sources) > 0 && applied > sources[len(sources)-1].Version {
		return 0, fmt.Errorf("%w: database schema %d, this release knows up to %d; upgrade Samuori or restore a matching backup", ErrSchemaNewer, applied, sources[len(sources)-1].Version)
	}
	results, err := p.Up(ctx)
	if err != nil {
		return 0, fmt.Errorf("database: migrate: %w", err)
	}
	return len(results), nil
}

// InTx runs fn in a transaction, committing on success and rolling back on error or panic.
func (d *DB) InTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Rebind converts ? placeholders to $n for PostgreSQL.
func (d *DB) Rebind(query string) string {
	if d.Dialect != Postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func redact(err error, dsn string) error {
	msg := err.Error()
	if u, perr := url.Parse(dsn); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "REDACTED")
		}
	}
	return errors.New(msg)
}

// MigrateTo applies migrations up to version, as an older release would have.
func (d *DB) MigrateTo(ctx context.Context, version int64) error {
	p, err := d.provider()
	if err != nil {
		return err
	}
	_, err = p.UpTo(ctx, version)
	return err
}

func sslRootCert(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return u.Query().Get("sslrootcert")
}

// NewestBefore is a keyset condition for a list ordered by created_at DESC, id DESC: the rows after the row whose id
// is the cursor. Bind the cursor three times; an unknown cursor matches nothing.
func NewestBefore(alias, table string) string {
	at := "(SELECT created_at FROM " + table + " WHERE id = ?)"
	return "(" + alias + ".created_at < " + at + " OR (" + alias + ".created_at = " + at + " AND " + alias + ".id < ?))"
}
