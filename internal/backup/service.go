package backup

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

// Record is one backup the controller wrote.
type Record struct {
	ID            string
	CreatedAt     time.Time
	File          string
	Size          int64
	SHA256        string
	Dialect       string
	SchemaVersion int64
	Tables        int
	Rows          int64
	Blobs         int
	IncludesKeys  bool
	Trigger       string
	CreatedBy     string
	CheckedAt     *time.Time
	CheckResult   string
	Removed       bool
}

var (
	ErrDisabled  = errors.New("backups are not configured (backup.dir and backup.recipientsFile)")
	ErrForbidden = errors.New("only organization administrators manage backups")
	ErrRunning   = errors.New("a backup is already running")
	ErrNotFound  = errors.New("not found")
)

// Service writes backups into one directory and records them; it keeps the newest Keep archives.
type Service struct {
	db   *db.DB
	opts Options
	dir  string
	keep int
	// Interval is the schedule the controller runs; zero means backups only on request.
	Interval time.Duration
	mu       sync.Mutex
	now      func() time.Time
}

func (s *Service) Dir() string { return s.dir }

func (s *Service) Keep() int { return s.keep }

// ReadRecipients parses an age recipients file: one public key per line, # comments allowed.
func ReadRecipients(path string) ([]age.Recipient, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rs, err := age.ParseRecipients(f)
	if err != nil {
		return nil, fmt.Errorf("backup: %s: %w", path, err)
	}
	return rs, nil
}

// ReadIdentities parses an age identity file, which must not be readable by group or others.
func ReadIdentities(path string) ([]age.Identity, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("backup: %s must not be readable by group or others", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return age.ParseIdentities(bufio.NewReader(f))
}

func NewService(d *db.DB, dir string, keep int, o Options) *Service {
	return &Service{db: d, opts: o, dir: dir, keep: keep, now: time.Now}
}

func (s *Service) Enabled() bool { return s != nil && s.dir != "" }

// Run writes a backup now. actor is nil for scheduled backups and a system principal for the backup command.
func (s *Service) Run(ctx context.Context, actor *authz.Principal, requestID string) (Record, error) {
	if !s.Enabled() {
		return Record{}, ErrDisabled
	}
	if actor != nil && actor.Kind != authz.System && !actor.IsOrgAdmin() {
		return Record{}, ErrForbidden
	}
	if !s.mu.TryLock() {
		return Record{}, ErrRunning
	}
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Record{}, err
	}
	now := s.now().UTC()
	r := Record{ID: ids.New(), CreatedAt: now, Trigger: "schedule"}
	switch {
	case actor != nil && actor.Kind == authz.System:
		r.Trigger = "command"
	case actor != nil:
		r.Trigger, r.CreatedBy = "manual", actor.ID
	}
	r.File = filepath.Join(s.dir, "samuori-"+now.Format("20060102T150405Z")+"-"+r.ID[len(r.ID)-6:]+".age")
	tmp := r.File + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Record{}, err
	}
	h := sha256.New()
	cw := &counter{w: io.MultiWriter(f, h)}
	opts := s.opts
	opts.Now = s.now
	m, err := Create(ctx, opts, cw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, r.File)
	}
	if err != nil {
		_ = os.Remove(tmp)
		s.audit(ctx, actor, r.ID, "failed", err.Error(), requestID)
		return Record{}, err
	}
	r.Size, r.SHA256 = cw.n, hex.EncodeToString(h.Sum(nil))
	r.Dialect, r.SchemaVersion, r.Tables, r.Blobs, r.IncludesKeys = string(m.Dialect), m.SchemaVersion, len(m.Tables), m.Blobs, m.IncludesKeys
	for _, t := range m.Tables {
		r.Rows += t.Rows
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO backups (id, created_at, file, size, sha256, dialect, schema_version, tables, row_count, blobs, includes_keys, trigger, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), r.ID, r.CreatedAt, r.File, r.Size, r.SHA256, r.Dialect, r.SchemaVersion, r.Tables, r.Rows, r.Blobs, r.IncludesKeys, r.Trigger, nullable(r.CreatedBy)); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: s.principal(actor), Action: "backup.create", ResourceType: "backup", ResourceID: r.ID, Outcome: "succeeded", Reason: filepath.Base(r.File), RequestID: requestID})
	})
	if err != nil {
		return Record{}, err
	}
	return r, s.prune(ctx)
}

// WriteFile writes a backup to path with these options, before the database can record it, as an upgrade does
// before migrating; Record adds it to the list afterwards.
func (s *Service) WriteFile(ctx context.Context, path string) (Manifest, error) {
	if !s.Enabled() {
		return Manifest{}, ErrDisabled
	}
	tmp := path + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Manifest{}, err
	}
	m, err := Create(ctx, s.opts, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return m, err
}

