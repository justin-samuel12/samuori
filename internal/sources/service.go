// Package sources turns uploaded archives into immutable, content-addressed source snapshots. Uploaded content is
// untrusted: it is never executed, and every entry is validated before it is stored (docs/architecture/sources.md).
package sources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/operations"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/tenancy"
)

const (
	StatusOpen       = "open"
	StatusCommitting = "committing"
	StatusCommitted  = "committed"
	StatusFailed     = "failed"
	StatusExpired    = "expired"

	commitKind = "upload.commit"
)

var (
	ErrNotFound = errors.New("not found")
	// ErrDeleted means the uploader deleted the snapshot: its record stays for build history, its files are gone.
	ErrDeleted = errors.New("this source upload was deleted")
	// ErrNotUploader refuses deleting someone else's upload.
	ErrNotUploader = errors.New("only the person who uploaded it can delete it")
	// ErrInUse refuses deleting an upload a build is running from.
	ErrInUse = errors.New("a build is running from this upload; cancel it first")
	ErrState = errors.New("upload is not open")
	ErrQuota = errors.New("too many open uploads")
)

// Config sets per-upload limits and service capacity.
type Config struct {
	Limits          Limits
	ChunkBytes      int64
	SessionTTL      time.Duration
	MaxOpenSessions int
	Workers         int
}

func DefaultConfig() Config {
	return Config{Limits: DefaultLimits(), ChunkBytes: 8 << 20, SessionTTL: 24 * time.Hour, MaxOpenSessions: 5, Workers: 2}
}

type Session struct {
	ID, ProjectID, Status string
	FileCount             int
	TotalBytes            int64
	ChunkBytes            int64
	MaxChunks             int
	Received              []int
	OperationID           string
	SnapshotID            string
	ExpiresAt             time.Time
}

type Snapshot struct {
	ID, ProjectID, Digest string
	FileCount             int
	TotalBytes            int64
	Rejected, Excluded    int
	Warnings              []Warning
	CreatedBy             string
	CreatedAt             time.Time
	DeletedBy             string
	DeletedAt             *time.Time
}

type Service struct {
	db      *db.DB
	blobs   *storage.Blobs
	ops     *operations.Store
	tenancy *tenancy.Store
	log     *slog.Logger
	cfg     Config
	staging string
	sem     chan struct{}
	wg      sync.WaitGroup
	now     func() time.Time
	owner   cluster.Ownership
	// sharedChunks keeps every chunk in blob storage, so any controller can commit an upload (ADR 0034).
	sharedChunks bool
}

func NewService(d *db.DB, blobs *storage.Blobs, ops *operations.Store, t *tenancy.Store, log *slog.Logger, staging string, cfg Config) (*Service, error) {
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return nil, err
	}
	return &Service{db: d, blobs: blobs, ops: ops, tenancy: t, log: log, cfg: cfg, staging: staging, sem: make(chan struct{}, cfg.Workers), now: time.Now, owner: &cluster.Solo{}}, nil
}

// UseOwnership records which controller commits each upload, so that only uploads of controllers that are gone are
// treated as interrupted.
func (s *Service) UseOwnership(o cluster.Ownership) { s.owner = o }

// ShareChunks stores chunks in blob storage as well as locally, for controllers behind one load balancer.
func (s *Service) ShareChunks() { s.sharedChunks = true }

func (s *Service) Limits() Limits { return s.cfg.Limits }

func (s *Service) ChunkBytes() int64 { return s.cfg.ChunkBytes }

