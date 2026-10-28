package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Column kinds shared by both dialects, so data can move between them (transfers, backups).
const (
	KindBool  = "bool"
	KindTime  = "time"
	KindBytes = "bytes"
	KindInt   = "int"
	KindFloat = "float"
	KindText  = "text"
)

type Column struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Serial bool   `json:"serial,omitempty"`
}

// Table describes one application table: columns, the key that orders it and self references.
type Table struct {
	Name    string            `json:"name"`
	Columns []Column          `json:"columns"`
	Key     []string          `json:"key"`
	SelfRef map[string]string `json:"selfRef,omitempty"`
}

func (t Table) columnList() string {
	names := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// SchemaVersion is the newest applied migration.
func (d *DB) SchemaVersion(ctx context.Context) (int64, error) {
	var v int64
	err := d.SQL.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("database: reading the schema version: %w", err)
	}
	return v, nil
}

// Tables lists the application tables in an order that satisfies foreign keys. A table without a primary key is
// ordered by all its columns.
func (d *DB) Tables(ctx context.Context) ([]Table, error) {
	var byName map[string]*Table
	var names []string
	var deps map[string][]string
	var err error
	if d.Dialect == SQLite {
		byName, names, deps, err = sqliteSchemaOf(ctx, d)
	} else {
		byName, names, deps, err = postgresSchemaOf(ctx, d)
	}
	if err != nil {
		return nil, err
	}
	out := make([]Table, 0, len(names))
	state := map[string]int{}
	var visit func(string) error
	visit = func(n string) error {
		switch state[n] {
		case 1:
			return fmt.Errorf("database: foreign keys form a cycle through %s", n)
		case 2:
			return nil
		}
		state[n] = 1
		refs := deps[n]
		slices.Sort(refs)
		for _, r := range refs {
			if byName[r] != nil {
				if err := visit(r); err != nil {
					return err
				}
			}
		}
		state[n] = 2
		t := *byName[n]
		if len(t.Key) == 0 {
			for _, c := range t.Columns {
				t.Key = append(t.Key, c.Name)
			}
		}
		out = append(out, t)
		return nil
	}
	slices.Sort(names)
	for _, n := range names {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func sqliteKind(declared string) string {
	t := strings.ToUpper(declared)
	switch {
	case strings.Contains(t, "BOOL"):
		return KindBool
	case strings.Contains(t, "TIMESTAMP"), strings.Contains(t, "DATE"):
		return KindTime
	case strings.Contains(t, "BLOB"):
		return KindBytes
	case strings.Contains(t, "INT"):
		return KindInt
	case strings.Contains(t, "REAL"), strings.Contains(t, "DOUB"), strings.Contains(t, "FLOA"), strings.Contains(t, "NUMERIC"):
		return KindFloat
	}
	return KindText
}

func sqliteSchemaOf(ctx context.Context, d *DB) (map[string]*Table, []string, map[string][]string, error) {
	rows, err := d.SQL.QueryContext(ctx, `SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'goose_db_version'`)
	if err != nil {
		return nil, nil, nil, err
	}
	created := map[string]string{}
	var names []string
	for rows.Next() {
		var n, ddl string
		if err := rows.Scan(&n, &ddl); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		names = append(names, n)
		created[n] = ddl
	}
	rows.Close()
	byName := map[string]*Table{}
	deps := map[string][]string{}
	for _, n := range names {
		t := &Table{Name: n, SelfRef: map[string]string{}}
		type keyCol struct {
			pos  int
			name string
		}
		var keys []keyCol
		cols, err := d.SQL.QueryContext(ctx, `SELECT name, type, pk FROM pragma_table_info(?) ORDER BY cid`, n)
		if err != nil {
			return nil, nil, nil, err
		}
		for cols.Next() {
			var c, typ string
			var pk int
			if err := cols.Scan(&c, &typ, &pk); err != nil {
				cols.Close()
				return nil, nil, nil, err
			}
			serial := pk > 0 && strings.Contains(strings.ToUpper(created[n]), "AUTOINCREMENT") && strings.Contains(strings.ToUpper(typ), "INT")
			t.Columns = append(t.Columns, Column{Name: c, Kind: sqliteKind(typ), Serial: serial})
			if pk > 0 {
				keys = append(keys, keyCol{pk, c})
			}
		}
		cols.Close()
		slices.SortFunc(keys, func(a, b keyCol) int { return a.pos - b.pos })
		for _, k := range keys {
			t.Key = append(t.Key, k.name)
		}
		fks, err := d.SQL.QueryContext(ctx, `SELECT "table", "from", COALESCE("to", '') FROM pragma_foreign_key_list(?)`, n)
		if err != nil {
			return nil, nil, nil, err
		}
		for fks.Next() {
			var ref, from, to string
			if err := fks.Scan(&ref, &from, &to); err != nil {
				fks.Close()
				return nil, nil, nil, err
			}
			if ref == n {
				t.SelfRef[from] = to
				continue
			}
			deps[n] = append(deps[n], ref)
		}
		fks.Close()
		byName[n] = t
	}
	return byName, names, deps, nil
}

func postgresKind(dataType string) string {
	switch dataType {
	case "boolean":
		return KindBool
	case "timestamp with time zone", "timestamp without time zone", "date":
		return KindTime
	case "bytea":
		return KindBytes
	case "bigint", "integer", "smallint":
		return KindInt
	case "double precision", "real", "numeric":
		return KindFloat
	}
	return KindText
}

func postgresSchemaOf(ctx context.Context, d *DB) (map[string]*Table, []string, map[string][]string, error) {
	rows, err := d.SQL.QueryContext(ctx, `SELECT c.table_name, c.column_name, c.data_type, COALESCE(c.column_default, '') FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = current_schema() AND t.table_type = 'BASE TABLE' AND c.table_name <> 'goose_db_version' ORDER BY c.table_name, c.ordinal_position`)
	if err != nil {
		return nil, nil, nil, err
	}
	byName := map[string]*Table{}
	var names []string
	for rows.Next() {
		var t, c, kind, def string
		if err := rows.Scan(&t, &c, &kind, &def); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		if byName[t] == nil {
			byName[t] = &Table{Name: t, SelfRef: map[string]string{}}
			names = append(names, t)
		}
		byName[t].Columns = append(byName[t].Columns, Column{Name: c, Kind: postgresKind(kind), Serial: strings.HasPrefix(def, "nextval(")})
	}
	rows.Close()
	keys, err := d.SQL.QueryContext(ctx, `SELECT tc.table_name, kcu.column_name FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu ON kcu.constraint_name = tc.constraint_name AND kcu.table_schema = tc.table_schema
		WHERE tc.table_schema = current_schema() AND tc.constraint_type = 'PRIMARY KEY' ORDER BY tc.table_name, kcu.ordinal_position`)
	if err != nil {
		return nil, nil, nil, err
	}
	for keys.Next() {
		var t, c string
		if err := keys.Scan(&t, &c); err != nil {
			keys.Close()
			return nil, nil, nil, err
		}
		if byName[t] != nil {
			byName[t].Key = append(byName[t].Key, c)
		}
	}
	keys.Close()
	deps := map[string][]string{}
	fks, err := d.SQL.QueryContext(ctx, `SELECT cl.relname, ref.relname, a.attname, af.attname FROM pg_constraint con
		JOIN pg_class cl ON cl.oid = con.conrelid JOIN pg_class ref ON ref.oid = con.confrelid
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = con.conkey[1]
		JOIN pg_attribute af ON af.attrelid = con.confrelid AND af.attnum = con.confkey[1]
		JOIN pg_namespace n ON n.oid = cl.relnamespace WHERE con.contype = 'f' AND n.nspname = current_schema()`)
	if err != nil {
		return nil, nil, nil, err
	}
	for fks.Next() {
		var t, ref, col, refCol string
		if err := fks.Scan(&t, &ref, &col, &refCol); err != nil {
			fks.Close()
			return nil, nil, nil, err
		}
		if byName[t] == nil {
			continue
		}
		if t == ref {
			byName[t].SelfRef[col] = refCol
			continue
		}
		deps[t] = append(deps[t], ref)
	}
	fks.Close()
	return byName, names, deps, nil
}

// Queryer is satisfied by *sql.DB and *sql.Tx.
type Queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

// ReadTable calls fn with every row of t in key order, each value normalised by its column kind.
func ReadTable(ctx context.Context, q Queryer, t Table, fn func(row []any) error) error {
	rows, err := q.QueryContext(ctx, `SELECT `+t.columnList()+` FROM `+t.Name+` ORDER BY `+strings.Join(t.Key, ", "))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]any, len(t.Columns))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for i, c := range t.Columns {
			if vals[i], err = Normalize(c.Kind, vals[i]); err != nil {
				return fmt.Errorf("%s.%s: %w", t.Name, c.Name, err)
			}
		}
		if err := fn(vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

var timeLayouts = []string{"2006-01-02 15:04:05.999999999-07:00", time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"}

// Normalize converts a value read from either dialect to the Go type of its kind: bool, time.Time in UTC to the
// microsecond (as PostgreSQL stores it), []byte, int64, float64 or string. nil stays nil.
func Normalize(kind string, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch kind {
	case KindBool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case int64:
			return x != 0, nil
		case string:
			return strconv.ParseBool(x)
		}
	case KindTime:
		switch x := v.(type) {
		case time.Time:
			return x.UTC().Truncate(time.Microsecond), nil
		case string:
			for _, l := range timeLayouts {
				if t, err := time.Parse(l, x); err == nil {
					return t.UTC().Truncate(time.Microsecond), nil
				}
			}
			return nil, fmt.Errorf("unrecognised time %q", x)
		}
	case KindBytes:
		switch x := v.(type) {
		case []byte:
			return x, nil
		case string:
			return []byte(x), nil
		}
	case KindInt:
		switch x := v.(type) {
		case int64:
			return x, nil
		case float64:
			return int64(x), nil
		case bool:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		case string:
			return strconv.ParseInt(x, 10, 64)
		}
	case KindFloat:
		switch x := v.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		case string:
			return strconv.ParseFloat(x, 64)
		case []byte:
			return strconv.ParseFloat(string(x), 64)
		}
	default:
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		case time.Time:
			return x.UTC().Format(time.RFC3339Nano), nil
		}
		return fmt.Sprint(v), nil
	}
	return nil, fmt.Errorf("cannot read %T as %s", v, kind)
}

