package selfupdate

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"hash/fnv"
	"slices"
	"time"

	"samuori.invalid/samuori/internal/workers"
)

// DefaultWavePercent is the share of a pool's workers updated at once.
const DefaultWavePercent = 25

// host is a controller or worker a rollout may update.
type host struct {
	kind, id, name, version, arch string
	// free is the space reported on the host's update filesystem; nil when the host does not report it.
	free      *int64
	reachable bool
	leader    bool
	lastSeen  time.Time
	minor     int
	org       string
	poolID    string
	pool      string
	location  string
	// archGuessed is true when an older worker reported no host architecture and its capabilities stood in.
	archGuessed bool
	// keepsWorker marks a controller host that also runs a worker (ADR 0046, OD-28); localWorker is that worker
	// when it can be drained and self-tested with its controller's step.
	keepsWorker bool
	localWorker *host
}

// stale reports whether h, or the worker updated with it, is not on version.
func (h host) stale(version string) bool {
	return h.version != version || (h.localWorker != nil && h.localWorker.version != version)
}

// waveSize is how many of a pool's n workers update at once: percent of the pool, at least one.
func waveSize(n, percent int) int {
	return max(1, n*percent/100)
}

// planSteps orders a rollout: controllers one wave each with the leader last, then each pool's workers in waves of
// waveSize, pools in parallel, each wave spread across locations. Wave numbers are shared across pools, so pool A's
// second wave and pool B's second wave carry the same number but advance independently. A controller's local worker
// is part of its controller's step, never of a wave.
func planSteps(controllers, workerHosts []host, version string, percent int, now time.Time) []Step {
	if percent >= 100 {
		return planAllAtOnce(controllers, workerHosts, version, now)
	}
	var steps []Step
	var cs []host
	for _, c := range controllers {
		if c.stale(version) {
			cs = append(cs, c)
		}
	}
	slices.SortStableFunc(cs, func(a, b host) int {
		if a.leader != b.leader {
			if a.leader {
				return 1
			}
			return -1
		}
		return 0
	})
	wave := 0
	for _, c := range cs {
		wave++
		st := Step{Kind: "controller", Target: c.name, Name: c.name, Arch: c.arch, State: "pending", Wave: wave, UpdatedAt: now}
		if c.localWorker != nil {
			st.LocalWorker, st.Name = c.localWorker.id, c.name+" with worker "+c.localWorker.name
		}
		steps = append(steps, st)
	}
	pools := map[string][]host{}
	var order []string
	for _, w := range workerHosts {
		if w.version == version {
			continue
		}
		if _, ok := pools[w.poolID]; !ok {
			order = append(order, w.poolID)
		}
		pools[w.poolID] = append(pools[w.poolID], w)
	}
	slices.SortFunc(order, func(a, b string) int { return cmp.Compare(pools[a][0].pool, pools[b][0].pool) })
	var ws []Step
	for _, id := range order {
		members := spreadLocations(pools[id])
		size := waveSize(len(members), percent)
		for i, w := range members {
			ws = append(ws, Step{Kind: "worker", Target: w.id, Name: w.name, Arch: w.arch, PoolID: w.poolID, Pool: w.pool, Location: w.location, State: "pending", Wave: wave + 1 + i/size, UpdatedAt: now})
		}
	}
	slices.SortStableFunc(ws, func(a, b Step) int { return cmp.Compare(a.Wave, b.Wave) })
	steps = append(steps, ws...)
	for i := range steps {
		steps[i].Seq = i + 1
	}
	return steps
}

// planAllAtOnce is the 100% plan: one secondary controller first, picked from the version so the checks and the
// rollout name the same host, then every other controller and every worker in one wave. The updated secondary takes
// the leader lease before the leader's own update starts (handOver).
func planAllAtOnce(controllers, workerHosts []host, version string, now time.Time) []Step {
	var cs, secondaries []host
	for _, c := range controllers {
		if c.stale(version) {
			cs = append(cs, c)
			if !c.leader && c.reachable {
				secondaries = append(secondaries, c)
			}
		}
	}
	// Without a secondary to update first, the remaining controller still goes alone so workers never fetch the
	// release while it restarts.
	first := ""
	switch {
	case len(secondaries) > 0:
		h := fnv.New32a()
		_, _ = h.Write([]byte(version))
		first = secondaries[int(h.Sum32()%uint32(len(secondaries)))].name
	case len(cs) > 0:
		first = cs[0].name
	}
	var steps []Step
	wave := func(name string) int {
		if first != "" && name != first {
			return 2
		}
		return 1
	}
	for _, c := range cs {
		st := Step{Kind: "controller", Target: c.name, Name: c.name, Arch: c.arch, State: "pending", Wave: wave(c.name), UpdatedAt: now}
		if c.localWorker != nil {
			st.LocalWorker, st.Name = c.localWorker.id, c.name+" with worker "+c.localWorker.name
		}
		steps = append(steps, st)
	}
	for _, w := range workerHosts {
		if w.version != version {
			steps = append(steps, Step{Kind: "worker", Target: w.id, Name: w.name, Arch: w.arch, PoolID: w.poolID, Pool: w.pool, Location: w.location, State: "pending", Wave: wave(""), UpdatedAt: now})
		}
	}
	slices.SortStableFunc(steps, func(a, b Step) int { return cmp.Compare(a.Wave, b.Wave) })
	for i := range steps {
		steps[i].Seq = i + 1
	}
	return steps
}

