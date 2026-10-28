// Package workers enrolls build workers and serves the worker protocol (docs/architecture/worker-protocol.md).
// Enrollment establishes identity only; it never makes a worker eligible for untrusted builds.
package workers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/hostcap"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/pki"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("conflict")
	ErrToken     = errors.New("enrollment token is invalid, expired, revoked or already used")
)

const (
	Enrolled = "enrolled"
	Expired  = "expired"
	Revoked  = "revoked"
	Active   = "active"
)

type Pool struct {
	ID, Name, Location, IsolationProfile, TrustDomain string
	TrustedWorkloadsOnly, ProfileApproved             bool
	CreatedAt                                         time.Time
	// ApprovedBy and ApprovedAt describe the newest approval decision, if any.
	ApprovedBy string
	ApprovedAt *time.Time
}

type Capabilities struct {
	OS                string   `json:"os"`
	Architectures     []string `json:"architectures"`
	Engines           []string `json:"engines"`
	IsolationProfiles []string `json:"isolationProfiles"`
	DaemonSlots       uint32   `json:"daemonSlots"`
	NestedVirt        bool     `json:"nestedVirtualization"`
	StrictEgress      bool     `json:"strictEgress"`
	ConfigDigest      string   `json:"configDigest,omitempty"`
	// Providers maps each optional build or conversion provider installed on the worker to its version (ADR 0047).
	Providers map[string]string `json:"providers,omitempty"`
}

// BusySlotsQuery counts the daemon slots a worker's builds and conversions hold; it takes the worker ID twice
// (one build or conversion per daemon, OD-16).
const BusySlotsQuery = `SELECT (SELECT COUNT(*) FROM build_attempts WHERE worker_id = ? AND state IN ('leased', 'preparing', 'building', 'committing')) +
	(SELECT COUNT(*) FROM kube_conversion_runs WHERE lease_worker_id = ? AND state IN ('assigned', 'running'))`

type Worker struct {
	ID, OrgID, PoolID, Name           string
	EnrollmentState, OperationalState string
	CertSerial, PrevCertSerial        string
	CertExpiresAt                     time.Time
	BundleVersion                     uint64
	RevokedAt, CompromisedSince       *time.Time
	RevokeReason, ReplacedBy          string
	Capabilities                      Capabilities
	Version                           string
	LastSeenAt                        *time.Time
	CreatedAt                         time.Time
	// SelfTest is the newest finished self-test; SelfTestRunning reports one in progress.
	SelfTest        WorkerSelfTest
	SelfTestRunning bool
}

type WorkerSelfTest struct {
	Status       string
	FinishedAt   *time.Time
	Profile      string
	ConfigDigest string
	FailedChecks []string
}

type Store struct {
	db   *db.DB
	auth *pki.Authority
	// CertTTL is the lifetime of worker certificates; renewal arrives with remote enrollment (WP24).
	CertTTL time.Duration
	now     func() time.Time
}

func NewStore(d *db.DB, auth *pki.Authority) *Store {
	return &Store{db: d, auth: auth, CertTTL: 7 * 24 * time.Hour, now: time.Now}
}

func (s *Store) Authority() *pki.Authority { return s.auth }

func (s *Store) CreatePool(ctx context.Context, actor authz.Principal, p Pool, requestID string) (Pool, error) {
	if !actor.IsOrgAdmin() {
		return Pool{}, ErrForbidden
	}
	return s.createPool(ctx, actor, p, requestID)
}

func (s *Store) ListPools(ctx context.Context, orgID string) ([]Pool, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT p.id, p.name, p.location, p.isolation_profile, p.trust_domain, p.trusted_workloads_only, p.profile_approved, p.created_at,
		(SELECT COALESCE(u.username, a.actor_id) FROM pool_approvals a LEFT JOIN users u ON u.id = a.actor_id WHERE a.pool_id = p.id ORDER BY a.created_at DESC LIMIT 1),
		(SELECT a.created_at FROM pool_approvals a WHERE a.pool_id = p.id ORDER BY a.created_at DESC LIMIT 1)
		FROM worker_pools p WHERE p.org_id = ? ORDER BY p.name`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pool
	for rows.Next() {
		var p Pool
		var by sql.NullString
		var at sql.NullTime
		if err := rows.Scan(&p.ID, &p.Name, &p.Location, &p.IsolationProfile, &p.TrustDomain, &p.TrustedWorkloadsOnly, &p.ProfileApproved, &p.CreatedAt, &by, &at); err != nil {
			return nil, err
		}
		p.ApprovedBy = by.String
		if at.Valid {
			p.ApprovedAt = &at.Time
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type Token struct {
	// PoolID is empty for portal tokens: the worker enrols unassigned (ADR 0045). Only the all-in-one local
	// worker's token names its pool.
	ID, PoolID, Value, CAPin string
	ExpiresAt                time.Time
}

// CreateToken issues a single-use enrollment token. The worker it enrols starts unassigned. Only its hash is stored.
func (s *Store) CreateToken(ctx context.Context, actor authz.Principal, ttl time.Duration, requestID string) (Token, error) {
	if !actor.IsOrgAdmin() {
		return Token{}, ErrForbidden
	}
	return s.issueToken(ctx, actor, "", ttl, requestID)
}

func (s *Store) issueToken(ctx context.Context, actor authz.Principal, poolID string, ttl time.Duration, requestID string) (Token, error) {
	if poolID != "" {
		var n int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM worker_pools WHERE id = ? AND org_id = ?`), poolID, actor.OrgID).Scan(&n); err != nil {
			return Token{}, err
		}
		if n == 0 {
			return Token{}, ErrNotFound
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, err
	}
	t := Token{ID: ids.New(), PoolID: poolID, Value: "samuoriw_" + base64.RawURLEncoding.EncodeToString(raw), CAPin: s.auth.Issuing().Pin(), ExpiresAt: s.now().UTC().Add(ttl)}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO enrollment_tokens (id, org_id, pool_id, token_hash, created_by, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			t.ID, actor.OrgID, nullable(poolID), hash(t.Value), actor.ID, t.ExpiresAt, s.now().UTC()); err != nil {
			return err
		}
		resType, resID := "enrollment_token", t.ID
		if poolID != "" {
			resType, resID = "worker_pool", poolID
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "enrollment_token.create", ResourceType: resType, ResourceID: resID, Outcome: "succeeded", RequestID: requestID})
	})
	return t, err
}

