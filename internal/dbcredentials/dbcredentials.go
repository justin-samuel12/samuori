// Package dbcredentials lets an administrator hand every controller a new PostgreSQL password after changing it at
// the database. Samuori never changes the password itself; it verifies the new one on each controller before using it
// (ADR 0043, docs/operators/database.md).
package dbcredentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/db"
)

const (
	Verified = "verified"
	Failed   = "failed"
	Pending  = "pending"

	syncEvery  = 5 * time.Second
	retryAfter = time.Minute
)

var (
	ErrForbidden   = errors.New("only administrators manage the database connection")
	ErrNotPostgres = errors.New("this installation uses SQLite, which has no password")
	ErrConflict    = errors.New("another password update was saved at the same time; reload and try again")
)

// Invalid is a refused update; Reason is safe to show.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

type Service struct {
	db      *db.DB
	member  *cluster.Member
	dataDir string
	baseDSN string
	aead    cipher.AEAD
	log     *slog.Logger

	mu      sync.Mutex
	current string
	applied int64
	failed  int64
	retryAt time.Time
}

// New takes the configured DSN before any stored password, and a 32-byte key derived from the cluster key.
func New(d *db.DB, member *cluster.Member, dataDir, baseDSN string, key []byte, log *slog.Logger) (*Service, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Service{db: d, member: member, dataDir: dataDir, baseDSN: baseDSN, aead: aead, log: log, current: baseDSN}
	if d.Dialect == db.Postgres {
		sp, ok, err := db.ReadStoredPassword(dataDir)
		if err != nil {
			return nil, err
		}
		if ok {
			if s.current, err = db.WithPassword(baseDSN, sp.Password); err != nil {
				return nil, err
			}
			s.applied = sp.Revision
		}
	}
	return s, nil
}

func aad(revision int64) []byte {
	return []byte("samuori-database-password/" + strconv.FormatInt(revision, 10))
}

func (s *Service) seal(revision int64, password string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(password), aad(revision)), nil
}

func (s *Service) open(revision int64, sealed []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("the stored password is damaged")
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], aad(revision))
	if err != nil {
		return "", errors.New("this controller's cluster key cannot open the stored password")
	}
	return string(plain), nil
}

// Update verifies password from this controller, stores it for the others and switches to it here.
func (s *Service) Update(ctx context.Context, actor authz.Principal, password, reason, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	if s.db.Dialect != db.Postgres {
		return ErrNotPostgres
	}
	reason = strings.TrimSpace(reason)
	switch {
	case password == "" || len(password) > 1024:
		return &Invalid{"enter the password you set at the database"}
	case len(reason) < 3:
		return &Invalid{"a reason is required"}
	}
	dsn, err := db.WithPassword(s.baseDSN, password)
	if err != nil {
		return err
	}
	if _, err := db.Verify(ctx, dsn); err != nil {
		return &Invalid{"nothing changed: " + err.Error()}
	}
	// The database already requires this password, so new connections, including the one below, must use it.
	s.db.SetPassword(password)
	var revision int64
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) + 1 FROM database_credentials`).Scan(&revision); err != nil {
			return err
		}
		sealed, err := s.seal(revision, password)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO database_credentials (revision, sealed, reason, created_by, created_at) VALUES (?, ?, ?, ?, ?)`),
			revision, sealed, reason, actor.ID, time.Now().UTC()); err != nil {
			return ErrConflict
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "database.password.update", ResourceType: "database", ResourceID: strconv.FormatInt(revision, 10), Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(ctx, revision, password, dsn)
}

// apply is called with mu held and dsn already verified.
func (s *Service) apply(ctx context.Context, revision int64, password, dsn string) error {
	if err := db.WriteStoredPassword(s.dataDir, db.StoredPassword{Revision: revision, Password: password}); err != nil {
		s.check(ctx, revision, Failed, "the password could not be saved on this controller")
		return err
	}
	s.db.SetPassword(password)
	s.current, s.applied, s.failed = dsn, revision, 0
	s.check(ctx, revision, Verified, "")
	s.log.Info("database password switched", "revision", revision)
	return nil
}

func (s *Service) check(ctx context.Context, revision int64, status, detail string) {
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`DELETE FROM database_credential_checks WHERE revision = ? AND controller_id = ?`), revision, s.member.ID)
	if err == nil {
		_, err = s.db.SQL.ExecContext(ctx, s.db.Rebind(`INSERT INTO database_credential_checks (revision, controller_id, hostname, status, detail, checked_at) VALUES (?, ?, ?, ?, ?, ?)`),
			revision, s.member.ID, s.member.Hostname, status, detail, time.Now().UTC())
	}
	if err != nil {
		s.log.Warn("database password check not recorded", "revision", revision, "err", err)
	}
}