// CreateSession reserves an upload of totalBytes (the archive size) after checking limits and project access.
func (s *Service) CreateSession(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string, fileCount int, totalBytes int64, excludes []string, requestID string) (Session, error) {
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		return Session{}, err
	}
	switch {
	case totalBytes > s.cfg.Limits.MaxArchiveBytes:
		return Session{}, reject("too_large", "uploads are limited to %d MiB", s.cfg.Limits.MaxArchiveBytes>>20)
	case fileCount > s.cfg.Limits.MaxFiles:
		return Session{}, reject("too_many_files", "uploads are limited to %d files", s.cfg.Limits.MaxFiles)
	}
	if err := ValidatePatterns(excludes); err != nil {
		return Session{}, reject("invalid_exclude", "%s", err.Error())
	}
	pats, _ := json.Marshal(excludes)
	now := s.now().UTC()
	sess := Session{
		ID: ids.New(), ProjectID: projectID, Status: StatusOpen, FileCount: fileCount, TotalBytes: totalBytes,
		ChunkBytes: s.cfg.ChunkBytes, MaxChunks: int((totalBytes + s.cfg.ChunkBytes - 1) / s.cfg.ChunkBytes),
		Received: []int{}, ExpiresAt: now.Add(s.cfg.SessionTTL),
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tenancy.RequireActive(ctx, s.db, tx, projectID); err != nil {
			return err
		}
		var open int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM upload_sessions WHERE created_by = ? AND status IN (?, ?) AND expires_at > ?`), p.ID, StatusOpen, StatusCommitting, now).Scan(&open); err != nil {
			return err
		}
		if open >= s.cfg.MaxOpenSessions {
			return ErrQuota
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO upload_sessions (id, org_id, project_id, created_by, status, file_count, total_bytes, chunk_bytes, max_chunks, exclude_patterns, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), sess.ID, p.OrgID, projectID, p.ID, StatusOpen, fileCount, totalBytes, sess.ChunkBytes, sess.MaxChunks, string(pats), sess.ExpiresAt, now); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "upload.create", ResourceType: "upload", ResourceID: sess.ID, Outcome: "succeeded", RequestID: requestID})
	})
	return sess, err
}

func (s *Service) GetSession(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (Session, error) {
	sess, _, err := s.session(ctx, s.db.SQL, p, roles, id)
	return sess, err
}

// session loads an upload owned by p on a project p can still write to; anything else is not found.
func (s *Service) session(ctx context.Context, q *sql.DB, p authz.Principal, roles []authz.Role, id string) (Session, []string, error) {
	var sess Session
	var owner, pats string
	var op, snap sql.NullString
	err := q.QueryRowContext(ctx, s.db.Rebind(`SELECT project_id, created_by, status, file_count, total_bytes, chunk_bytes, max_chunks, exclude_patterns, operation_id, snapshot_id, expires_at
		FROM upload_sessions WHERE id = ? AND org_id = ?`), id, p.OrgID).Scan(&sess.ProjectID, &owner, &sess.Status, &sess.FileCount, &sess.TotalBytes, &sess.ChunkBytes, &sess.MaxChunks, &pats, &op, &snap, &sess.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != p.ID) {
		return Session{}, nil, ErrNotFound
	}
	if err != nil {
		return Session{}, nil, err
	}
	if _, err := s.tenancy.GetProject(ctx, p, roles, sess.ProjectID); err != nil {
		return Session{}, nil, ErrNotFound
	}
	sess.ID, sess.OperationID, sess.SnapshotID = id, op.String, snap.String
	if sess.Status == StatusOpen && !s.now().Before(sess.ExpiresAt) {
		sess.Status = StatusExpired
	}
	rows, err := q.QueryContext(ctx, s.db.Rebind(`SELECT idx FROM upload_chunks WHERE upload_id = ? ORDER BY idx`), id)
	if err != nil {
		return Session{}, nil, err
	}
	defer rows.Close()
	sess.Received = []int{}
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			return Session{}, nil, err
		}
		sess.Received = append(sess.Received, i)
	}
	var excludes []string
	_ = json.Unmarshal([]byte(pats), &excludes)
	return sess, excludes, rows.Err()
}

func (s *Service) chunkSize(sess Session, index int) int64 {
	if index == sess.MaxChunks-1 {
		return sess.TotalBytes - int64(sess.MaxChunks-1)*sess.ChunkBytes
	}
	return sess.ChunkBytes
}

// PutChunk stores chunk index; every chunk except the last must be exactly ChunkBytes. Re-sending replaces it.
func (s *Service) PutChunk(ctx context.Context, p authz.Principal, roles []authz.Role, id string, index int, body io.Reader) error {
	sess, _, err := s.session(ctx, s.db.SQL, p, roles, id)
	if err != nil {
		return err
	}
	if sess.Status != StatusOpen {
		return ErrState
	}
	if index < 0 || index >= sess.MaxChunks {
		return reject("chunk_index", "chunk index must be between 0 and %d", sess.MaxChunks-1)
	}
	want := s.chunkSize(sess, index)
	dir := filepath.Join(s.staging, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(body, want+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != want {
		return reject("chunk_size", "chunk %d must be exactly %d bytes", index, want)
	}
	digest := "sha256:" + hex.EncodeToString(sum.Sum(nil))
	if s.sharedChunks {
		f, err := os.Open(tmp.Name())
		if err != nil {
			return err
		}
		_, _, err = s.blobs.PutClass(storage.ClassSource, f, want)
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, strconv.Itoa(index))); err != nil {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT status FROM upload_sessions WHERE id = ?`), id).Scan(&status); err != nil {
			return err
		}
		if status != StatusOpen {
			return ErrState
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM upload_chunks WHERE upload_id = ? AND idx = ?`), id, index); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO upload_chunks (upload_id, idx, size, digest) VALUES (?, ?, ?, ?)`), id, index, n, digest)
		return err
	})
}

