// Package cluster coordinates controllers that share one database: membership with heartbeats, a leader lease with a
// fencing epoch for singleton duties, and encrypted material every controller needs (ADR 0034). A standalone
// controller runs the same code as a cluster of one.
package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

// Timing for heartbeats and leases. A controller is presumed dead once its heartbeat is older than DeadAfter; the
// leader lease outlives a missed renewal but not a dead leader.
const (
	HeartbeatEvery = 3 * time.Second
	DeadAfter      = 15 * time.Second
	LeaseTTL       = 10 * time.Second
	// MaxClockSkew bounds the difference between a controller's clock and the database server's clock.
	MaxClockSkew = 2 * time.Second
)

// Member is this controller's entry in the cluster.
type Member struct {
	ID       string
	Hostname string
	Version  string
	// Host is the name in this controller's certificates, which its NATS member uses as its server name; CertPin is
	// its API certificate's pin. Both are empty for a controller without a certificate.
	Host    string
	CertPin string
	// LocalWorker means this host also runs a worker that updates with the controller (ADR 0046, OD-28).
	LocalWorker bool

	db      *db.DB
	log     *slog.Logger
	now     func() time.Time
	leader  atomic.Bool
	epoch   atomic.Int64
	removed atomic.Bool
	// removedAt is when this controller learned it is being removed, in Unix nanoseconds.
	removedAt atomic.Int64
	// lastBeat is when this controller last recorded a heartbeat, in Unix nanoseconds.
	lastBeat atomic.Int64
	mu       sync.Mutex
	gained   []func(context.Context)
}

// Identity is how a controller is known beyond its process: its certificate name and API certificate pin.
type Identity struct {
	Host, CertPin string
	LocalWorker   bool
}

// Join registers a new controller instance. Every process start is a new member, so work it claimed before a crash
// is recognisably orphaned.
func Join(ctx context.Context, d *db.DB, version string, log *slog.Logger) (*Member, error) {
	return JoinAs(ctx, d, version, Identity{}, log)
}

// JoinAs is Join for a controller with a certificate identity. A host that was removed is refused while it still
// holds the identity it had then; a new identity, which only an accepted join issues, admits it again (ADR 0045).
func JoinAs(ctx context.Context, d *db.DB, version string, id Identity, log *slog.Logger) (*Member, error) {
	hostname, _ := os.Hostname()
	return join(ctx, d, version, hostname, id, log)
}

func join(ctx context.Context, d *db.DB, version, hostname string, id Identity, log *slog.Logger) (*Member, error) {
	m := &Member{ID: ids.New(), Hostname: hostname, Version: version, Host: id.Host, CertPin: id.CertPin, LocalWorker: id.LocalWorker, db: d, log: log, now: time.Now}
	if err := m.CheckClock(ctx); err != nil {
		return nil, err
	}
	if err := m.admit(ctx); err != nil {
		return nil, err
	}
	now := m.now().UTC()
	m.lastBeat.Store(now.UnixNano())
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO controllers (id, hostname, version, started_at, heartbeat_at, host, cert_pin, local_worker) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		m.ID, hostname, version, now, now, m.Host, m.CertPin, m.LocalWorker); err != nil {
		return nil, fmt.Errorf("cluster: registering this controller: %w", err)
	}
	return m, nil
}

// ErrClockSkew means this controller's clock disagrees with the database server's; leases would be unreliable.
var ErrClockSkew = errors.New("cluster: the controller clock differs from the database clock")

// CheckClock compares the local clock with PostgreSQL's. SQLite runs on this host and shares its clock.
func (m *Member) CheckClock(ctx context.Context) error {
	if m.db.Dialect != db.Postgres {
		return nil
	}
	before := m.now()
	var server time.Time
	if err := m.db.SQL.QueryRowContext(ctx, `SELECT now()`).Scan(&server); err != nil {
		return err
	}
	after := m.now()
	mid := before.Add(after.Sub(before) / 2)
	if skew := mid.Sub(server).Abs(); skew > MaxClockSkew+after.Sub(before) {
		return fmt.Errorf("%w by %s; synchronise both with NTP", ErrClockSkew, skew.Round(time.Millisecond))
	}
	return nil
}

// Run heartbeats and contends for leadership until ctx ends, then leaves the cluster.
func (m *Member) Run(ctx context.Context) {
	t := time.NewTicker(HeartbeatEvery)
	defer t.Stop()
	for {
		m.beat(ctx)
		select {
		case <-ctx.Done():
			m.leave(context.WithoutCancel(ctx))
			return
		case <-t.C:
		}
	}
}

