package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"samuori.invalid/samuori/internal/storage"
)

// GCResult reports one collection; Kept counts blobs spared only by the grace period.
type GCResult struct {
	Deleted, Kept, Referenced int
	DroppedArtifacts          int64
}

type GCPolicy struct {
	// Grace protects blobs written recently, such as uploads that are not yet recorded.
	Grace time.Duration
	// FailedRetention is how long outputs of attempts that did not complete are kept.
	FailedRetention time.Duration
}

func DefaultGCPolicy() GCPolicy {
	return GCPolicy{Grace: time.Hour, FailedRetention: 7 * 24 * time.Hour}
}

// Collect deletes blobs that nothing references: not a snapshot, file or artifact still in use, not held, and not
// written within the grace period. Holds always win (docs/architecture/registry-and-artifacts.md#garbage-collection).
func (s *Store) Collect(ctx context.Context, p GCPolicy) (GCResult, error) {
	var res GCResult
	cutoff := s.now().UTC().Add(-p.FailedRetention)
	r, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`DELETE FROM build_artifacts WHERE created_at < ? AND attempt_id IN (SELECT id FROM build_attempts WHERE state IN ('failed', 'abandoned', 'superseded'))
		AND digest NOT IN (SELECT digest FROM artifact_holds WHERE released_at IS NULL)`), cutoff)
	if err != nil {
		return res, err
	}
	res.DroppedArtifacts, _ = r.RowsAffected()
	if err := s.pruneCaches(ctx); err != nil {
		return res, err
	}

	keep, err := s.references(ctx)
	if err != nil {
		return res, err
	}
	res.Referenced = len(keep)
	grace := s.now().Add(-p.Grace)
	err = s.blobs.Walk(func(d string, mod time.Time, _ int64) error {
		if mod.IsZero() && !keep[d] {
			t, ok := s.blobs.ModTime(d)
			if !ok {
				res.Kept++
				return nil
			}
			mod = t
		}
		switch {
		case keep[d]:
		case mod.After(grace):
			res.Kept++
		default:
			if err := s.blobs.Delete(d); err != nil {
				return err
			}
			res.Deleted++
		}
		return nil
	})
	return res, err
}

// RemoveUnreferenced deletes those of the given blobs that nothing references any more, at once and without the grace
// period, for content its owner deleted; blobs another snapshot, artifact or hold uses are kept. It reports how many
// stored blobs it removed and how many it kept because they are referenced.
func (s *Store) RemoveUnreferenced(ctx context.Context, digests []string) (removed, kept int, err error) {
	keep, err := s.references(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, d := range digests {
		if keep[d] {
			kept++
			continue
		}
		_, stored := s.blobs.ModTime(d)
		if err := s.blobs.Delete(d); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return removed, kept, err
		}
		if stored {
			removed++
		}
	}
	return removed, kept, nil
}

// references is every blob something still uses: holds, artifacts, publications, open uploads, validation outputs of
// projects that are not purged, and the manifests and files of snapshots that are not deleted.
func (s *Store) references(ctx context.Context) (map[string]bool, error) {
	keep := map[string]bool{}
	for _, q := range []string{
		`SELECT digest FROM artifact_holds WHERE released_at IS NULL`,
		`SELECT digest FROM build_artifacts WHERE run_id NOT IN (SELECT run_id FROM build_run_deletions)`,
		`SELECT artifact_digest FROM build_runs WHERE artifact_digest IS NOT NULL AND id NOT IN (SELECT run_id FROM build_run_deletions)`,
		`SELECT output_digest FROM check_results WHERE output_digest IS NOT NULL AND validation_run_id NOT IN (SELECT v.id FROM validation_runs v JOIN projects p ON p.id = v.project_id WHERE p.state = 'purged')`,
		`SELECT artifact_digest FROM publications WHERE artifact_id NOT IN (SELECT a.id FROM build_artifacts a JOIN build_run_deletions d ON d.run_id = a.run_id)`,
		`SELECT digest FROM upload_chunks WHERE digest IS NOT NULL`,
		`SELECT digest FROM releases`,
	} {
		if err := s.collect(ctx, q, keep); err != nil {
			return nil, err
		}
	}
	manifests := map[string]bool{}
	if err := s.collect(ctx, `SELECT digest FROM source_snapshots WHERE id NOT IN (SELECT snapshot_id FROM source_snapshot_deletions)`, manifests); err != nil {
		return nil, err
	}
	for d := range manifests {
		keep[d] = true
		raw, err := s.blobs.ReadAll(d, 256<<20)
		if err != nil {
			return nil, err
		}
		var m struct {
			Files []struct {
				Digest string `json:"digest"`
			} `json:"files"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		for _, f := range m.Files {
			keep[f.Digest] = true
		}
	}
	return keep, nil
}

func (s *Store) collect(ctx context.Context, q string, into map[string]bool) error {
	rows, err := s.db.SQL.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return err
		}
		into[d] = true
	}
	return rows.Err()
}

type Usage struct {
	Blobs, Bytes, Held int64
}

// Usage measures the blob store and counts active holds.
func (s *Store) Usage(ctx context.Context) (Usage, error) {
	var u Usage
	err := s.blobs.Walk(func(_ string, _ time.Time, size int64) error {
		u.Blobs++
		u.Bytes += size
		return nil
	})
	if err != nil {
		return u, err
	}
	err = s.db.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_holds WHERE released_at IS NULL`).Scan(&u.Held)
	return u, err
}

// Route returns the backend a class is routed to, or nil when it is kept locally.
func (s *Store) Route(c storage.Class) storage.Remote { return s.blobs.RouteOf(c) }
