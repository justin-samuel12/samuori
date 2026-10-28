package rootlesshost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/isolation"
)

// SelfTestImage is the digest-pinned image the self-test probes run in; it is the Node.js 22 runtime of the
// built-in recipes, so a working mirror already serves it.
const SelfTestImage = "docker.io/library/node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392"

const probeScript = `const fs = require('fs'), net = require('net'), dns = require('dns');
const out = (id, ok, detail) => console.log('SELFTEST ' + id + ' ' + (ok ? 'pass' : 'fail') + ' ' + detail);
const read = (p) => { try { return fs.readFileSync(p, 'utf8'); } catch { return ''; } };
const map = read('/proc/self/uid_map').trim().split(/\s+/);
out('SEC-ISO-01', map[0] === '0' && map[1] !== '0', 'step root maps to host uid ' + map[1]);
const status = read('/proc/self/status');
const seccomp = (status.match(/Seccomp:\s+(\d)/) || [])[1];
out('SEC-ISO-02', seccomp === '2', 'seccomp mode ' + seccomp);
const sockets = ['/var/run/docker.sock', '/run/docker.sock', '/run/containerd/containerd.sock', '/run/buildkit/buildkitd.sock', '/var/run/buildkit/buildkitd.sock', '/run/podman/podman.sock'].filter((p) => fs.existsSync(p));
out('SEC-ISO-03', sockets.length === 0, sockets.length ? 'found ' + sockets.join(' ') : 'no engine or runtime sockets');
const capEff = (status.match(/CapEff:\s+([0-9a-f]+)/) || [])[1] || '';
const devices = fs.readdirSync('/dev').filter((d) => /^(sd|vd|nvme|xvd|hd|mem|kmem|port|loop|dm-|mapper|kvm|fuse)/.test(d));
const hostMounts = read('/proc/self/mountinfo').split('\n').filter(Boolean).map((l) => { const [a, b] = l.split(' - '); return {point: a.split(' ')[4], fs: (b || '').split(' ')[0]}; })
  .filter((m) => /^(ext[234]|xfs|btrfs|vfat|zfs)$/.test(m.fs) && !['/etc/resolv.conf', '/etc/hosts', '/etc/hostname', '/run/.containerenv'].includes(m.point));
out('SEC-ISO-05', !/^0*1f{10}$/.test(capEff) && devices.length === 0 && hostMounts.length === 0, 'caps ' + capEff + ', devices ' + (devices.join(' ') || 'none') + ', host mounts ' + hostMounts.length);
const tasks = [
  new Promise((r) => { const s = net.connect({host: '1.1.1.1', port: 443, timeout: 4000}); s.on('connect', () => { s.destroy(); r('direct internet reachable'); }); s.on('timeout', () => { s.destroy(); r(''); }); s.on('error', () => r('')); }),
  dns.promises.resolve4('example.com').then(() => 'DNS answered', () => ''),
  new Promise((r) => { const s = net.connect({host: '169.254.169.254', port: 80, timeout: 4000}); s.on('connect', () => { s.destroy(); r('metadata address reachable'); }); s.on('timeout', () => { s.destroy(); r(''); }); s.on('error', () => r('')); }),
  new Promise((r) => { if (!fs.existsSync('/dev/otel-grpc.sock')) return r(''); const s = net.connect('/dev/otel-grpc.sock'); s.on('connect', () => { s.destroy(); r('otel'); }); s.on('error', () => r('')); }),
];
Promise.all(tasks).then(([direct, dnsr, md, otel]) => {
  const leaks = [direct, dnsr, md].filter(Boolean);
  out('SEC-NET-01', leaks.length === 0, leaks.length ? leaks.join(', ') : 'no direct internet, DNS or metadata');
  out('SEC-ISO-04', !otel, otel ? 'the telemetry socket accepts connections' : (fs.existsSync('/dev/otel-grpc.sock') ? 'telemetry socket present but refuses connections' : 'no telemetry socket'));
});
`

func passFail(id string, ok bool, reason string) isolation.Check {
	outcome := "passed"
	if !ok {
		outcome = "failed"
	}
	return isolation.Check{ID: id, Category: "self-test", Outcome: outcome, Reason: reason}
}

func dockerfilePlan() []byte {
	raw, _ := buildplan.Plan{Version: 1, Method: "dockerfile", Platforms: []string{"linux/" + runtime.GOARCH}, Cache: buildplan.PlanCache{Mode: "none"},
		Dockerfile: &buildplan.PlanDockerfile{Context: ".", Path: "Dockerfile"}, Validation: buildplan.PlanValidation{Profile: buildplan.ProfileNone},
		Egress: buildplan.PlanEgress{Presets: []string{}, Allow: []buildplan.EgressRule{}}}.Encode()
	return raw
}

// build runs one self-test build in a fresh environment and always destroys it.
func (p *Provider) build(ctx context.Context, files map[string]string, limits isolation.Limits) (isolation.RunResult, error) {
	src, err := os.MkdirTemp("", "samuori-selftest-")
	if err != nil {
		return isolation.RunResult{}, err
	}
	defer os.RemoveAll(src)
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			return isolation.RunResult{}, err
		}
	}
	id := "selftest-" + strings.ToLower(ids.New())
	env, err := p.Prepare(ctx, isolation.Spec{AttemptID: id, SourceDir: src, Profile: p.cfg.Profile, Limits: limits})
	if err != nil {
		return isolation.RunResult{}, err
	}
	defer p.Destroy(context.WithoutCancel(ctx), env)
	return p.Run(ctx, env, dockerfilePlan())
}