type EnrollRequest struct {
	Token, Name, Version string
	// PreviousWorkerID is the identity the worker held before. It is revoked when it is in the token's pool, or,
	// for a token without a pool, when it has the same name in the same organization.
	PreviousWorkerID string
	CSR              []byte
	Capabilities     Capabilities
	ProtocolMinor    uint32
}

// Enroll consumes the token and records the worker in one transaction, then signs its certificate. The worker
// starts with no eligibility for untrusted builds, and unassigned unless the token names a pool.
func (s *Store) Enroll(ctx context.Context, in EnrollRequest) (Worker, []byte, error) {
	now := s.now().UTC()
	w := Worker{ID: ids.New(), Name: in.Name, EnrollmentState: Enrolled, OperationalState: Active, Capabilities: in.Capabilities, Version: in.Version, CreatedAt: now}
	cert, err := s.auth.Issuing().SignWorker(in.CSR, w.ID, s.CertTTL)
	if err != nil {
		return Worker{}, nil, err
	}
	w.CertSerial, w.CertExpiresAt = cert.SerialNumber.Text(16), cert.NotAfter.UTC()
	caps, _ := json.Marshal(in.Capabilities)
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var tokenID, orgID string
		var pool sql.NullString
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT id, org_id, pool_id FROM enrollment_tokens WHERE token_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`), hash(in.Token), now).Scan(&tokenID, &orgID, &pool)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrToken
		}
		if err != nil {
			return err
		}
		w.OrgID, w.PoolID = orgID, pool.String
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE enrollment_tokens SET used_at = ?, worker_id = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL`), now, w.ID, tokenID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrToken
		}
		if w.Name == "" {
			w.Name = "worker-" + strings.ToLower(w.ID[len(w.ID)-6:])
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO workers (id, org_id, pool_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			w.ID, orgID, nullable(w.PoolID), w.Name, Enrolled, Active, w.CertSerial, w.CertExpiresAt, string(caps), in.Version, in.ProtocolMinor, now); err != nil {
			return err
		}
		if in.PreviousWorkerID != "" {
			scope, arg := `pool_id = ?`, w.PoolID
			if w.PoolID == "" {
				scope, arg = `name = ?`, w.Name
			}
			res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET enrollment_state = ?, revoked_at = ?, revoke_reason = ?, replaced_by = ? WHERE id = ? AND org_id = ? AND `+scope+` AND enrollment_state <> ?`),
				Revoked, now, "re-enrolled as "+w.ID, w.ID, in.PreviousWorkerID, orgID, arg, Revoked)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				if err := audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{ID: w.ID, OrgID: orgID, Kind: authz.Service}, Action: "worker.revoke", ResourceType: "worker", ResourceID: in.PreviousWorkerID, Outcome: "succeeded", Reason: "re-enrolled as " + w.ID}); err != nil {
					return err
				}
			}
		}
		reason := "unassigned"
		if w.PoolID != "" {
			reason = "pool " + w.PoolID
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{ID: w.ID, OrgID: orgID, Kind: authz.Service}, Action: "worker.enroll", ResourceType: "worker", ResourceID: w.ID, Outcome: "succeeded", Reason: reason})
	})
	if err != nil {
		return Worker{}, nil, err
	}
	return w, cert.Raw, nil
}

// Authenticate maps a verified client certificate to an enrolled worker whose recorded serial matches.
func (s *Store) Authenticate(ctx context.Context, workerID, serial string) (Worker, error) {
	w, err := s.get(ctx, `id = ?`, workerID)
	if err != nil {
		return Worker{}, err
	}
	if w.EnrollmentState != Enrolled || serial == "" || (serial != w.CertSerial && serial != w.PrevCertSerial) {
		return Worker{}, ErrForbidden
	}
	if serial == w.CertSerial && w.PrevCertSerial != "" {
		if _, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET prev_cert_serial = '' WHERE id = ? AND cert_serial = ?`), w.ID, serial); err != nil {
			return Worker{}, err
		}
		w.PrevCertSerial = ""
	}
	// The TLS layer has verified the certificate's own validity period; cert_expires_at tracks the newest one.
	if serial == w.CertSerial && !s.now().Before(w.CertExpiresAt) {
		return Worker{}, ErrForbidden
	}
	return w, nil
}

