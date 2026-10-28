package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PasswordFile holds the PostgreSQL password an administrator set on the Database page, which replaces the one in
// the configured DSN (docs/operators/database.md).
const PasswordFile = "database-password"

type StoredPassword struct {
	Revision int64  `json:"revision"`
	Password string `json:"password"`
}

func ReadStoredPassword(dataDir string) (StoredPassword, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, PasswordFile))
	if errors.Is(err, os.ErrNotExist) {
		return StoredPassword{}, false, nil
	}
	if err != nil {
		return StoredPassword{}, false, err
	}
	var sp StoredPassword
	if err := json.Unmarshal(raw, &sp); err != nil || sp.Password == "" {
		return StoredPassword{}, false, errors.New("database: " + PasswordFile + " is not valid")
	}
	return sp, true, nil
}

func WriteStoredPassword(dataDir string, sp StoredPassword) error {
	raw, _ := json.Marshal(sp)
	tmp, err := os.CreateTemp(dataDir, PasswordFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dataDir, PasswordFile))
}

// WithPassword returns a PostgreSQL DSN with its password replaced.
func WithPassword(dsn, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return "", errors.New("database: the PostgreSQL DSN names no user")
	}
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String(), nil
}

// EffectiveDSN applies a password stored in the data directory to a PostgreSQL DSN.
func EffectiveDSN(dsn, dataDir string) (string, error) {
	if dialect, _, err := ParseDSN(dsn); err != nil || dialect != Postgres || dataDir == "" {
		return dsn, nil
	}
	sp, ok, err := ReadStoredPassword(dataDir)
	if err != nil || !ok {
		return dsn, err
	}
	return WithPassword(dsn, sp.Password)
}

// Target describes where a DSN points, without its password.
type Target struct {
	Engine, Host, Port, Database, User, TLS string
}

func Describe(dsn string) Target {
	dialect, _, err := ParseDSN(dsn)
	if err != nil {
		return Target{}
	}
	if dialect == SQLite {
		return Target{Engine: "sqlite"}
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return Target{Engine: "postgresql"}
	}
	t := Target{Engine: "postgresql", Host: u.Hostname(), Port: u.Port(), Database: strings.TrimPrefix(u.Path, "/"), TLS: u.Query().Get("sslmode")}
	if t.Port == "" {
		t.Port = "5432"
	}
	if t.TLS == "" {
		t.TLS = "prefer"
	}
	if u.User != nil {
		t.User = u.User.Username()
	}
	return t
}

type Probe struct {
	User, ServerVersion string
	Latency             time.Duration
}

// Verify signs in with a fresh connection, never a pooled one, so it proves the password works now.
func Verify(ctx context.Context, dsn string) (Probe, error) {
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return Probe{}, errors.New("the PostgreSQL DSN is not valid")
	}
	cc.ConnectTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && (pe.Code == "28P01" || pe.Code == "28000") {
			return Probe{}, errors.New("the database refused the user name or password")
		}
		return Probe{}, fmt.Errorf("the database could not be reached: %w", redact(err, dsn))
	}
	defer conn.Close(context.Background())
	var p Probe
	if err := conn.QueryRow(ctx, `SELECT current_user, current_setting('server_version')`).Scan(&p.User, &p.ServerVersion); err != nil {
		return Probe{}, fmt.Errorf("the database answered but a query failed: %w", redact(err, dsn))
	}
	p.Latency = time.Since(start)
	return p, nil
}
