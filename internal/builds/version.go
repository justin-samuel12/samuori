package builds

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// assignVersion gives a build that just succeeded its version, <UTC date>.<n>, counting the project's successful
// builds that day (ADR 0054). It runs in the transaction that marks the build succeeded; the counter row serializes
// controllers finishing builds of one project at once.
func (s *Service) assignVersion(ctx context.Context, tx *sql.Tx, runID, projectID string, at time.Time) (string, error) {
	day := at.UTC().Format("2006-01-02")
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO build_version_counters (project_id, day, last) VALUES (?, ?, 0) ON CONFLICT (project_id, day) DO NOTHING`), projectID, day); err != nil {
		return "", err
	}
	var n int
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`UPDATE build_version_counters SET last = last + 1 WHERE project_id = ? AND day = ? RETURNING last`), projectID, day).Scan(&n); err != nil {
		return "", err
	}
	v := fmt.Sprintf("%s.%d", day, n)
	_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE build_runs SET version = ? WHERE id = ?`), v, runID)
	return v, err
}