// Canonical renders a normalised value for checksums.
func Canonical(v any) string {
	switch x := v.(type) {
	case nil:
		return "\\N"
	case time.Time:
		return x.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	case []byte:
		return hex.EncodeToString(x)
	case bool:
		if x {
			return "t"
		}
		return "f"
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return fmt.Sprint(v)
}

// RowHasher accumulates a table checksum from normalised rows in key order.
type RowHasher struct {
	h    interface{ Write([]byte) (int, error) }
	sum  func() []byte
	Rows int64
}

func NewRowHasher() *RowHasher {
	h := sha256.New()
	return &RowHasher{h: h, sum: func() []byte { return h.Sum(nil) }}
}

func (r *RowHasher) Add(row []any) {
	for _, v := range row {
		_, _ = r.h.Write([]byte(Canonical(v) + "\x1f"))
	}
	_, _ = r.h.Write([]byte{0x1e})
	r.Rows++
}

func (r *RowHasher) Sum() string { return hex.EncodeToString(r.sum()) }

// TableChecksum hashes a table's rows in key order.
func TableChecksum(ctx context.Context, q Queryer, t Table) (string, int64, error) {
	h := NewRowHasher()
	err := ReadTable(ctx, q, t, func(row []any) error {
		h.Add(row)
		return nil
	})
	return h.Sum(), h.Rows, err
}

// Writer inserts rows into the tables of a fresh database inside one transaction, holding rows of self-referencing
// tables until the row they refer to is present.
type Writer struct {
	d     *DB
	tx    *sql.Tx
	t     Table
	batch [][]any
	held  [][]any
	n     int64
}

func (d *DB) NewWriter(tx *sql.Tx, t Table) *Writer { return &Writer{d: d, tx: tx, t: t} }

func (w *Writer) Add(ctx context.Context, row []any) error {
	if len(w.t.SelfRef) > 0 {
		w.held = append(w.held, row)
		return nil
	}
	w.batch = append(w.batch, row)
	if len(w.batch) >= w.limit() {
		return w.flush(ctx)
	}
	return nil
}

func (w *Writer) limit() int { return max(1, 20000/len(w.t.Columns)) }

// Close writes what is left and resets serial sequences; it returns the rows written.
func (w *Writer) Close(ctx context.Context) (int64, error) {
	ordered, err := selfOrder(w.t, w.held)
	if err != nil {
		return 0, err
	}
	for _, r := range ordered {
		w.batch = append(w.batch, r)
		if len(w.batch) >= w.limit() {
			if err := w.flush(ctx); err != nil {
				return 0, err
			}
		}
	}
	if err := w.flush(ctx); err != nil {
		return 0, err
	}
	if w.d.Dialect == Postgres {
		for _, c := range w.t.Columns {
			if c.Serial {
				if _, err := w.tx.ExecContext(ctx, fmt.Sprintf(`SELECT setval(pg_get_serial_sequence('%s', '%s'), COALESCE(MAX(%s), 1), MAX(%s) IS NOT NULL) FROM %s`, w.t.Name, c.Name, c.Name, c.Name, w.t.Name)); err != nil {
					return 0, err
				}
			}
		}
	}
	return w.n, nil
}

func (w *Writer) flush(ctx context.Context) error {
	if len(w.batch) == 0 {
		return nil
	}
	var b strings.Builder
	var args []any
	fmt.Fprintf(&b, `INSERT INTO %s (%s) VALUES `, w.t.Name, w.t.columnList())
	for i, row := range w.batch {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(")
		for j, v := range row {
			if j > 0 {
				b.WriteString(", ")
			}
			args = append(args, v)
			b.WriteString("?")
		}
		b.WriteString(")")
	}
	if _, err := w.tx.ExecContext(ctx, w.d.Rebind(b.String()), args...); err != nil {
		return err
	}
	w.n += int64(len(w.batch))
	w.batch = w.batch[:0]
	return nil
}

func selfOrder(t Table, rows [][]any) ([][]any, error) {
	if len(t.SelfRef) == 0 {
		return rows, nil
	}
	idx := func(name string) int {
		for i, c := range t.Columns {
			if c.Name == name {
				return i
			}
		}
		return -1
	}
	present := map[string]map[string]bool{}
	for _, refCol := range t.SelfRef {
		present[refCol] = map[string]bool{}
	}
	var out [][]any
	pending := rows
	for len(pending) > 0 {
		var next [][]any
		for _, r := range pending {
			ready := true
			for col, refCol := range t.SelfRef {
				if v := r[idx(col)]; v != nil && !present[refCol][fmt.Sprint(v)] {
					ready = false
				}
			}
			if !ready {
				next = append(next, r)
				continue
			}
			out = append(out, r)
			for refCol := range present {
				present[refCol][fmt.Sprint(r[idx(refCol)])] = true
			}
		}
		if len(next) == len(pending) {
			return nil, fmt.Errorf("%s: %d rows refer to rows that do not exist", t.Name, len(next))
		}
		pending = next
	}
	return out, nil
}

// ErrNotEmpty means a database already holds application data where an empty one is required.
var ErrNotEmpty = errors.New("the target database is not empty")

// RequireEmpty fails unless every table is empty.
func RequireEmpty(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}, tables []Table) error {
	for _, t := range tables {
		var n int64
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t.Name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: %s holds %d rows", ErrNotEmpty, t.Name, n)
		}
	}
	return nil
}