// Run picks up passwords other controllers stored, verifies each before switching, and retries a failure later.
func (s *Service) Run(ctx context.Context) {
	if s.db.Dialect != db.Postgres {
		return
	}
	t := time.NewTicker(syncEvery)
	defer t.Stop()
	for {
		s.sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) sync(ctx context.Context) {
	var revision int64
	var sealed []byte
	err := s.db.SQL.QueryRowContext(ctx, `SELECT revision, sealed FROM database_credentials ORDER BY revision DESC LIMIT 1`).Scan(&revision, &sealed)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision <= s.applied || (revision == s.failed && time.Now().Before(s.retryAt)) {
		return
	}
	fail := func(detail string) {
		s.failed, s.retryAt = revision, time.Now().Add(retryAfter)
		s.check(ctx, revision, Failed, detail)
		s.log.Warn("database password not switched", "revision", revision, "reason", detail)
	}
	password, err := s.open(revision, sealed)
	if err != nil {
		fail(err.Error())
		return
	}
	dsn, err := db.WithPassword(s.baseDSN, password)
	if err != nil {
		fail(err.Error())
		return
	}
	if _, err := db.Verify(ctx, dsn); err != nil {
		fail(err.Error())
		return
	}
	_ = s.apply(ctx, revision, password, dsn)
}

// Test signs in with a fresh connection using the password this controller uses now.
func (s *Service) Test(ctx context.Context, actor authz.Principal) (db.Probe, error) {
	if !actor.IsOrgAdmin() {
		return db.Probe{}, ErrForbidden
	}
	if s.db.Dialect != db.Postgres {
		start := time.Now()
		var v string
		if err := s.db.SQL.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&v); err != nil {
			return db.Probe{}, err
		}
		return db.Probe{ServerVersion: v, Latency: time.Since(start)}, nil
	}
	s.mu.Lock()
	dsn := s.current
	s.mu.Unlock()
	return db.Verify(ctx, dsn)
}

type Revision struct {
	Number    int64
	CreatedBy string
	CreatedAt time.Time
	Reason    string
}

type ControllerCheck struct {
	ID, Hostname string
	Live         bool
	Status       string
	Detail       string
	CheckedAt    *time.Time
}

type Status struct {
	Target        db.Target
	ServerVersion string
	Latency       time.Duration
	Pool          sql.DBStats
	Revision      *Revision
	Controllers   []ControllerCheck
	CanUpdate     bool
}

// Status reports the connection as this controller sees it and each controller's check of the newest password.
func (s *Service) Status(ctx context.Context, actor authz.Principal) (Status, error) {
	if !actor.IsOrgAdmin() {
		return Status{}, ErrForbidden
	}
	s.mu.Lock()
	st := Status{Target: db.Describe(s.current), Pool: s.db.SQL.Stats(), CanUpdate: s.db.Dialect == db.Postgres}
	s.mu.Unlock()
	version := `SELECT current_setting('server_version')`
	if s.db.Dialect == db.SQLite {
		version = `SELECT sqlite_version()`
	}
	start := time.Now()
	if err := s.db.SQL.QueryRowContext(ctx, version).Scan(&st.ServerVersion); err != nil {
		return Status{}, err
	}
	st.Latency = time.Since(start)

	var r Revision
	err := s.db.SQL.QueryRowContext(ctx, `SELECT revision, created_by, created_at, reason FROM database_credentials ORDER BY revision DESC LIMIT 1`).Scan(&r.Number, &r.CreatedBy, &r.CreatedAt, &r.Reason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Status{}, err
	}
	checks := map[string]ControllerCheck{}
	if err == nil {
		st.Revision = &r
		rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT controller_id, status, detail, checked_at FROM database_credential_checks WHERE revision = ?`), r.Number)
		if err != nil {
			return Status{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var c ControllerCheck
			var at time.Time
			if err := rows.Scan(&c.ID, &c.Status, &c.Detail, &at); err != nil {
				return Status{}, err
			}
			c.CheckedAt = &at
			checks[c.ID] = c
		}
		if err := rows.Err(); err != nil {
			return Status{}, err
		}
	}
	controllers, err := s.member.Controllers(ctx)
	if err != nil {
		return Status{}, err
	}
	st.Controllers = []ControllerCheck{}
	for _, c := range controllers {
		if !c.Live {
			continue
		}
		cc, ok := checks[c.ID]
		if !ok {
			cc = ControllerCheck{ID: c.ID, Status: Pending}
			if st.Revision == nil {
				cc.Status = Verified
			}
		}
		cc.Hostname, cc.Live = c.Hostname, true
		st.Controllers = append(st.Controllers, cc)
	}
	return st, nil
}