// Record adds an archive written by WriteFile to the list, with the given trigger, and audits it.
func (s *Service) Record(ctx context.Context, path string, m Manifest, trigger, reason string) (Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		return Record{}, err
	}
	r := Record{ID: ids.New(), CreatedAt: m.CreatedAt, File: path, Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), Dialect: string(m.Dialect), SchemaVersion: m.SchemaVersion,
		Tables: len(m.Tables), Blobs: m.Blobs, IncludesKeys: m.IncludesKeys, Trigger: trigger}
	for _, t := range m.Tables {
		r.Rows += t.Rows
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO backups (id, created_at, file, size, sha256, dialect, schema_version, tables, row_count, blobs, includes_keys, trigger)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), r.ID, r.CreatedAt, r.File, r.Size, r.SHA256, r.Dialect, r.SchemaVersion, r.Tables, r.Rows, r.Blobs, r.IncludesKeys, r.Trigger); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{Kind: authz.System}, Action: "backup.create", ResourceType: "backup", ResourceID: r.ID, Outcome: "succeeded", Reason: reason})
	})
	return r, err
}

func (s *Service) principal(actor *authz.Principal) authz.Principal {
	if actor != nil {
		return *actor
	}
	return authz.Principal{Kind: authz.System}
}

func (s *Service) audit(ctx context.Context, actor *authz.Principal, id, outcome, reason, requestID string) {
	_ = s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: s.principal(actor), Action: "backup.create", ResourceType: "backup", ResourceID: id, Outcome: outcome, Reason: reason, RequestID: requestID})
	})
}

// prune removes archives beyond the newest Keep; their records stay, marked removed.
func (s *Service) prune(ctx context.Context) error {
	list, err := s.List(ctx, authz.Principal{Kind: authz.System}, 1000)
	if err != nil {
		return err
	}
	kept := 0
	for _, r := range list {
		if r.Removed {
			continue
		}
		if kept++; kept <= s.keep {
			continue
		}
		if err := os.Remove(r.File); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE backups SET removed_at = ? WHERE id = ?`), s.now().UTC(), r.ID); err != nil {
			return err
		}
	}
	return nil
}

// List returns backups, newest first.
func (s *Service) List(ctx context.Context, actor authz.Principal, limit int) ([]Record, error) {
	if actor.Kind != authz.System && !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, created_at, file, size, sha256, dialect, schema_version, tables, row_count, blobs, includes_keys, trigger, COALESCE(created_by, ''), checked_at, COALESCE(check_result, ''), removed_at IS NOT NULL
		FROM backups ORDER BY created_at DESC, id DESC LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var checked sql.NullTime
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.File, &r.Size, &r.SHA256, &r.Dialect, &r.SchemaVersion, &r.Tables, &r.Rows, &r.Blobs, &r.IncludesKeys, &r.Trigger, &r.CreatedBy, &checked, &r.CheckResult, &r.Removed); err != nil {
			return nil, err
		}
		if checked.Valid {
			t := checked.Time
			r.CheckedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Check re-reads an archive and compares it with the digest recorded when it was written. Only an operator holding
// the age identity can decrypt and fully verify it (samuori-server verify-backup).
func (s *Service) Check(ctx context.Context, actor authz.Principal, id, requestID string) (Record, error) {
	if !actor.IsOrgAdmin() {
		return Record{}, ErrForbidden
	}
	if !s.Enabled() {
		return Record{}, ErrDisabled
	}
	list, err := s.List(ctx, actor, 1000)
	if err != nil {
		return Record{}, err
	}
	var r *Record
	for i := range list {
		if list[i].ID == id {
			r = &list[i]
		}
	}
	if r == nil {
		return Record{}, ErrNotFound
	}
	result := "intact"
	f, err := os.Open(r.File)
	if err != nil {
		result = "missing"
	} else {
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil || n != r.Size || hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
			result = "changed"
		}
	}
	now := s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE backups SET checked_at = ?, check_result = ? WHERE id = ?`), now, result, id); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "backup.check", ResourceType: "backup", ResourceID: id, Outcome: result, RequestID: requestID})
	})
	r.CheckedAt, r.CheckResult = &now, result
	return *r, err
}

type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func nullable(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// CheckTools verifies, for a PostgreSQL database, that pg_dump and pg_restore exist and are at least the server's
// major version, as a dump of a newer server would fail. SQLite needs no tools.
func (s *Service) CheckTools(ctx context.Context) error {
	if !s.Enabled() || s.db.Dialect != db.Postgres {
		return nil
	}
	var serverNum int
	if err := s.db.SQL.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverNum); err != nil {
		return err
	}
	server := serverNum / 10000
	for _, tool := range []string{s.opts.tool("pg_dump", s.opts.PGDump), s.opts.tool("pg_restore", s.opts.PGRestore)} {
		out, err := exec.CommandContext(ctx, tool, "--version").Output()
		if err != nil {
			return fmt.Errorf("backup: %s is not available (%v); PostgreSQL backups need it, at major version %d or newer", tool, err, server)
		}
		// "pg_dump (PostgreSQL) 18.1", sometimes followed by a vendor note.
		major := 0
		for _, f := range strings.Fields(string(out)) {
			if f[0] >= '0' && f[0] <= '9' {
				_, _ = fmt.Sscanf(f, "%d", &major)
				break
			}
		}
		if major < server {
			return fmt.Errorf("backup: %s is version %d, older than the PostgreSQL %d server it must dump", tool, major, server)
		}
	}
	return nil
}
