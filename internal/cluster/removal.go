package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/clusterjoin"
)

// RemovalWipeTimeout is how long a removed controller has to hand the wipe to its host before it is reported as
// not confirmed wiped.
const RemovalWipeTimeout = 2 * time.Minute

var (
	ErrForbidden = errors.New("cluster: only administrators remove controllers")
	ErrNotFound  = errors.New("cluster: no such controller")
	// ErrRemoved means this host's controller identity was removed from the cluster; installing again and an
	// accepted join admit it again.
	ErrRemoved = errors.New("cluster: this controller was removed from the cluster; install it again and join to come back")
)

// Refused is a removal the cluster cannot take now, with the reason for the administrator.
type Refused struct{ Reason string }

func (e *Refused) Error() string { return e.Reason }

// Removal is where a controller host's removal stands.
type Removal struct {
	Hostname, Host, State, Outcome, Detail, Reason, RequestedBy string
	RequestedAt, UpdatedAt                                      time.Time
}

// Removals removes controller hosts from the cluster (ADR 0045): leadership is handed over, the host wipes Samuori,
// its identity is revoked and its membership rows are deleted, and the remaining NATS members forget it.
type Removals struct {
	m *Member
	// NATSOnControllers means every controller runs a NATS member, so removal must keep a majority of them.
	NATSOnControllers bool
	// StreamPeers reads the messaging stream's replicas from JetStream: controller rows alone miss a member NATS
	// has not taken back yet, such as one that rejoined within NATS's five-minute peer-remove window.
	StreamPeers func(ctx context.Context) ([]StreamPeer, int, error)
	// Wipe asks this host's root helper to delete Samuori (samuori-wipe.path).
	Wipe func() error
	// Forget asks this host's root helper to drop the named NATS members from its routes and the JetStream meta
	// group.
	Forget func(hosts []string) error
	// SyncRoutes asks this host's root helper to bring its NATS member's routes and route verification to the plan
	// (samuori-nats-routes.path).
	SyncRoutes func(clusterjoin.RoutePlan) (clusterjoin.RouteOutcome, error)
	// LocalWorker drains (revoke false) or revokes the worker on a controller host that keeps one (ADR 0046,
	// OD-28): it is drained when the removal is requested and revoked before the host's rows go, since the wipe
	// deletes it with the controller.
	LocalWorker func(ctx context.Context, revoke bool, reason string) error

	mu        sync.Mutex
	forgotten map[string]bool
	routes    routeSync
}

func NewRemovals(m *Member) *Removals { return &Removals{m: m, forgotten: map[string]bool{}} }

// Removed reports whether this controller's host is being or was removed; it then no longer contends for leadership.
func (m *Member) Removed() bool { return m.removed.Load() }

