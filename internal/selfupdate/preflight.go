package selfupdate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/updatebundle"
	"samuori.invalid/samuori/internal/version"
	"samuori.invalid/samuori/internal/workers"
)

// Check outcomes. Only failed blocks a rollout; a warning names something an administrator should know.
const (
	CheckPassed  = "passed"
	CheckWarning = "warning"
	CheckFailed  = "failed"
)

type Check struct {
	ID, Name, State, Detail string
}

// Preflight is what a rollout of a version would do and whether it may start.
type Preflight struct {
	Version string
	Ready   bool
	Checks  []Check
	Steps   []Step
	Left    []string
}

// diskFactor covers the staged copy, the helper's verified copy, the unpacked release and the kept current and
// previous releases on the same filesystem.
const diskFactor = 4

// Preflight runs the checks before a rollout of version: signature, version path, worker protocol, reachability,
// disk space and architectures (ADR 0045).
func (s *Service) Preflight(ctx context.Context, actor authz.Principal, ver string, allowDowngrade bool, percent int) (Preflight, error) {
	if !actor.IsOrgAdmin() {
		return Preflight{}, ErrForbidden
	}
	return s.preflight(ctx, ver, allowDowngrade, percent)
}

func (s *Service) preflight(ctx context.Context, ver string, allowDowngrade bool, percent int) (Preflight, error) {
	if percent <= 0 {
		percent = DefaultWavePercent
	}
	var manifest, sums, sig []byte
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT manifest, checksums, signature FROM release_bundles WHERE version = ?`), ver).Scan(&manifest, &sums, &sig)
	if errors.Is(err, sql.ErrNoRows) {
		var n int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM releases WHERE version = ?`), ver).Scan(&n); err != nil {
			return Preflight{}, err
		}
		if n == 0 {
			return Preflight{}, ErrNotFound
		}
		return Preflight{Version: ver, Checks: []Check{{ID: "signature", Name: "Signature", State: CheckFailed,
			Detail: "this release was uploaded as separate files; upload the bundle from make update"}}}, nil
	}
	if err != nil {
		return Preflight{}, err
	}
	var m updatebundle.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return Preflight{}, err
	}
	sizes := map[string]int64{}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT arch, size FROM releases WHERE version = ?`), ver)
	if err != nil {
		return Preflight{}, err
	}
	for rows.Next() {
		var a string
		var n int64
		if err := rows.Scan(&a, &n); err != nil {
			rows.Close()
			return Preflight{}, err
		}
		sizes[a] = n
	}
	rows.Close()
	controllers, workerHosts, left, err := s.hosts(ctx)
	if err != nil {
		return Preflight{}, err
	}
	all := append(slices.Clone(controllers), workerHosts...)
	for _, c := range controllers {
		if c.localWorker != nil {
			all = append(all, *c.localWorker)
		}
	}
	var todo []host
	for _, h := range all {
		if h.version != ver {
			todo = append(todo, h)
		}
	}
	p := Preflight{Version: ver, Left: left, Steps: planSteps(controllers, workerHosts, ver, percent, s.now().UTC())}

	sigCheck := Check{ID: "signature", Name: "Signature", State: CheckPassed, Detail: "signed by a key in " + s.signers}
	if err := s.verify(sums, sig); err != nil {
		sigCheck.State, sigCheck.Detail = CheckFailed, err.Error()
	}
	p.Checks = append(p.Checks, sigCheck,
		versionCheck(m.Version, all, len(p.Steps), allowDowngrade),
		protocolCheck(m.WorkerProtocol, allowDowngrade),
		reachabilityCheck(all),
		diskCheck(todo, sizes),
		archCheck(todo, m.Architectures))
	if len(left) > 0 {
		p.Checks = append(p.Checks, Check{ID: "excluded", Name: "Workers left out", State: CheckWarning,
			Detail: "Not updated, because they are not active, unassigned, or their pool's isolation profile cannot run self-tests: " + strings.Join(left, ", ")})
	}
	p.Ready = true
	for _, c := range p.Checks {
		if c.State == CheckFailed {
			p.Ready = false
		}
	}
	return p, nil
}

func versionCheck(target string, hosts []host, steps int, allowDowngrade bool) Check {
	c := Check{ID: "version", Name: "Version path", State: CheckPassed}
	t, ok := version.Parse(target)
	if !ok {
		c.State, c.Detail = CheckFailed, target+" is not a semantic version"
		return c
	}
	if steps == 0 {
		c.State, c.Detail = CheckFailed, "every controller and worker already runs "+target
		return c
	}
	var older, incompatible []string
	for _, h := range hosts {
		v, ok := version.Parse(h.version)
		if !ok {
			continue
		}
		if version.Compare(t, v) < 0 {
			older = append(older, h.name+" ("+h.version+")")
		}
		if h.kind == "controller" && !version.ControllersCompatible(h.version, target) {
			incompatible = append(incompatible, h.name+" ("+h.version+")")
		}
		if h.kind == "worker" {
			if ok, why := version.WorkerCompatible(target, h.version); !ok {
				incompatible = append(incompatible, h.name+": "+why)
			}
		}
	}
	switch {
	case len(incompatible) > 0:
		c.State, c.Detail = CheckFailed, "these hosts cannot work with "+target+" during the rollout; update through the minor versions between: "+strings.Join(incompatible, ", ")
	case len(older) > 0 && !allowDowngrade:
		c.State, c.Detail = CheckFailed, target+" is older than "+strings.Join(older, ", ")+"; allow the downgrade explicitly to go back"
	case len(older) > 0:
		c.State, c.Detail = CheckWarning, "downgrade allowed: "+target+" is older than "+strings.Join(older, ", ")
	default:
		c.Detail = fmt.Sprintf("%d hosts move to %s", steps, target)
	}
	return c
}

func protocolCheck(p updatebundle.Protocol, allowDowngrade bool) Check {
	c := Check{ID: "protocol", Name: "Worker protocol", State: CheckPassed}
	here := fmt.Sprintf("%d.%d", workers.ProtocolMajor, workers.ProtocolMinor)
	theirs := fmt.Sprintf("%d.%d", p.Major, p.Minor)
	switch {
	case p.Major != workers.ProtocolMajor:
		c.State, c.Detail = CheckFailed, "the bundle speaks worker protocol "+theirs+" and this controller "+here+"; majors must match"
	case p.Minor < workers.ProtocolMinor && !allowDowngrade:
		c.State, c.Detail = CheckFailed, "the bundle speaks the older worker protocol "+theirs+" (this controller: "+here+")"
	case p.Minor < workers.ProtocolMinor:
		c.State, c.Detail = CheckWarning, "downgrade allowed to worker protocol "+theirs
	default:
		c.Detail = "worker protocol " + theirs + "; workers on " + here + " or the minor before keep working"
	}
	return c
}

func reachabilityCheck(hosts []host) Check {
	c := Check{ID: "reachability", Name: "Every host reachable", State: CheckPassed}
	var down []string
	for _, h := range hosts {
		if !h.reachable {
			d := h.kind + " " + h.name
			if h.kind == "controller" {
				d += " (last heartbeat " + h.lastSeen.UTC().Format("2006-01-02 15:04 MST") + ")"
			}
			down = append(down, d)
		}
	}
	if len(down) > 0 {
		c.State, c.Detail = CheckFailed, "not connected: "+strings.Join(down, ", ")
	} else {
		c.Detail = fmt.Sprintf("%d hosts connected", len(hosts))
	}
	return c
}

func diskCheck(hosts []host, sizes map[string]int64) Check {
	c := Check{ID: "disk", Name: "Free disk space", State: CheckPassed}
	var short, unknown []string
	for _, h := range hosts {
		need := diskFactor * sizes[h.arch]
		switch {
		case h.free == nil:
			unknown = append(unknown, h.name)
		case sizes[h.arch] > 0 && *h.free < need:
			short = append(short, fmt.Sprintf("%s has %s free, needs %s", h.name, mib(*h.free), mib(need)))
		}
	}
	switch {
	case len(short) > 0:
		c.State, c.Detail = CheckFailed, strings.Join(short, "; ")
	case len(unknown) > 0:
		c.State, c.Detail = CheckWarning, "not reported by "+strings.Join(unknown, ", ")+" (releases before the update bundle do not report free space)"
	default:
		c.Detail = fmt.Sprintf("every host has at least %d times its archive free on its update filesystem", diskFactor)
	}
	return c
}

func archCheck(hosts []host, arches []string) Check {
	c := Check{ID: "architectures", Name: "Architectures", State: CheckPassed}
	var missing, unknown, guessed []string
	for _, h := range hosts {
		switch {
		case h.arch == "":
			unknown = append(unknown, h.name)
		case !slices.Contains(arches, h.arch):
			missing = append(missing, h.name+" ("+h.arch+")")
		case h.archGuessed:
			guessed = append(guessed, h.name)
		}
	}
	switch {
	case len(missing) > 0:
		c.State, c.Detail = CheckFailed, "the bundle holds "+strings.Join(arches, ", ")+" but not what these hosts run: "+strings.Join(missing, ", ")
	case len(unknown) > 0:
		c.State, c.Detail = CheckFailed, "these hosts reported no architecture yet: "+strings.Join(unknown, ", ")
	case len(guessed) > 0:
		c.State, c.Detail = CheckWarning, "taken from the advertised capabilities of older workers: "+strings.Join(guessed, ", ")
	default:
		c.Detail = "the bundle holds " + strings.Join(arches, ", ")
	}
	return c
}

func mib(n int64) string { return fmt.Sprintf("%d MiB", n>>20) }
