package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// TableReport is one table of a transfer or backup: its rows and the checksum of their normalised values.
type TableReport struct {
	Name     string `json:"name"`
	Rows     int64  `json:"rows"`
	Checksum string `json:"checksum"`
}

// ErrTargetNotEmpty means the PostgreSQL database already holds data; a transfer only fills a fresh database.
var ErrTargetNotEmpty = ErrNotEmpty

// ErrVersionMismatch means the two databases are at different schema versions.
var ErrVersionMismatch = errors.New("the databases are at different schema versions")

// Transfer copies every table of a SQLite database into a freshly migrated, empty PostgreSQL database at the same
// schema version (ADR 0017, D07); see Copy.
func Transfer(ctx context.Context, src, dst *DB, progress func(table string, rows int64)) ([]TableReport, error) {
	if src.Dialect != SQLite || dst.Dialect != Postgres {
		return nil, errors.New("database: a transfer copies SQLite into PostgreSQL")
	}
	return Copy(ctx, src, dst, nil, progress)
}

// Copy copies every table but those in skip from src into an empty dst at the same schema version, in either
// direction, in one transaction. Before committing it reads every copied table back from both sides in byte order of
// the key and compares row counts and checksums of normalised values; any difference rolls the whole copy back,
// leaving the target empty. The source is only read. Timestamps keep microsecond precision, as PostgreSQL stores
// them.
func Copy(ctx context.Context, src, dst *DB, skip []string, progress func(table string, rows int64)) ([]TableReport, error) {
	sv, err := src.SchemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	dv, err := dst.SchemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	if sv != dv {
		return nil, fmt.Errorf("%w: source %d, target %d", ErrVersionMismatch, sv, dv)
	}
	tables, err := dst.Tables(ctx)
	if err != nil {
		return nil, err
	}
	if err := sameTables(ctx, src, tables); err != nil {
		return nil, err
	}
	var reports []TableReport
	err = dst.InTx(ctx, func(tx *sql.Tx) error {
		if err := RequireEmpty(ctx, tx, tables); err != nil {
			return err
		}
		for _, t := range tables {
			if slices.Contains(skip, t.Name) {
				continue
			}
			w := dst.NewWriter(tx, t)
			if err := readBytewise(ctx, src, src.SQL, t, func(row []any) error { return w.Add(ctx, row) }); err != nil {
				return fmt.Errorf("database: copying %s: %w", t.Name, err)
			}
			n, err := w.Close(ctx)
			if err != nil {
				return fmt.Errorf("database: copying %s: %w", t.Name, err)
			}
			if progress != nil {
				progress(t.Name, n)
			}
		}
		for _, t := range tables {
			if slices.Contains(skip, t.Name) {
				continue
			}
			want, wantN, err := bytewiseChecksum(ctx, src, src.SQL, t)
			if err != nil {
				return err
			}
			got, gotN, err := bytewiseChecksum(ctx, dst, tx, t)
			if err != nil {
				return err
			}
			if want != got || wantN != gotN {
				return fmt.Errorf("database: %s differs after the copy (%d rows, checksum %.12s; target %d rows, checksum %.12s)", t.Name, wantN, want, gotN, got)
			}
			reports = append(reports, TableReport{Name: t.Name, Rows: gotN, Checksum: got})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reports, nil
}

// RowCounts counts the rows of every application table.
func (d *DB) RowCounts(ctx context.Context) ([]TableReport, error) {
	tables, err := d.Tables(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TableReport, 0, len(tables))
	for _, t := range tables {
		var n int64
		if err := d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t.Name).Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, TableReport{Name: t.Name, Rows: n})
	}
	return out, nil
}

func sameTables(ctx context.Context, src *DB, dstTables []Table) error {
	srcTables, err := src.Tables(ctx)
	if err != nil {
		return err
	}
	names := func(ts []Table) []string {
		out := make([]string, len(ts))
		for i, t := range ts {
			out[i] = t.Name
		}
		slices.Sort(out)
		return out
	}
	if a, b := names(srcTables), names(dstTables); !slices.Equal(a, b) {
		return fmt.Errorf("database: the source and target hold different tables (%d and %d) at the same schema version", len(a), len(b))
	}
	return nil
}

// readBytewise is ReadTable ordered by the byte values of text keys on both dialects, so checksums of the same rows
// agree whatever collation the PostgreSQL database uses.
func readBytewise(ctx context.Context, d *DB, q Queryer, t Table, fn func(row []any) error) error {
	order := make([]string, len(t.Key))
	for i, k := range t.Key {
		order[i] = k
		if d.Dialect == Postgres && t.kind(k) == KindText {
			order[i] = k + ` COLLATE "C"`
		}
	}
	ordered := t
	ordered.Key = []string{strings.Join(order, ", ")}
	return ReadTable(ctx, q, ordered, fn)
}

func bytewiseChecksum(ctx context.Context, d *DB, q Queryer, t Table) (string, int64, error) {
	h := NewRowHasher()
	err := readBytewise(ctx, d, q, t, func(row []any) error {
		h.Add(row)
		return nil
	})
	return h.Sum(), h.Rows, err
}

func (t Table) kind(column string) string {
	for _, c := range t.Columns {
		if c.Name == column {
			return c.Kind
		}
	}
	return KindText
}
