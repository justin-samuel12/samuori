package builds

import (
	"context"
	"errors"

	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
	"samuori.invalid/samuori/internal/workers"
)

// Live logs (docs/architecture/build-lifecycle.md#live-logs) hold at most this much output per attempt, in chunks of
// at most MaxLiveChunk.
const (
	MaxLiveLog   = 2 << 20
	MaxLiveChunk = 64 << 10
)

// AppendLog stores build output a worker sends while its attempt runs. Only the worker holding the current lease may
// append, chunks must follow on from what is stored, and builds that use secrets get no live log: their output is
// masked only when the attempt ends. stop tells the worker to send no more.
func (sc *Scheduler) AppendLog(ctx context.Context, workerID string, c *workerv1.LogChunk) (stop bool, err error) {
	r, err := sc.current(ctx, sc.s.db.SQL, workerID, c.GetLease())
	if errors.Is(err, workers.ErrStaleLease) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if r.SecretBearing {
		return true, nil
	}
	data := c.GetData()
	if len(data) == 0 || len(data) > MaxLiveChunk {
		return false, errors.New("a live log chunk must hold 1 byte to 64 KiB")
	}
	var stored int64
	if err := sc.s.db.SQL.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT COALESCE(SUM(LENGTH(data)), 0) FROM build_live_logs WHERE attempt_id = ?`), r.CurrentAttemptID).Scan(&stored); err != nil {
		return false, err
	}
	switch off := int64(c.GetOffset()); {
	case off < stored:
		return false, nil
	case off > stored:
		return false, errors.New("live log chunk does not follow the stored output")
	}
	if stored+int64(len(data)) > MaxLiveLog {
		return true, nil
	}
	_, err = sc.s.db.SQL.ExecContext(ctx, sc.s.db.Rebind(`INSERT INTO build_live_logs (attempt_id, generation, offset_bytes, data, created_at) VALUES (?, ?, ?, ?, ?)`),
		r.CurrentAttemptID, r.Generation, stored, data, sc.s.now().UTC())
	if err != nil && isUnique(err) {
		return false, nil
	}
	return false, err
}