// SelfTest runs the worker self-test checks in this provider's own environments (ADR 0027). Each check builds in a
// fresh environment with the provider's real configuration: build user, firewall, proxy, mirror and limits.
func (p *Provider) SelfTest(ctx context.Context) []isolation.Check {
	limits := isolation.Limits{VCPUs: 2, MemoryBytes: 2 << 30, WallClock: 10 * time.Minute}
	var out []isolation.Check
	res, err := p.build(ctx, map[string]string{"Dockerfile": "FROM " + SelfTestImage + "\nCOPY probe.js /probe.js\nRUN [\"node\", \"/probe.js\"]\n", "probe.js": probeScript}, limits)
	found := map[string]isolation.Check{}
	for _, line := range strings.Split(string(res.Logs), "\n") {
		i := strings.Index(line, "SELFTEST ")
		if i < 0 || strings.Contains(line, "console.log") {
			continue
		}
		f := strings.SplitN(strings.TrimSpace(line[i+9:]), " ", 3)
		if len(f) == 3 {
			found[f[0]] = passFail(f[0], f[1] == "pass", f[2])
		}
	}
	for _, id := range []string{"SEC-ISO-01", "SEC-ISO-02", "SEC-ISO-03", "SEC-ISO-04", "SEC-ISO-05", "SEC-NET-01"} {
		c, ok := found[id]
		if !ok {
			reason := "the probe build did not report this check"
			if err != nil {
				reason += ": " + clipErr(err)
			}
			c = isolation.Check{ID: id, Category: "self-test", Outcome: "errored", Reason: reason}
		}
		out = append(out, c)
	}
	out = append(out, p.occupancyCheck(ctx))
	memory := isolation.Limits{VCPUs: 2, MemoryBytes: 1 << 30, WallClock: 10 * time.Minute}
	_, err = p.build(ctx, map[string]string{"Dockerfile": "FROM " + SelfTestImage + "\nRUN [\"node\", \"-e\", \"const a=[];for(;;){a.push(Buffer.alloc(64<<20,1))}\"]\n"}, memory)
	oom := errors.Is(err, isolation.ErrResourceLimit)
	out = append(out, passFail("SEC-ISO-08", oom, "an oversized allocation ended as: "+clipErr(err)))
	out = append(out, passFail("SEC-DOS-02", oom, "the build was contained by its memory limit"))
	flood := fmt.Sprintf("const {spawn} = require('child_process'); for (let i = 0; i < %d; i++) { spawn('sleep', ['120']).on('error', (e) => { console.log('LIMIT ' + e.code); process.exit(3); }); } setTimeout(() => { console.log('NO-LIMIT'); process.exit(0); }, 30000);", p.cfg.Tasks+500)
	_, err = p.build(ctx, map[string]string{"Dockerfile": "FROM " + SelfTestImage + "\nCOPY flood.js /flood.js\nRUN [\"node\", \"/flood.js\"]\n", "flood.js": flood}, limits)
	limited := err != nil && strings.Contains(err.Error(), "LIMIT EAGAIN")
	out = append(out, passFail("SEC-DOS-03", limited, "a process flood ended as: "+clipErr(err)))
	return out
}

// occupancyCheck proves one build per daemon: a second environment for the same attempt is refused, and two
// attempts get two daemons (SEC-ISO-07).
func (p *Provider) occupancyCheck(ctx context.Context) isolation.Check {
	src, err := os.MkdirTemp("", "samuori-selftest-")
	if err != nil {
		return isolation.Check{ID: "SEC-ISO-07", Category: "self-test", Outcome: "errored", Reason: err.Error()}
	}
	defer os.RemoveAll(src)
	limits := isolation.Limits{MemoryBytes: 1 << 30, WallClock: time.Minute}
	a, b := "selftest-"+strings.ToLower(ids.New()), "selftest-"+strings.ToLower(ids.New())
	ea, err := p.Prepare(ctx, isolation.Spec{AttemptID: a, SourceDir: src, Profile: p.cfg.Profile, Limits: limits})
	if err != nil {
		return isolation.Check{ID: "SEC-ISO-07", Category: "self-test", Outcome: "errored", Reason: clipErr(err)}
	}
	defer p.Destroy(context.WithoutCancel(ctx), ea)
	_, dup := p.Prepare(ctx, isolation.Spec{AttemptID: a, SourceDir: src, Profile: p.cfg.Profile, Limits: limits})
	eb, err := p.Prepare(ctx, isolation.Spec{AttemptID: b, SourceDir: src, Profile: p.cfg.Profile, Limits: limits})
	if err != nil {
		return isolation.Check{ID: "SEC-ISO-07", Category: "self-test", Outcome: "errored", Reason: clipErr(err)}
	}
	defer p.Destroy(context.WithoutCancel(ctx), eb)
	x, _ := p.get(ea.ID)
	y, _ := p.get(eb.ID)
	separate := x != nil && y != nil && x.runner != y.runner && x.dir != y.dir && x.daemonDir != y.daemonDir
	if separate && x.daemon != nil {
		separate = x.daemon.Pid() != y.daemon.Pid()
	}
	return passFail("SEC-ISO-07", errors.Is(dup, isolation.ErrOccupied) && separate, "a second environment for one attempt was refused and each attempt has its own "+p.cfg.Engine+" instance and state")
}

func clipErr(err error) string {
	if err == nil {
		return "success"
	}
	s := err.Error()
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}