func (s *Store) Seen(ctx context.Context, id string, caps *Capabilities, version string) error {
	now := s.now().UTC()
	if caps == nil {
		_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET last_seen_at = ? WHERE id = ?`), now, id)
		return err
	}
	raw, _ := json.Marshal(caps)
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET last_seen_at = ?, capabilities = ?, version = ? WHERE id = ?`), now, string(raw), version, id)
	return err
}

// RecordCapacity keeps the disk, memory and CPUs a worker last reported (docs/operators/capacity.md).
func (s *Store) RecordCapacity(ctx context.Context, id string, r hostcap.Report) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET capacity = ?, capacity_at = ? WHERE id = ?`), string(raw), time.Now().UTC(), id)
	return err
}

// RecordHost keeps the architecture and free update space a protocol 1.3 worker reports, for rollout checks.
func (s *Store) RecordHost(ctx context.Context, id, arch string, freeBytes uint64) error {
	if arch != "amd64" && arch != "arm64" {
		arch = "unsupported:" + arch[:min(len(arch), 16)]
	}
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET host_arch = ?, update_free_bytes = ? WHERE id = ?`), arch, int64(min(freeBytes, 1<<62)), id)
	return err
}

// OrgOf returns the organization of a worker, for controller-internal lookups.
func (s *Store) OrgOf(ctx context.Context, workerID string) (string, error) {
	var org string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT org_id FROM workers WHERE id = ?`), workerID).Scan(&org)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return org, err
}

func (s *Store) Get(ctx context.Context, orgID, id string) (Worker, error) {
	return s.get(ctx, `id = ? AND org_id = ?`, id, orgID)
}

func (s *Store) List(ctx context.Context, orgID, cursor string, limit int) ([]Worker, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectWorker+` WHERE org_id = ? AND id > ? ORDER BY id LIMIT ?`), orgID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// The self-test columns count only tests for the worker's current pool, so moving a worker resets its trust.
const selectWorker = `SELECT id, org_id, COALESCE(pool_id, ''), name, enrollment_state, operational_state, cert_serial, prev_cert_serial, cert_expires_at, bundle_version, revoked_at, revoke_reason, compromised_since, replaced_by, capabilities, version, last_seen_at, created_at,
	(SELECT t.status || '|' || t.profile || '|' || t.config_digest || '|' || t.failed_checks FROM pool_self_tests t WHERE t.worker_id = workers.id AND t.pool_id = workers.pool_id AND t.status <> 'running' ORDER BY t.started_at DESC LIMIT 1),
	(SELECT t.finished_at FROM pool_self_tests t WHERE t.worker_id = workers.id AND t.pool_id = workers.pool_id AND t.status <> 'running' ORDER BY t.started_at DESC LIMIT 1),
	(SELECT COUNT(*) FROM pool_self_tests t WHERE t.worker_id = workers.id AND t.status = 'running')
	FROM workers`

func (s *Store) get(ctx context.Context, where string, args ...any) (Worker, error) {
	w, err := scanWorker(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectWorker+` WHERE `+where), args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Worker{}, ErrNotFound
	}
	return w, err
}

func scanWorker(r interface{ Scan(...any) error }) (Worker, error) {
	var w Worker
	var caps string
	var seen, finished, revoked, compromised sql.NullTime
	var latest sql.NullString
	var running int
	err := r.Scan(&w.ID, &w.OrgID, &w.PoolID, &w.Name, &w.EnrollmentState, &w.OperationalState, &w.CertSerial, &w.PrevCertSerial, &w.CertExpiresAt, &w.BundleVersion, &revoked, &w.RevokeReason, &compromised, &w.ReplacedBy,
		&caps, &w.Version, &seen, &w.CreatedAt, &latest, &finished, &running)
	if err != nil {
		return Worker{}, err
	}
	if revoked.Valid {
		w.RevokedAt = &revoked.Time
	}
	if compromised.Valid {
		w.CompromisedSince = &compromised.Time
	}
	w.SelfTest.Status, w.SelfTestRunning = "never-run", running > 0
	if parts := strings.SplitN(latest.String, "|", 4); latest.Valid && len(parts) == 4 {
		w.SelfTest.Status, w.SelfTest.Profile, w.SelfTest.ConfigDigest = parts[0], parts[1], parts[2]
		_ = json.Unmarshal([]byte(parts[3]), &w.SelfTest.FailedChecks)
	}
	if finished.Valid {
		w.SelfTest.FinishedAt = &finished.Time
	}
	_ = json.Unmarshal([]byte(caps), &w.Capabilities)
	if seen.Valid {
		w.LastSeenAt = &seen.Time
	}
	return w, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func isUnique(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}
