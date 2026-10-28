// Package selfupdate keeps signed update bundles and rolls them out: controllers one at a time with the leader last,
// then workers in waves per pool, each drained, updated and self-tested. Hosts apply a release only after verifying
// its signature themselves (ADRs 0044 and 0045, docs/operators/upgrades.md#self-update).
package selfupdate

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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/updatebundle"
	"samuori.invalid/samuori/internal/updatestage"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
	"samuori.invalid/samuori/internal/workers"
)

const (
	SignersFile = "/etc/samuori/release-signers"
	principal   = "release@samuori.invalid"
	namespace   = "samuori-release"
)

var (
	ErrForbidden  = errors.New("only administrators manage releases and rollouts")
	ErrNoSigners  = errors.New("this controller has no release signers (" + SignersFile + "); install it with --release-signers")
	ErrExists     = errors.New("this release is already uploaded")
	ErrNotFound   = errors.New("not found")
	ErrRunning    = errors.New("a rollout is already running")
	systemWorkers = func(org string) authz.Principal {
		return authz.Principal{ID: "system:self-update", OrgID: org, Kind: authz.System}
	}
)

// Invalid is a refused request; Reason is safe to show.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

// Workers is what a rollout needs from the worker service.
type Workers interface {
	ApplyAction(ctx context.Context, actor authz.Principal, id, action, reason string, compromisedSince *time.Time, requestID string) (workers.Worker, error)
	Send(workerID string, m *workerv1.ControllerMessage) error
	Connected(id string) bool
	RequestWorkerSelfTest(ctx context.Context, actor authz.Principal, workerID, requestID string) (workers.SelfTest, error)
}

type Service struct {
	db       *db.DB
	blobs    *storage.Blobs
	member   *cluster.Member
	workers  Workers
	signers  string
	stageDir string
	arch     string
	log      *slog.Logger
	now      func() time.Time

	changesMu sync.Mutex
	changes   map[string][]updatebundle.Change
}

func New(d *db.DB, blobs *storage.Blobs, member *cluster.Member, w Workers, arch string, log *slog.Logger) *Service {
	return &Service{db: d, blobs: blobs, member: member, workers: w, signers: SignersFile, stageDir: updatestage.Dir, arch: arch, log: log, now: time.Now}
}

// Release is one uploaded version and the architectures its bundle holds.
type Release struct {
	Version        string
	Architectures  []string
	Size           int64
	WorkerProtocol *updatebundle.Protocol
	Changes        []updatebundle.Change
	// Bundled is false for releases uploaded as separate files before bundles; they cannot be rolled out.
	Bundled    bool
	UploadedBy string
	UploadedAt time.Time
}

// SignersConfigured reports whether this controller can verify releases.
func (s *Service) SignersConfigured() bool {
	fi, err := os.Stat(s.signers)
	return err == nil && fi.Size() > 0
}

// verify checks the SSH signature over the checksums against this host's signers.
func (s *Service) verify(checksums, signature []byte) error {
	dir, err := os.MkdirTemp("", "samuori-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sig := filepath.Join(dir, "SHA256SUMS.sig")
	if err := os.WriteFile(sig, signature, 0o600); err != nil {
		return err
	}
	cmd := exec.Command("ssh-keygen", "-Y", "verify", "-f", s.signers, "-I", principal, "-n", namespace, "-s", sig)
	cmd.Stdin = bytes.NewReader(checksums)
	if out, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("ssh-keygen is not installed on this controller")
		}
		return &Invalid{"the signature does not match a key in " + s.signers + ": " + strings.TrimSpace(string(out))}
	}
	return nil
}