func (m *Member) beat(parent context.Context) {
	// A database that stops answering must not stall the loop: an unrecorded heartbeat is how this controller learns
	// it is cut off (Fresh).
	ctx, cancel := context.WithTimeout(parent, HeartbeatEvery)
	defer cancel()
	now := m.now().UTC()
	res, err := m.db.SQL.ExecContext(ctx, m.db.Rebind(`UPDATE controllers SET heartbeat_at = ? WHERE id = ? AND stopped_at IS NULL`), now, m.ID)
	if err != nil && parent.Err() == nil {
		m.log.Warn("controller heartbeat", "error", err)
	}
	if err == nil {
		if n, _ := res.RowsAffected(); n == 1 {
			m.lastBeat.Store(now.UnixNano())
		}
	}
	if err := m.CheckClock(ctx); err != nil && parent.Err() == nil {
		m.log.Error("clock check", "error", err)
		m.step(false, 0)
		return
	}
	if m.removing(ctx) {
		m.step(false, 0)
		return
	}
	held, epoch, err := m.contend(ctx, now)
	if err != nil && parent.Err() == nil {
		m.log.Warn("leader lease", "error", err)
	}
	m.step(held, epoch)
}

func (m *Member) step(held bool, epoch int64) {
	was := m.leader.Swap(held)
	m.epoch.Store(epoch)
	switch {
	case held && !was:
		m.log.Info("this controller is now the leader", "controller", m.ID, "epoch", epoch)
		m.mu.Lock()
		hooks := append([]func(context.Context){}, m.gained...)
		m.mu.Unlock()
		for _, h := range hooks {
			go h(context.Background())
		}
	case !held && was:
		m.log.Warn("this controller lost leadership", "controller", m.ID)
	}
}

const leaderLease = "leader"

// contend takes or renews the leader lease. Taking an expired lease increments its epoch, which fences the previous
// holder's singleton writes (Fenced).
func (m *Member) contend(ctx context.Context, now time.Time) (bool, int64, error) {
	until := now.Add(LeaseTTL)
	var epoch int64
	err := m.db.InTx(ctx, func(tx *sql.Tx) error {
		var holder string
		var expires time.Time
		err := tx.QueryRowContext(ctx, m.db.Rebind(`SELECT holder, epoch, expires_at FROM cluster_leases WHERE name = ?`), leaderLease).Scan(&holder, &epoch, &expires)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			epoch = 1
			_, err = tx.ExecContext(ctx, m.db.Rebind(`INSERT INTO cluster_leases (name, holder, epoch, expires_at) VALUES (?, ?, ?, ?)`), leaderLease, m.ID, epoch, until)
			return err
		case err != nil:
			return err
		}
		if holder != m.ID && now.Before(expires) {
			epoch = 0
			return nil
		}
		previous := epoch
		if holder != m.ID {
			epoch++
		}
		res, err := tx.ExecContext(ctx, m.db.Rebind(`UPDATE cluster_leases SET holder = ?, epoch = ?, expires_at = ? WHERE name = ? AND holder = ? AND epoch = ?`),
			m.ID, epoch, until, leaderLease, holder, previous)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			epoch = 0
		}
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	return epoch > 0, epoch, nil
}

// HandOver gives the leader lease, held at epoch, to another live controller under a new epoch, fencing this
// controller's singleton writes at once. The new holder renews the lease on its next heartbeat; if it died
// meanwhile, the lease expires and the controllers elect a leader as usual.
func (m *Member) HandOver(ctx context.Context, epoch int64, to string) error {
	now := m.now().UTC()
	err := m.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := m.Fenced(ctx, tx, epoch); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, m.db.Rebind(`SELECT COUNT(*) FROM controllers WHERE id = ? AND stopped_at IS NULL AND heartbeat_at > ?`), to, now.Add(-DeadAfter)).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return errors.New("cluster: the new leader is not alive")
		}
		_, err := tx.ExecContext(ctx, m.db.Rebind(`UPDATE cluster_leases SET holder = ?, epoch = ?, expires_at = ? WHERE name = ? AND holder = ? AND epoch = ?`),
			to, epoch+1, now.Add(LeaseTTL), leaderLease, m.ID, epoch)
		return err
	})
	if err != nil {
		return err
	}
	m.step(false, 0)
	return nil
}

func (m *Member) leave(ctx context.Context) {
	now := m.now().UTC()
	if m.leader.Load() {
		_, _ = m.db.SQL.ExecContext(ctx, m.db.Rebind(`UPDATE cluster_leases SET expires_at = ? WHERE name = ? AND holder = ?`), now, leaderLease, m.ID)
	}
	_, _ = m.db.SQL.ExecContext(ctx, m.db.Rebind(`UPDATE controllers SET stopped_at = ?, heartbeat_at = ? WHERE id = ?`), now, now, m.ID)
	m.leader.Store(false)
}