// admit refuses a removed host that still holds the identity it had when it was removed, and forgets the removal of
// a host that comes back with a new identity. Accepting a host's join marks its removal rejoined (clusterjoin), so the
// other members stop forgetting it before it starts.
func (m *Member) admit(ctx context.Context) error {
	var pins, state string
	err := m.db.SQL.QueryRowContext(ctx, m.db.Rebind(`SELECT cert_pins, state FROM controller_removals WHERE hostname = ?`), m.Hostname).Scan(&pins, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if (state != "removed" && state != "rejoined") || m.CertPin == "" || slices.Contains(strings.Split(pins, ","), m.CertPin) {
		return ErrRemoved
	}
	_, err = m.db.SQL.ExecContext(ctx, m.db.Rebind(`DELETE FROM controller_removals WHERE hostname = ?`), m.Hostname)
	return err
}

// removing checks whether this host is being removed and, if it is, gives up the leader lease at once so another
// controller takes over with a higher epoch (Fenced).
func (m *Member) removing(ctx context.Context) bool {
	var n int
	if err := m.db.SQL.QueryRowContext(ctx, m.db.Rebind(`SELECT COUNT(*) FROM controller_removals WHERE hostname = ? AND state IN ('requested', 'wiping')`), m.Hostname).Scan(&n); err != nil {
		return m.removed.Load()
	}
	if n == 0 {
		return false
	}
	if !m.removed.Swap(true) {
		m.removedAt.Store(m.now().UnixNano())
		m.log.Warn("this controller is being removed from the cluster; handing over leadership and draining", "controller", m.ID)
	}
	_, _ = m.db.SQL.ExecContext(ctx, m.db.Rebind(`UPDATE cluster_leases SET expires_at = ? WHERE name = ? AND holder = ?`), m.now().UTC(), leaderLease, m.ID)
	return true
}

type host struct {
	hostname, name string
	live           bool
	pins           []string
}

// hosts groups recent controller rows by host.
func (r *Removals) hosts(ctx context.Context) ([]host, error) {
	now := r.m.now().UTC()
	rows, err := r.m.db.SQL.QueryContext(ctx, r.m.db.Rebind(`SELECT hostname, host, cert_pin, heartbeat_at, stopped_at FROM controllers WHERE heartbeat_at > ? ORDER BY started_at`), now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []host
	index := map[string]int{}
	for rows.Next() {
		var hostname, name, pin string
		var beat time.Time
		var stopped sql.NullTime
		if err := rows.Scan(&hostname, &name, &pin, &beat, &stopped); err != nil {
			return nil, err
		}
		i, ok := index[hostname]
		if !ok {
			i = len(out)
			index[hostname] = i
			out = append(out, host{hostname: hostname})
		}
		h := &out[i]
		if name != "" {
			h.name = name
		}
		if pin != "" && !slices.Contains(h.pins, pin) {
			h.pins = append(h.pins, pin)
		}
		h.live = h.live || (!stopped.Valid && beat.After(now.Add(-DeadAfter)))
	}
	return out, rows.Err()
}

// Request starts removing the controller host hostname. It is refused when it would remove the last controller,
// while another controller is not running, when the remaining NATS members would not be a majority, and while an
// update rollout runs.
func (r *Removals) Request(ctx context.Context, actor authz.Principal, hostname, reason, requestID string) (Removal, error) {
	if !actor.IsOrgAdmin() {
		return Removal{}, ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len(reason) < 3 {
		return Removal{}, &Refused{"a reason is required"}
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	hosts, err := r.hosts(ctx)
	if err != nil {
		return Removal{}, err
	}
	var target *host
	others := 0
	for i := range hosts {
		h := &hosts[i]
		if h.hostname == hostname {
			target = h
			continue
		}
		if !h.live {
			return Removal{}, &Refused{fmt.Sprintf("controller %s is not running; start it or remove it first, so the cluster stays healthy while %s leaves", h.hostname, hostname)}
		}
		others++
	}
	if target == nil {
		return Removal{}, ErrNotFound
	}
	if others == 0 {
		return Removal{}, &Refused{"this is the last controller; a cluster needs at least one"}
	}
	if total := others + 1; r.NATSOnControllers && others*2 <= total {
		return Removal{}, &Refused{fmt.Sprintf("removing it would leave %d of %d NATS members, which is not a majority; add a controller first", others, total)}
	}
	if r.NATSOnControllers && r.StreamPeers != nil {
		if err := streamKeepsMajority(ctx, r.StreamPeers, target.name); err != nil {
			return Removal{}, err
		}
	}
	var running int
	if err := r.m.db.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM rollouts WHERE state = 'running'`).Scan(&running); err != nil {
		return Removal{}, err
	}
	if running > 0 {
		return Removal{}, &Refused{"an update rollout is running; wait for it to finish or stop it first"}
	}
	now := r.m.now().UTC()
	rm := Removal{Hostname: hostname, Host: target.name, State: "requested", Reason: reason, RequestedBy: actor.ID, RequestedAt: now, UpdatedAt: now}
	if !target.live {
		rm.Detail = "the controller is not running"
	}
	err = r.m.db.InTx(ctx, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, r.m.db.Rebind(`SELECT state FROM controller_removals WHERE hostname = ?`), hostname).Scan(&state)
		switch {
		case err == nil && state != "removed" && state != "rejoined":
			return &Refused{"this controller is already being removed"}
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.ExecContext(ctx, r.m.db.Rebind(`DELETE FROM controller_removals WHERE hostname = ?`), hostname); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, r.m.db.Rebind(`INSERT INTO controller_removals (hostname, host, cert_pins, org_id, reason, requested_by, requested_at, state, detail, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'requested', ?, ?)`),
			hostname, target.name, strings.Join(target.pins, ","), actor.OrgID, reason, actor.ID, now, rm.Detail, now); err != nil {
			return err
		}
		return audit.Append(ctx, r.m.db, tx, audit.Event{Actor: actor, Action: "controller.remove", ResourceType: "controller", ResourceID: hostname, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return rm, err
	}
	if keeps, kerr := r.keepsWorker(ctx, hostname); kerr != nil || keeps {
		if kerr == nil {
			kerr = r.LocalWorker(ctx, false, "its controller host "+hostname+" is being removed")
		}
		if kerr != nil {
			rm.Detail = strings.TrimPrefix(rm.Detail+"; its worker could not be drained: "+kerr.Error(), "; ")
			_, _ = r.m.db.SQL.ExecContext(ctx, r.m.db.Rebind(`UPDATE controller_removals SET detail = ? WHERE hostname = ?`), rm.Detail, hostname)
		}
	}
	return rm, nil
}

// keepsWorker reports whether hostname runs a worker that goes with its controller.
func (r *Removals) keepsWorker(ctx context.Context, hostname string) (bool, error) {
	if r.LocalWorker == nil {
		return false, nil
	}
	var n int
	err := r.m.db.SQL.QueryRowContext(ctx, r.m.db.Rebind(`SELECT COUNT(*) FROM controllers WHERE hostname = ? AND local_worker = ?`), hostname, true).Scan(&n)
	return n > 0, err
}

// List returns recent removals, newest first.
func (r *Removals) List(ctx context.Context) ([]Removal, error) {
	rows, err := r.m.db.SQL.QueryContext(ctx, r.m.db.Rebind(`SELECT hostname, host, state, outcome, detail, reason, requested_by, requested_at, updated_at FROM controller_removals WHERE updated_at > ? AND state <> 'rejoined' ORDER BY requested_at DESC`),
		r.m.now().UTC().Add(-30*24*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Removal
	for rows.Next() {
		var x Removal
		if err := rows.Scan(&x.Hostname, &x.Host, &x.State, &x.Outcome, &x.Detail, &x.Reason, &x.RequestedBy, &x.RequestedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// StageOwnWipe runs on every controller. On a host being removed it waits until another controller holds the leader
// lease, then has the host wipe Samuori.
func (r *Removals) StageOwnWipe(ctx context.Context) error {
	if !r.m.Removed() || r.Wipe == nil {
		return nil
	}
	// Its health has answered 503 since the removal (Draining), so a load balancer stops sending it requests first.
	if r.m.now().Sub(time.Unix(0, r.m.removedAt.Load())) < RemovalDrain {
		return nil
	}
	var state string
	err := r.m.db.SQL.QueryRowContext(ctx, r.m.db.Rebind(`SELECT state FROM controller_removals WHERE hostname = ?`), r.m.Hostname).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state != "requested") {
		return nil
	}
	if err != nil {
		return err
	}
	var n int
	if err := r.m.db.SQL.QueryRowContext(ctx, r.m.db.Rebind(`SELECT COUNT(*) FROM cluster_leases WHERE name = ? AND holder <> ? AND expires_at > ?`), leaderLease, r.m.ID, r.m.now().UTC()).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if err := r.Wipe(); err != nil {
		_, uerr := r.m.db.SQL.ExecContext(ctx, r.m.db.Rebind(`UPDATE controller_removals SET detail = ? WHERE hostname = ? AND state = 'requested'`),
			"the host could not start the wipe: "+err.Error(), r.m.Hostname)
		return errors.Join(err, uerr)
	}
	_, err = r.m.db.SQL.ExecContext(ctx, r.m.db.Rebind(`UPDATE controller_removals SET state = 'wiping', detail = ?, updated_at = ? WHERE hostname = ? AND state = 'requested'`),
		"the host started removing Samuori", r.m.now().UTC(), r.m.Hostname)
	return err
}

// Process advances removals on the leader: a wipe the host started, or a host that never answered, ends with its
// membership rows deleted and its identity revoked.
func (r *Removals) Process(ctx context.Context, epoch int64) error {
	rows, err := r.m.db.SQL.QueryContext(ctx, `SELECT hostname, state, detail, requested_at FROM controller_removals WHERE state IN ('requested', 'wiping')`)
	if err != nil {
		return err
	}
	type pending struct {
		hostname, state, detail string
		requested               time.Time
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.hostname, &p.state, &p.detail, &p.requested); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	hosts, err := r.hosts(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, h := range hosts {
		live[h.hostname] = h.live
	}
	now := r.m.now().UTC()
	for _, p := range list {
		finishing := p.state == "wiping" || !live[p.hostname] || now.Sub(p.requested) > RemovalWipeTimeout
		if keeps, err := r.keepsWorker(ctx, p.hostname); err != nil {
			return err
		} else if keeps && finishing {
			if err := r.LocalWorker(ctx, true, "its controller host "+p.hostname+" was removed"); err != nil {
				return fmt.Errorf("revoking the worker of %s: %w", p.hostname, err)
			}
		}
		switch {
		case p.state == "wiping":
			err = r.finish(ctx, epoch, p.hostname, "wiped", p.detail)
		case !live[p.hostname]:
			err = r.finish(ctx, epoch, p.hostname, "not-confirmed", "the controller was not running, so its host was not wiped")
		case now.Sub(p.requested) > RemovalWipeTimeout:
			detail := "the controller did not confirm the wipe"
			if p.detail != "" {
				detail += "; " + p.detail
			}
			err = r.finish(ctx, epoch, p.hostname, "not-confirmed", detail)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Removals) finish(ctx context.Context, epoch int64, hostname, outcome, detail string) error {
	hosts, err := r.hosts(ctx)
	if err != nil {
		return err
	}
	return r.m.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := r.m.Fenced(ctx, tx, epoch); err != nil {
			return err
		}
		var pins []string
		var stored, org string
		if err := tx.QueryRowContext(ctx, r.m.db.Rebind(`SELECT cert_pins, org_id FROM controller_removals WHERE hostname = ?`), hostname).Scan(&stored, &org); err != nil {
			return err
		}
		if stored != "" {
			pins = strings.Split(stored, ",")
		}
		for _, h := range hosts {
			if h.hostname == hostname {
				for _, p := range h.pins {
					if !slices.Contains(pins, p) {
						pins = append(pins, p)
					}
				}
			}
		}
		res, err := tx.ExecContext(ctx, r.m.db.Rebind(`UPDATE controller_removals SET state = 'removed', outcome = ?, detail = ?, cert_pins = ?, updated_at = ? WHERE hostname = ? AND state IN ('requested', 'wiping')`),
			outcome, detail, strings.Join(pins, ","), r.m.now().UTC(), hostname)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, r.m.db.Rebind(`DELETE FROM controllers WHERE hostname = ?`), hostname); err != nil {
			return err
		}
		return audit.Append(ctx, r.m.db, tx, audit.Event{Actor: authz.Principal{ID: "system:controller-removal", OrgID: org, Kind: authz.System}, Action: "controller.revoke",
			ResourceType: "controller", ResourceID: hostname, Outcome: "succeeded", Reason: outcome + ": " + detail})
	})
}

// ForgetRemoved runs on every controller that runs a NATS member: it asks the host's root helper once per process
// to drop each removed controller from this member's routes and from the JetStream meta group.
func (r *Removals) ForgetRemoved(ctx context.Context) error {
	if r.Forget == nil || r.m.Removed() {
		return nil
	}
	rows, err := r.m.db.SQL.QueryContext(ctx, r.m.db.Rebind(`SELECT host FROM controller_removals WHERE state = 'removed' AND host <> '' AND hostname <> ?`), r.m.Hostname)
	if err != nil {
		return err
	}
	var hosts []string
	r.mu.Lock()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			r.mu.Unlock()
			return err
		}
		if !r.forgotten[h] && h != r.m.Host {
			hosts = append(hosts, h)
		}
	}
	r.mu.Unlock()
	rows.Close()
	if err := rows.Err(); err != nil || len(hosts) == 0 {
		return err
	}
	if err := r.Forget(hosts); err != nil {
		return err
	}
	r.mu.Lock()
	for _, h := range hosts {
		r.forgotten[h] = true
	}
	r.mu.Unlock()
	return nil
}

// StreamPeer is one replica of the messaging stream.
type StreamPeer struct {
	Name    string
	Current bool
}

func streamKeepsMajority(ctx context.Context, read func(context.Context) ([]StreamPeer, int, error), leaving string) error {
	peers, replicas, err := read(ctx)
	if err != nil {
		return &Refused{"the messaging stream's replicas could not be read, so removal could break messaging: " + err.Error()}
	}
	current := 0
	var behind []string
	for _, p := range peers {
		switch {
		case p.Name == leaving:
		case p.Current:
			current++
		default:
			behind = append(behind, p.Name)
		}
	}
	if current*2 > replicas {
		return nil
	}
	msg := fmt.Sprintf("the messaging stream would keep %d of its %d replicas current, which is not a majority", current, replicas)
	if len(behind) > 0 {
		msg += "; waiting for " + strings.Join(behind, ", ") + " to catch up"
	}
	return &Refused{msg + ". A controller that joined again after a removal is taken back by NATS after up to five minutes; try again then"}
}

// RemovalDrain is how long a controller being removed reports itself unavailable before it wipes its host: longer
// than a load balancer needs to take it out of service (an ALB with two failed checks 10 s apart).
const RemovalDrain = 30 * time.Second

// Draining is the health check of a controller being removed: unavailable, so load balancers stop routing to it.
func (m *Member) Draining(context.Context) error {
	if m.Removed() {
		return errors.New("this controller is being removed from the cluster")
	}
	return nil
}
