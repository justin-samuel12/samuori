package artifacts

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

// TestDeletedBuildsAreNotKept: garbage collection stops keeping a deleted build's image, logs and failed
// publications, but keeps content another build still references (ADR 0053).
func TestDeletedBuildsAreNotKept(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, dbtest.SQLite(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	blobs, err := storage.NewBlobs(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(d, blobs, filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, q := range []string{
		`PRAGMA foreign_keys = OFF`,
		`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES ('a1', 'o1', 'r1', 't1', 1, 'image', 'sha256:only-deleted', 1, ?)`,
		`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES ('a2', 'o1', 'r1', 't1', 1, 'log', 'sha256:log-deleted', 1, ?)`,
		`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES ('a3', 'o1', 'r1', 't1', 1, 'image', 'sha256:shared', 1, ?)`,
		`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES ('a4', 'o1', 'r2', 't2', 1, 'image', 'sha256:shared', 1, ?)`,
		`INSERT INTO publications (id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, gate_decisions, requested_by, idempotency_key, created_at, updated_at) VALUES ('p1', 'o1', 'p', 'publish', 'a1', 'sha256:only-deleted', 'd', '[]', 'failed', '[]', 'u', 'k', ?, ?)`,
		`INSERT INTO build_run_deletions (run_id, org_id, deleted_by, reason, deleted_at) VALUES ('r1', 'o1', 'u', '', ?)`,
	} {
		args := []any{}
		for range countPlaceholders(q) {
			args = append(args, now)
		}
		if _, err := d.SQL.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(q, err)
		}
	}
	keep, err := s.references(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if keep["sha256:only-deleted"] || keep["sha256:log-deleted"] || !keep["sha256:shared"] {
		t.Errorf("kept = %v", keep)
	}
}

// TestOperatorLogoIsReferenced: the operator logo blobs are referenced, so garbage collection does not delete the
// branding image (ADR 0072). Without this the login page would lose its logo after the grace period.
func TestOperatorLogoIsReferenced(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, dbtest.SQLite(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	blobs, err := storage.NewBlobs(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(d, blobs, filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, q := range []string{
		`PRAGMA foreign_keys = OFF`,
		`INSERT INTO branding (org_id, logo_digest, logo_content_type, logo_dark_digest, updated_by, updated_at) VALUES ('o1', 'sha256:logo-primary', 'image/svg+xml', 'sha256:logo-dark', 'u', ?)`,
	} {
		args := []any{}
		for range countPlaceholders(q) {
			args = append(args, now)
		}
		if _, err := d.SQL.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(q, err)
		}
	}
	keep, err := s.references(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !keep["sha256:logo-primary"] || !keep["sha256:logo-dark"] {
		t.Errorf("operator logo not referenced: %v", keep)
	}
}

func countPlaceholders(q string) int {
	n := 0
	for _, c := range q {
		if c == '?' {
			n++
		}
	}
	return n
}