// Commit starts snapshot creation once every chunk is present. Repeating it with the same key returns the same
// operation instead of starting another.
func (s *Service) Commit(ctx context.Context, p authz.Principal, roles []authz.Role, id, key, requestID string) (operations.Operation, error) {
	sess, excludes, err := s.session(ctx, s.db.SQL, p, roles, id)
	if err != nil {
		return operations.Operation{}, err
	}
	var op operations.Operation
	var start bool
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		existing, created, err := s.ops.Begin(ctx, tx, p, commitKind+":"+id, key)
		if err != nil || !created {
			op = existing
			return err
		}
		if sess.Status != StatusOpen {
			return ErrState
		}
		var chunks int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM upload_chunks WHERE upload_id = ?`), id).Scan(&chunks); err != nil {
			return err
		}
		if chunks != sess.MaxChunks {
			return reject("incomplete", "%d of %d chunks have been received", chunks, sess.MaxChunks)
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE upload_sessions SET status = ?, operation_id = ?, owner_controller = ? WHERE id = ? AND status = ? AND expires_at > ?`), StatusCommitting, existing.ID, s.owner.ControllerID(), id, StatusOpen, s.now().UTC())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrState
		}
		op, start = existing, true
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "upload.commit", ResourceType: "upload", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
	if err != nil || !start {
		return op, err
	}
	s.wg.Add(1)
	go s.process(p, id, op.ID, excludes, requestID)
	return op, nil
}