// Upload verifies an update bundle from make update — its structure, checksums and signature — and keeps each
// architecture's archive in blob storage. f must be seekable; it is read more than once.
func (s *Service) Upload(ctx context.Context, actor authz.Principal, f io.ReadSeeker, requestID string) (Release, error) {
	if !actor.IsOrgAdmin() {
		return Release{}, ErrForbidden
	}
	if !s.SignersConfigured() {
		return Release{}, ErrNoSigners
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Release{}, err
	}
	b, err := updatebundle.Read(f)
	var inv *updatebundle.Invalid
	if errors.As(err, &inv) {
		return Release{}, &Invalid{inv.Reason}
	}
	if err != nil {
		return Release{}, err
	}
	if err := s.verify(b.Sums, b.Signature); err != nil {
		return Release{}, err
	}
	m := b.Manifest
	var n int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM releases WHERE version = ?`), m.Version).Scan(&n); err != nil {
		return Release{}, err
	}
	if n > 0 {
		return Release{}, ErrExists
	}
	type kept struct {
		arch, digest string
		size         int64
	}
	var blobs []kept
	var total int64
	for _, a := range b.Archives {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return Release{}, err
		}
		var k kept
		err := updatebundle.Extract(f, a.Arch, m.Version, func(r io.Reader) error {
			h := sha256.New()
			digest, size, err := s.blobs.PutClass(storage.ClassOther, io.TeeReader(r, h), updatebundle.MaxArchive)
			if err != nil {
				return err
			}
			if hex.EncodeToString(h.Sum(nil)) != a.Digest {
				return &Invalid{a.Name + " changed while it was stored"}
			}
			k = kept{a.Arch, digest, size}
			return nil
		})
		if err != nil {
			return Release{}, err
		}
		blobs = append(blobs, k)
		total += k.size
	}
	now := s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM releases WHERE version = ?`), m.Version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrExists
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO release_bundles (version, manifest, checksums, signature, size, uploaded_by, uploaded_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			m.Version, b.ManifestRaw, b.Sums, b.Signature, total, actor.ID, now); err != nil {
			return err
		}
		for _, k := range blobs {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO releases (version, arch, digest, size, checksums, signature, uploaded_by, uploaded_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
				m.Version, k.arch, k.digest, k.size, b.Sums, b.Signature, actor.ID, now); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "release.upload", ResourceType: "release", ResourceID: m.Version, Outcome: "succeeded", Reason: strings.Join(m.Architectures, ","), RequestID: requestID})
	})
	if err != nil {
		return Release{}, err
	}
	p := m.WorkerProtocol
	return Release{Version: m.Version, Architectures: m.Architectures, Size: total, WorkerProtocol: &p, Bundled: true, UploadedBy: actor.ID, UploadedAt: now}, nil
}

func (s *Service) Releases(ctx context.Context, actor authz.Principal) ([]Release, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT r.version, r.arch, r.digest, r.size, r.uploaded_by, r.uploaded_at, b.manifest FROM releases r
		LEFT JOIN release_bundles b ON b.version = r.version ORDER BY r.uploaded_at DESC, r.version, r.arch`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Release{}
	index := map[string]int{}
	for rows.Next() {
		var r Release
		var arch, digest string
		var size int64
		var manifest []byte
		if err := rows.Scan(&r.Version, &arch, &digest, &size, &r.UploadedBy, &r.UploadedAt, &manifest); err != nil {
			return nil, err
		}
		i, ok := index[r.Version]
		if !ok {
			if manifest != nil {
				var m updatebundle.Manifest
				if json.Unmarshal(manifest, &m) == nil {
					r.WorkerProtocol = &m.WorkerProtocol
				}
				r.Changes = s.releaseChanges(digest)
				r.Bundled = true
			}
			r.Architectures = []string{}
			out = append(out, r)
			i = len(out) - 1
			index[r.Version] = i
		}
		out[i].Architectures = append(out[i].Architectures, arch)
		out[i].Size += size
	}
	return out, rows.Err()
}

// releaseChanges reads the change history from a release archive once; archives never change.
func (s *Service) releaseChanges(digest string) []updatebundle.Change {
	s.changesMu.Lock()
	defer s.changesMu.Unlock()
	if c, ok := s.changes[digest]; ok {
		return c
	}
	in, err := s.blobs.Open(digest)
	if err != nil {
		return nil
	}
	defer in.Close()
	c, err := updatebundle.ReadChanges(in)
	if err != nil {
		s.log.Warn("release change history", "digest", digest, "error", err)
		return nil
	}
	if s.changes == nil {
		s.changes = map[string][]updatebundle.Change{}
	}
	s.changes[digest] = c
	return c
}

// DeleteRelease removes a version that no running rollout uses; its blobs go with the next garbage collection.
func (s *Service) DeleteRelease(ctx context.Context, actor authz.Principal, version, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM rollouts WHERE state = 'running' AND version = ?`), version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return &Invalid{"a running rollout uses this release"}
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM releases WHERE version = ?`), version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM release_bundles WHERE version = ?`), version); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "release.delete", ResourceType: "release", ResourceID: version, Outcome: "succeeded", RequestID: requestID})
	})
}