// spreadLocations orders workers so consecutive ones alternate between locations, which spreads every wave.
func spreadLocations(hs []host) []host {
	byLoc := map[string][]host{}
	var locs []string
	for _, h := range hs {
		if _, ok := byLoc[h.location]; !ok {
			locs = append(locs, h.location)
		}
		byLoc[h.location] = append(byLoc[h.location], h)
	}
	slices.Sort(locs)
	for _, l := range locs {
		slices.SortFunc(byLoc[l], func(a, b host) int { return cmp.Compare(a.name, b.name) })
	}
	out := make([]host, 0, len(hs))
	for i := 0; len(out) < len(hs); i++ {
		for _, l := range locs {
			if i < len(byLoc[l]) {
				out = append(out, byLoc[l][i])
			}
		}
	}
	return out
}

// hosts lists the cluster as a rollout sees it: one entry per controller host name (its newest process) and every
// active remote worker in a pool that can self-test. left names the workers a rollout leaves out, with the reason.
func (s *Service) hosts(ctx context.Context) (controllers, workerHosts []host, left []string, err error) {
	members, err := s.member.Controllers(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	facts := map[string]host{}
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT id, arch, update_free_bytes, local_worker FROM controllers`)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var h host
		var free sql.NullInt64
		if err := rows.Scan(&h.id, &h.arch, &free, &h.keepsWorker); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		if free.Valid {
			h.free = &free.Int64
		}
		facts[h.id] = h
	}
	rows.Close()
	removed := map[string]bool{}
	rrows, err := s.db.SQL.QueryContext(ctx, `SELECT hostname FROM controller_removals`)
	if err != nil {
		return nil, nil, nil, err
	}
	for rrows.Next() {
		var h string
		if err := rrows.Scan(&h); err != nil {
			rrows.Close()
			return nil, nil, nil, err
		}
		removed[h] = true
	}
	rrows.Close()
	byName := map[string]int{}
	for _, c := range members {
		if removed[c.Hostname] {
			continue
		}
		h := host{kind: "controller", id: c.ID, name: c.Hostname, version: c.Version, reachable: c.Live, leader: c.Leader, lastSeen: c.Heartbeat, arch: facts[c.ID].arch, free: facts[c.ID].free,
			keepsWorker: facts[c.ID].keepsWorker}
		if i, ok := byName[c.Hostname]; ok {
			// A live process outranks a stopped one; the newest process wins otherwise.
			if !controllers[i].reachable || h.reachable {
				controllers[i] = h
			}
			continue
		}
		byName[c.Hostname] = len(controllers)
		controllers = append(controllers, h)
	}
	slices.SortFunc(controllers, func(a, b host) int { return cmp.Compare(a.name, b.name) })

	candidates, err := s.workerCandidates(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var locals []candidate
	for _, c := range candidates {
		switch {
		case c.name == workers.LocalWorkerName:
			locals = append(locals, c)
		case c.why != "":
			left = append(left, c.name+" ("+c.why+")")
		default:
			workerHosts = append(workerHosts, c.host)
		}
	}
	// The worker named local belongs to the one controller host that keeps its worker. Without exactly one such host
	// (an all-in-one install, whose installer updates its worker) it stays out of the rollout.
	var keeper *host
	for i := range controllers {
		if controllers[i].keepsWorker {
			if keeper != nil {
				keeper = nil
				break
			}
			keeper = &controllers[i]
		}
	}
	if keeper != nil && len(locals) == 1 {
		if l := locals[0]; l.why != "" {
			left = append(left, l.name+" on "+keeper.name+" ("+l.why+"; reinstalled with its controller, without a self-test)")
		} else {
			keeper.localWorker = &l.host
		}
	}
	return controllers, workerHosts, left, nil
}

// candidate is an enrolled worker a rollout may update; why says why it cannot be.
type candidate struct {
	host
	why string
}

// workerCandidates lists enrolled workers not being removed. An updated worker must pass its self-test, which a
// pool without an approved, runnable profile cannot give.
func (s *Service) workerCandidates(ctx context.Context) ([]candidate, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT w.id, w.org_id, w.name, w.version, w.protocol_minor, w.operational_state, w.capabilities, w.host_arch, w.update_free_bytes,
			COALESCE(w.pool_id, ''), COALESCE(p.name, ''), COALESCE(p.location, ''), COALESCE(p.isolation_profile, ''), COALESCE(p.profile_approved, ?)
		FROM workers w LEFT JOIN worker_pools p ON p.id = w.pool_id
		WHERE w.enrollment_state = 'enrolled' AND NOT EXISTS (SELECT 1 FROM worker_removals r WHERE r.worker_id = w.id)
		ORDER BY w.name`), false)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		c := candidate{host: host{kind: "worker"}}
		var state, caps, profile string
		var approved bool
		var free sql.NullInt64
		if err := rows.Scan(&c.id, &c.org, &c.name, &c.version, &c.minor, &state, &caps, &c.arch, &free, &c.poolID, &c.pool, &c.location, &profile, &approved); err != nil {
			return nil, err
		}
		if free.Valid {
			c.free = &free.Int64
		}
		if c.arch == "" {
			var capabilities workers.Capabilities
			if json.Unmarshal([]byte(caps), &capabilities) == nil && len(capabilities.Architectures) > 0 {
				c.arch, c.archGuessed = capabilities.Architectures[0], true
			}
		}
		switch {
		case state != workers.Active:
			c.why = state
		case c.poolID == "":
			c.why = "unassigned"
		case !approved || profile == "vm-per-job":
			c.why = "pool " + c.pool + " cannot run self-tests"
		default:
			c.reachable = s.workers.Connected(c.id)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