// Wait blocks until background snapshot creation finishes.
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) process(p authz.Principal, id, opID string, excludes []string, requestID string) {
	defer s.wg.Done()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx := context.Background()
	dir := filepath.Join(s.staging, id)
	defer os.RemoveAll(dir)

	if err := s.db.InTx(ctx, func(tx *sql.Tx) error { return s.ops.SetStatus(ctx, tx, opID, operations.Running) }); err != nil {
		s.log.Error("upload commit", "upload", id, "error", err)
	}
	snap, err := s.build(ctx, id, dir, excludes)
	if err == nil {
		snap.ID, snap.ProjectID, snap.CreatedBy, snap.CreatedAt = ids.New(), "", p.ID, s.now().UTC()
		err = s.db.InTx(ctx, func(tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT project_id FROM upload_sessions WHERE id = ?`), id).Scan(&snap.ProjectID); err != nil {
				return err
			}
			if err := s.insertSnapshot(ctx, tx, p, snap, requestID); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE upload_sessions SET status = ?, snapshot_id = ? WHERE id = ? AND status = ? AND owner_controller = ?`), StatusCommitted, snap.ID, id, StatusCommitting, s.owner.ControllerID())
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errSuperseded
			}
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM upload_chunks WHERE upload_id = ?`), id); err != nil {
				return err
			}
			return s.ops.Finish(ctx, tx, opID, operations.Succeeded, "snapshot", snap.ID, "", "")
		})
	}
	if err == nil || errors.Is(err, errSuperseded) {
		return
	}
	code, msg := "internal", "the upload could not be processed; quote operation "+opID+" to an operator"
	var r *Rejection
	if errors.As(err, &r) {
		code, msg = r.Code, r.Message
	} else {
		s.log.Error("upload commit failed", "upload", id, "operation", opID, "error", err)
	}
	if ferr := s.fail(ctx, p, id, opID, code, msg, requestID); ferr != nil {
		s.log.Error("recording upload failure", "upload", id, "error", ferr)
	}
}

// errSuperseded means another controller took over the commit because this one was presumed gone.
var errSuperseded = errors.New("the upload commit was taken over by another controller")

func (s *Service) fail(ctx context.Context, p authz.Principal, id, opID, code, msg, requestID string) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE upload_sessions SET status = ? WHERE id = ? AND status = ?`), StatusFailed, id, StatusCommitting)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM upload_chunks WHERE upload_id = ?`), id); err != nil {
			return err
		}
		if err := s.ops.Finish(ctx, tx, opID, operations.Failed, "upload", id, code, msg); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "snapshot.create", ResourceType: "upload", ResourceID: id, Outcome: "failed", Reason: code, RequestID: requestID})
	})
}

func (s *Service) insertSnapshot(ctx context.Context, tx *sql.Tx, p authz.Principal, snap Snapshot, requestID string) error {
	if err := tenancy.RequireActive(ctx, s.db, tx, snap.ProjectID); err != nil {
		return err
	}
	warnings, _ := json.Marshal(snap.Warnings)
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO source_snapshots (id, org_id, project_id, digest, file_count, total_bytes, rejected_entries, excluded_entries, warnings, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), snap.ID, p.OrgID, snap.ProjectID, snap.Digest, snap.FileCount, snap.TotalBytes, snap.Rejected, snap.Excluded, string(warnings), p.ID, snap.CreatedAt); err != nil {
		return err
	}
	return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "snapshot.create", ResourceType: "snapshot", ResourceID: snap.ID, Outcome: "succeeded", Reason: snap.Digest, RequestID: requestID})
}

// Ingest creates a snapshot from an archive Samuori produced itself, such as a fetched Git commit. The archive passes the
// same entry rules, exclusions, limits and preflight as an upload; a *Rejection explains a refused archive.
func (s *Service) Ingest(ctx context.Context, p authz.Principal, roles []authz.Role, projectID, archive, requestID string) (Snapshot, error) {
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		return Snapshot{}, err
	}
	snap, err := s.snapshot(archive, nil)
	if err != nil {
		return Snapshot{}, err
	}
	snap.ID, snap.ProjectID, snap.CreatedBy, snap.CreatedAt = ids.New(), projectID, p.ID, s.now().UTC()
	return snap, s.db.InTx(ctx, func(tx *sql.Tx) error { return s.insertSnapshot(ctx, tx, p, snap, requestID) })
}

// Staging is a directory for archives on their way to Ingest.
func (s *Service) Staging() string { return s.staging }

// build joins the chunks in order. A chunk received by another controller is read from blob storage and verified
// against the digest recorded when it arrived.
func (s *Service) build(ctx context.Context, id, dir string, excludes []string) (Snapshot, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Snapshot{}, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT idx, COALESCE(digest, '') FROM upload_chunks WHERE upload_id = ? ORDER BY idx`), id)
	if err != nil {
		return Snapshot{}, err
	}
	var digests []string
	for rows.Next() {
		var idx int
		var d string
		if err := rows.Scan(&idx, &d); err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		if idx != len(digests) {
			rows.Close()
			return Snapshot{}, fmt.Errorf("upload %s: chunk %d is missing", id, len(digests))
		}
		digests = append(digests, d)
	}
	rows.Close()
	archive := filepath.Join(dir, "archive")
	out, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Snapshot{}, err
	}
	for i, d := range digests {
		var in io.ReadCloser
		in, err = os.Open(filepath.Join(dir, strconv.Itoa(i)))
		if errors.Is(err, os.ErrNotExist) && s.sharedChunks && d != "" {
			in, err = s.blobs.Open(d)
		}
		if err != nil {
			out.Close()
			return Snapshot{}, err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			out.Close()
			return Snapshot{}, err
		}
	}
	if err := out.Close(); err != nil {
		return Snapshot{}, err
	}
	return s.snapshot(archive, excludes)
}