func clip(s string) string {
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

// Run advances rollouts while this controller leads, reports this host's architecture and free update space, and
// stages this controller's own updates.
func (s *Service) Run(ctx context.Context) {
	go s.member.Singleton(ctx, "self-update", 5*time.Second, func(ctx context.Context, _ int64) error { return s.advance(ctx) })
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		if err := s.reportHost(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("self-update host report", "error", err)
		}
		if err := s.stageOwn(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("self-update staging", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) reportHost(ctx context.Context) error {
	var free sql.NullInt64
	if n, err := updatestage.FreeBytes(s.stageDir); err == nil {
		free = sql.NullInt64{Int64: int64(min(n, 1<<62)), Valid: true}
	}
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE controllers SET arch = ?, update_free_bytes = ? WHERE id = ?`), s.arch, free, s.member.ID)
	return err
}

// stageOwn hands this controller's release to the host's samuori-self-update, and records how a previous one ended.
func (s *Service) stageOwn(ctx context.Context) error {
	if err := s.collectResult(ctx); err != nil {
		return err
	}
	var rolloutID, version string
	var seq int
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT r.id, r.version, st.seq FROM rollout_steps st JOIN rollouts r ON r.id = st.rollout_id
		WHERE r.state = 'running' AND st.kind = 'controller' AND st.target = ? AND st.state = 'requested'`), s.member.Hostname).Scan(&rolloutID, &version, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.stage(ctx, version, s.arch); err != nil {
		return s.setStep(ctx, rolloutID, seq, "failed", err.Error())
	}
	return s.setStep(ctx, rolloutID, seq, "applying", "staged; the host is installing "+version)
}

// stage writes a release, its checksums and signature, and then the request, which starts samuori-self-update.
func (s *Service) stage(ctx context.Context, version, arch string) error {
	if fi, err := os.Stat(s.stageDir); err != nil || !fi.IsDir() {
		return errors.New("this host was installed without self-update (" + s.stageDir + " is missing); reinstall it once with install.sh --release-signers")
	}
	var digest string
	var sums, sig []byte
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT digest, checksums, signature FROM releases WHERE version = ? AND arch = ?`), version, arch).Scan(&digest, &sums, &sig)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no %s release of %s is uploaded", arch, version)
	}
	if err != nil {
		return err
	}
	in, err := s.blobs.Open(digest)
	if err != nil {
		return err
	}
	defer in.Close()
	return updatestage.Write(s.stageDir, version, in, sums, sig)
}

func (s *Service) collectResult(ctx context.Context) error {
	res, ok := updatestage.TakeResult(s.stageDir)
	if !ok || res.Outcome != "failed" {
		return nil
	}
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE rollout_steps SET state = 'failed', detail = ?, updated_at = ?
		WHERE kind = 'controller' AND target = ? AND state = 'applying' AND rollout_id IN (SELECT id FROM rollouts WHERE state = 'running')`),
		clip(res.Detail), s.now().UTC(), s.member.Hostname)
	return err
}

// OpenRelease serves a release to a worker the running rollout is updating now (workers.Releases).
func (s *Service) OpenRelease(ctx context.Context, workerID, version, arch string) ([]byte, []byte, io.ReadCloser, error) {
	var rolloutID string
	var seq int
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT r.id, st.seq FROM rollout_steps st JOIN rollouts r ON r.id = st.rollout_id
		WHERE r.state = 'running' AND r.version = ? AND st.kind = 'worker' AND st.target = ? AND st.state IN ('requested', 'applying')`), version, workerID).Scan(&rolloutID, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil, workers.ErrNotUpdateTarget
	}
	if err != nil {
		return nil, nil, nil, err
	}
	var digest string
	var sums, sig []byte
	err = s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT digest, checksums, signature FROM releases WHERE version = ? AND arch = ?`), version, arch).Scan(&digest, &sums, &sig)
	if errors.Is(err, sql.ErrNoRows) {
		_ = s.setStep(ctx, rolloutID, seq, "failed", "no "+arch+" release of "+version+" is uploaded")
		return nil, nil, nil, workers.ErrNotUpdateTarget
	}
	if err != nil {
		return nil, nil, nil, err
	}
	in, err := s.blobs.Open(digest)
	if err != nil {
		return nil, nil, nil, err
	}
	_ = s.setStep(ctx, rolloutID, seq, "applying", "the worker is fetching and installing "+version)
	return sums, sig, in, nil
}

// ReportUpdate records a failure the worker's host reported; success shows as the worker's new version.
func (s *Service) ReportUpdate(ctx context.Context, workerID, version, outcome, detail string) error {
	if outcome != "failed" {
		return nil
	}
	// A worker on a controller's host shares the host's update directory, so it may pick up the result of its
	// controller step's install (ADR 0046, OD-28).
	if version == updatestage.Rollback {
		_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE rollout_steps SET state = 'failed', detail = ?, updated_at = ?
			WHERE ((kind = 'worker' AND target = ?) OR (kind = 'controller' AND local_worker_id = ?)) AND state = 'rolling_back' AND rollout_id IN (SELECT id FROM rollouts WHERE state = 'running')`),
			clip("the self-test failed and rolling back failed too: "+detail), s.now().UTC(), workerID, workerID)
		return err
	}
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE rollout_steps SET state = 'failed', detail = ?, updated_at = ?
		WHERE ((kind = 'worker' AND target = ?) OR (kind = 'controller' AND local_worker_id = ?)) AND state IN ('requested', 'applying') AND rollout_id IN (SELECT id FROM rollouts WHERE state = 'running' AND version = ?)`),
		clip(detail), s.now().UTC(), workerID, workerID, version)
	return err
}