// Fresh reports whether this controller's heartbeat is recent enough that peers still count it as alive. A controller
// that is no longer fresh has been presumed dead and must stop serving workers (self-fencing, ADR 0034).
func (m *Member) Fresh() bool {
	return m.now().Sub(time.Unix(0, m.lastBeat.Load())) < DeadAfter-HeartbeatEvery
}

// CheckFresh is Fresh as a health check.
func (m *Member) CheckFresh(context.Context) error {
	if !m.Fresh() {
		return errors.New("cluster: this controller cannot record its heartbeat; other controllers treat it as gone")
	}
	return nil
}

// Leader reports whether this controller holds the leader lease, and its epoch.
func (m *Member) Leader() (bool, int64) { return m.leader.Load(), m.epoch.Load() }

// OnLeadership runs fn each time this controller becomes the leader.
func (m *Member) OnLeadership(fn func(context.Context)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gained = append(m.gained, fn)
}

// ErrNotLeader means a singleton write was attempted by a controller that no longer holds the lease it started under.
var ErrNotLeader = errors.New("cluster: this controller is not the leader")

// Fenced checks, inside tx, that this controller still holds the leader lease at epoch. Singleton duties call it in
// the transaction that records their effect, so a deposed leader cannot commit.
func (m *Member) Fenced(ctx context.Context, tx *sql.Tx, epoch int64) error {
	var n int
	q := `SELECT COUNT(*) FROM cluster_leases WHERE name = ? AND holder = ? AND epoch = ? AND expires_at > ?`
	if err := tx.QueryRowContext(ctx, m.db.Rebind(q), leaderLease, m.ID, epoch, m.now().UTC()).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return ErrNotLeader
	}
	return nil
}

// Singleton runs fn every interval while this controller is the leader. fn receives the epoch it runs under.
func (m *Member) Singleton(ctx context.Context, name string, every time.Duration, fn func(ctx context.Context, epoch int64) error) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if ok, epoch := m.Leader(); ok {
			if err := fn(ctx, epoch); err != nil && ctx.Err() == nil {
				m.log.Error("singleton duty", "duty", name, "error", err)
			}
		}
	}
}

// LiveSQL is a condition on a controller ID column that holds while that controller is alive, with its argument.
func (m *Member) LiveSQL(column string) (string, time.Time) {
	return `EXISTS (SELECT 1 FROM controllers c WHERE c.id = ` + column + ` AND c.stopped_at IS NULL AND c.heartbeat_at > ?)`, m.now().UTC().Add(-DeadAfter)
}

// Controller is one row of the cluster view.
type Controller struct {
	ID, Hostname, Version, Host string
	StartedAt, Heartbeat        time.Time
	Live, Leader                bool
}

// Controllers lists live controllers and those that stopped or died in the last day.
func (m *Member) Controllers(ctx context.Context) ([]Controller, error) {
	now := m.now().UTC()
	var holder string
	var expires time.Time
	_ = m.db.SQL.QueryRowContext(ctx, m.db.Rebind(`SELECT holder, expires_at FROM cluster_leases WHERE name = ?`), leaderLease).Scan(&holder, &expires)
	rows, err := m.db.SQL.QueryContext(ctx, m.db.Rebind(`SELECT id, hostname, version, host, started_at, heartbeat_at, stopped_at FROM controllers WHERE heartbeat_at > ? ORDER BY started_at`), now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Every process start is a new row; a host is listed once, as its most recently started process.
	var out []Controller
	at := map[string]int{}
	for rows.Next() {
		var c Controller
		var stopped sql.NullTime
		if err := rows.Scan(&c.ID, &c.Hostname, &c.Version, &c.Host, &c.StartedAt, &c.Heartbeat, &stopped); err != nil {
			return nil, err
		}
		c.Live = !stopped.Valid && c.Heartbeat.After(now.Add(-DeadAfter))
		c.Leader = c.Live && c.ID == holder && now.Before(expires)
		if i, ok := at[c.Hostname]; ok {
			out[i] = c
			continue
		}
		at[c.Hostname] = len(out)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Prune deletes controller rows that stopped or died more than a week ago.
func (m *Member) Prune(ctx context.Context) error {
	_, err := m.db.SQL.ExecContext(ctx, m.db.Rebind(`DELETE FROM controllers WHERE heartbeat_at < ?`), m.now().UTC().Add(-7*24*time.Hour))
	return err
}

// NeverJoined reports whether this controller is the cluster's only member so far: no other controller has ever
// started (a controller's rows stay until it is removed). An accepted join whose host never started does not count.
// Such a controller cannot have a NATS majority yet but can serve the portal, where the next controller is added.
func (m *Member) NeverJoined(ctx context.Context) bool {
	var others int
	if err := m.db.SQL.QueryRowContext(ctx, m.db.Rebind(`SELECT COUNT(*) FROM controllers WHERE hostname <> ?`), m.Hostname).Scan(&others); err != nil {
		return false
	}
	return others == 0
}