func (s *Service) snapshot(archive string, excludes []string) (Snapshot, error) {
	res, err := Extract(archive, s.cfg.Limits, excludes, s.blobs)
	if err != nil {
		return Snapshot{}, err
	}
	canonical := res.Manifest.Canonical()
	digest, _, err := s.blobs.PutClass(storage.ClassSource, bytes.NewReader(canonical), int64(len(canonical)))
	if err != nil {
		return Snapshot{}, err
	}
	if digest != res.Manifest.Digest() {
		return Snapshot{}, fmt.Errorf("manifest digest mismatch")
	}
	if res.Warnings == nil {
		res.Warnings = []Warning{}
	}
	return Snapshot{Digest: digest, FileCount: len(res.Manifest.Files), TotalBytes: res.TotalBytes, Rejected: res.Rejected, Excluded: res.Excluded, Warnings: res.Warnings}, nil
}

func (s *Service) ListSnapshots(ctx context.Context, p authz.Principal, roles []authz.Role, projectID, cursor string, limit int) ([]Snapshot, error) {
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectSnapshot+` WHERE project_id = ? AND org_id = ? AND id < ? AND d.snapshot_id IS NULL ORDER BY id DESC LIMIT ?`), projectID, p.OrgID, cursorOrMax(cursor), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

func (s *Service) GetSnapshot(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (Snapshot, error) {
	snap, err := scanSnapshot(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectSnapshot+` WHERE id = ? AND org_id = ?`), id, p.OrgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := s.tenancy.GetProject(ctx, p, roles, snap.ProjectID); err != nil {
		return Snapshot{}, ErrNotFound
	}
	return snap, nil
}

// SnapshotByID loads a snapshot without an access check; callers must have authorized the request another way,
// such as a worker's current lease.
func (s *Service) SnapshotByID(ctx context.Context, id string) (Snapshot, error) {
	snap, err := scanSnapshot(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectSnapshot+` WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	return snap, err
}

// Manifest loads and verifies a snapshot's file list; tampered content fails with storage.ErrCorrupt.
func (s *Service) Manifest(snap Snapshot) (Manifest, error) {
	if snap.DeletedAt != nil {
		return Manifest{}, ErrDeleted
	}
	raw, err := s.blobs.ReadAll(snap.Digest, 256<<20)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, err
	}
	if m.Digest() != snap.Digest {
		return Manifest{}, storage.ErrCorrupt
	}
	return m, nil
}

// DeleteSnapshot deletes an upload for the person who made it, if they can still reach its project. The record
// stays, marked deleted, so builds keep their history; the returned digests (its manifest and files) are for the
// caller to remove from storage where nothing else uses them (ADR 0040).
func (s *Service) DeleteSnapshot(ctx context.Context, p authz.Principal, roles []authz.Role, id, requestID string) ([]string, error) {
	snap, err := s.GetSnapshot(ctx, p, roles, id)
	if err != nil {
		return nil, err
	}
	if snap.DeletedAt != nil {
		return nil, ErrNotFound
	}
	if snap.CreatedBy != p.ID {
		return nil, ErrNotUploader
	}
	m, err := s.Manifest(snap)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	digests := []string{snap.Digest}
	for _, f := range m.Files {
		digests = append(digests, f.Digest)
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var running int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM build_runs WHERE snapshot_id = ? AND state NOT IN ('succeeded', 'blocked', 'failed', 'timed_out', 'cancelled', 'expired', 'discarded')`), id).Scan(&running); err != nil {
			return err
		}
		if running > 0 {
			return ErrInUse
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO source_snapshot_deletions (snapshot_id, deleted_by, deleted_at) VALUES (?, ?, ?)`), id, p.ID, s.now().UTC()); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "snapshot.delete", ResourceType: "snapshot", ResourceID: id, Outcome: "succeeded",
			Reason: fmt.Sprintf("%s, %d files", snap.Digest, snap.FileCount), RequestID: requestID})
	})
	if err != nil {
		return nil, err
	}
	return digests, nil
}

// ReadFile returns a snapshot file's verified content.
func (s *Service) ReadFile(f File) ([]byte, error) { return s.blobs.ReadAll(f.Digest, f.Size) }

// Reconcile fails commits whose controller is gone; they must be uploaded again because staging is not durable.
// Commits of live controllers are left alone.
func (s *Service) Reconcile(ctx context.Context) error {
	orphaned, args := s.owner.Orphaned("owner_controller")
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, org_id, created_by, operation_id FROM upload_sessions WHERE status = ? AND `+orphaned), append([]any{StatusCommitting}, args...)...)
	if err != nil {
		return err
	}
	type stuck struct{ id, org, owner, op string }
	var list []stuck
	for rows.Next() {
		var x stuck
		var op sql.NullString
		if err := rows.Scan(&x.id, &x.org, &x.owner, &op); err != nil {
			rows.Close()
			return err
		}
		x.op = op.String
		list = append(list, x)
	}
	rows.Close()
	for _, x := range list {
		p := authz.Principal{ID: x.owner, OrgID: x.org, Kind: authz.User}
		if err := s.fail(ctx, p, x.id, x.op, "interrupted", "the server restarted while processing this upload; upload it again", ""); err != nil {
			return err
		}
		_ = os.RemoveAll(filepath.Join(s.staging, x.id))
	}
	return s.Expire(ctx)
}

