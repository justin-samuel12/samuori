// Package conversion moves an installation between a single server (SQLite, local blobs) and the first controller
// of a cluster (PostgreSQL, shared blob storage) during a maintenance window (ADR 0046,
// docs/operators/high-availability.md#convert-between-a-single-server-and-a-cluster). The source is only read.
package conversion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/db"
)

// SkipTables are not copied: cluster_material holds controller material sealed with the cluster key, which each
// side keeps in its own data directory and a cluster's first controller shares again on its first start. A stale
// copy would replace the new cluster CA.
var SkipTables = []string{"cluster_material"}

var ErrSchemaBehind = errors.New("the source database is not at this release's schema version")

// CheckSource returns the source's schema version, which must be the newest this release knows, so the source is
// never migrated by a conversion.
func CheckSource(ctx context.Context, src *db.DB) (int64, error) {
	applied, known, err := src.Schema(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the source schema: %w", err)
	}
	if applied != known {
		return 0, fmt.Errorf("%w: it is at %d, this release at %d; run this release's samuori-server on it first, or convert with the release it runs", ErrSchemaBehind, applied, known)
	}
	return applied, nil
}

// PrepareTarget requires an empty target: either never migrated and holding no tables, or migrated to version and
// holding no rows. With migrate it then brings a fresh target to version.
func PrepareTarget(ctx context.Context, dst *db.DB, version int64, migrate bool) error {
	applied, known, err := dst.Schema(ctx)
	if err != nil {
		return fmt.Errorf("reading the target schema: %w", err)
	}
	if applied == 0 {
		n, err := userTables(ctx, dst)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: it holds %d tables but no Samuori schema; use a new, empty database", db.ErrNotEmpty, n)
		}
		if known != version {
			return fmt.Errorf("%w: source %d, this release migrates to %d", db.ErrVersionMismatch, version, known)
		}
		if !migrate {
			return nil
		}
		if _, err := dst.Migrate(ctx); err != nil {
			return err
		}
		if applied, _, err = dst.Schema(ctx); err != nil {
			return err
		}
	}
	if applied != version {
		return fmt.Errorf("%w: source %d, target %d", db.ErrVersionMismatch, version, applied)
	}
	tables, err := dst.Tables(ctx)
	if err != nil {
		return err
	}
	return db.RequireEmpty(ctx, dst.SQL, tables)
}

func userTables(ctx context.Context, d *db.DB) (int, error) {
	q := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'goose_db_version'`
	if d.Dialect == db.Postgres {
		q = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name <> 'goose_db_version'`
	}
	var n int
	err := d.SQL.QueryRowContext(ctx, q).Scan(&n)
	return n, err
}

// CopyDatabase copies and verifies every table but SkipTables; see db.Copy.
func CopyDatabase(ctx context.Context, src, dst *db.DB, progress func(table string, rows int64)) ([]db.TableReport, error) {
	return db.Copy(ctx, src, dst, SkipTables, progress)
}

// Running lists controller hosts that heartbeat recently and have not stopped.
func Running(ctx context.Context, d *db.DB) ([]string, error) {
	return hostnames(ctx, d, `SELECT DISTINCT hostname FROM controllers WHERE stopped_at IS NULL AND heartbeat_at > ?`, time.Now().UTC().Add(-cluster.DeadAfter))
}

// SoleController requires self to be the only controller host registered in d: a cluster converts to a single
// server only after the other controllers were removed.
func SoleController(ctx context.Context, d *db.DB, self string) error {
	hosts, err := hostnames(ctx, d, `SELECT DISTINCT hostname FROM controllers`)
	if err != nil {
		return err
	}
	var others []string
	for _, h := range hosts {
		if h != self {
			others = append(others, h)
		}
	}
	switch {
	case len(others) > 0 && len(hosts) > 1:
		return fmt.Errorf("%d controllers are registered; remove %s under Infrastructure > Servers first, so only this one (%s) remains", len(hosts), strings.Join(others, ", "), self)
	case len(others) > 0:
		return fmt.Errorf("the only registered controller is %s, not this host (%s); run the conversion there", others[0], self)
	case len(hosts) == 0:
		return errors.New("no controller is registered in this database")
	}
	return nil
}

func hostnames(ctx context.Context, d *db.DB, q string, args ...any) ([]string, error) {
	rows, err := d.SQL.QueryContext(ctx, d.Rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// Totals sums table reports.
func Totals(tables []db.TableReport) (n int, rows int64) {
	for _, t := range tables {
		rows += t.Rows
	}
	return len(tables), rows
}

// Audit records the conversion in the target database once its copy is verified.
func Audit(ctx context.Context, dst *db.DB, host, direction, reason string) error {
	return dst.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, dst, tx, audit.Event{Actor: authz.Principal{Kind: authz.System}, Action: "topology.convert",
			ResourceType: "installation", ResourceID: host, Outcome: "succeeded", Reason: direction + ": " + reason})
	})
}