// Expire closes open uploads past their deadline and removes their staged chunks.
func (s *Service) Expire(ctx context.Context) error {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id FROM upload_sessions WHERE status = ? AND expires_at <= ?`), StatusOpen, s.now().UTC())
	if err != nil {
		return err
	}
	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, id)
	}
	rows.Close()
	for _, id := range expired {
		err := s.db.InTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE upload_sessions SET status = ? WHERE id = ? AND status = ?`), StatusExpired, id, StatusOpen); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM upload_chunks WHERE upload_id = ?`), id)
			return err
		})
		if err != nil {
			return err
		}
		_ = os.RemoveAll(filepath.Join(s.staging, id))
	}
	return nil
}

// ExpireProject closes a project's open uploads and removes their staged chunks, for an archived project (ADR 0058).
func (s *Service) ExpireProject(ctx context.Context, projectID string) (int, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id FROM upload_sessions WHERE project_id = ? AND status = ?`), projectID, StatusOpen)
	if err != nil {
		return 0, err
	}
	var open []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		open = append(open, id)
	}
	rows.Close()
	n := 0
	for _, id := range open {
		err := s.db.InTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE upload_sessions SET status = ? WHERE id = ? AND status = ?`), StatusExpired, id, StatusOpen)
			if err != nil {
				return err
			}
			if c, _ := res.RowsAffected(); c == 1 {
				n++
			}
			_, err = tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM upload_chunks WHERE upload_id = ?`), id)
			return err
		})
		if err != nil {
			return n, err
		}
		_ = os.RemoveAll(filepath.Join(s.staging, id))
	}
	return n, nil
}

// RunJanitor expires stale uploads until ctx ends.
func (s *Service) RunJanitor(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("reconciling uploads", "error", err)
			}
		}
	}
}

const selectSnapshot = `SELECT id, project_id, digest, file_count, total_bytes, rejected_entries, excluded_entries, warnings, created_by, created_at, COALESCE(d.deleted_by, ''), d.deleted_at
	FROM source_snapshots LEFT JOIN source_snapshot_deletions d ON d.snapshot_id = source_snapshots.id`

func scanSnapshot(r interface{ Scan(...any) error }) (Snapshot, error) {
	var snap Snapshot
	var warnings string
	var deleted sql.NullTime
	err := r.Scan(&snap.ID, &snap.ProjectID, &snap.Digest, &snap.FileCount, &snap.TotalBytes, &snap.Rejected, &snap.Excluded, &warnings, &snap.CreatedBy, &snap.CreatedAt, &snap.DeletedBy, &deleted)
	if err == nil {
		err = json.Unmarshal([]byte(warnings), &snap.Warnings)
	}
	if deleted.Valid {
		snap.DeletedAt = &deleted.Time
	}
	return snap, err
}

func cursorOrMax(c string) string {
	if c == "" {
		return "\U0010FFFF"
	}
	return c
}
