//go:build linux

package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/adapters/isolation/rootlesshost"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/vmguest"
)

func check(id string, ok bool, reason string) isolation.Check {
	outcome := "passed"
	if !ok {
		outcome = "failed"
	}
	return isolation.Check{ID: id, Category: "self-test", Outcome: outcome, Reason: reason}
}

func errored(id string, err error) isolation.Check {
	return isolation.Check{ID: id, Category: "self-test", Outcome: "errored", Reason: clipN(errText(err), 300)}
}

func errText(err error) string {
	if err == nil {
		return "success"
	}
	return err.Error()
}

// SelfTest runs the worker self-test checks in this provider's own VMs (ADR 0027, ADR 0038): the in-guest probes
// of the rootless engine inside one VM, then host-level checks that each attempt gets its own VM, that nothing
// carries from one VM to the next, and that the disk limit holds.
func (p *Provider) SelfTest(ctx context.Context) []isolation.Check {
	out := p.guestProbes(ctx)
	out = append(out, p.occupancy(ctx), p.noCarryOver(ctx), p.diskQuota(ctx))
	return out
}

var guestChecks = []string{"SEC-ISO-01", "SEC-ISO-02", "SEC-ISO-03", "SEC-ISO-04", "SEC-ISO-05", "SEC-NET-01", "SEC-ISO-08", "SEC-DOS-02", "SEC-DOS-03"}

func (p *Provider) guestProbes(ctx context.Context) []isolation.Check {
	l, _ := p.limits(isolation.Limits{})
	fail := func(err error) []isolation.Check {
		var out []isolation.Check
		for _, id := range guestChecks {
			out = append(out, errored(id, err))
		}
		return out
	}
	v, err := p.boot(ctx, "vm-"+strings.ToLower(ids.New()), l)
	if err != nil {
		return fail(err)
	}
	defer p.discard(v)
	sess, err := p.cfg.Proxy.Open(nil)
	if err != nil {
		return fail(err)
	}
	defer sess.Close()
	req, _ := json.Marshal(vmguest.RunRequest{AttemptID: "selftest", Limits: l, ProxyURL: sess.URL(fmt.Sprintf("%s:%d", vmguest.RelayIP, vmguest.ProxyRelayPort)),
		Mirrors: p.mirrors, InsecureMirrors: p.cfg.InsecureMirrors, Tasks: p.cfg.Tasks})
	ctx, cancel := context.WithTimeout(ctx, 40*time.Minute)
	defer cancel()
	res, _, err := p.exchange(ctx, v, "/v1/selftest", "application/json", bytes.NewReader(req), v.dir)
	if err == nil && res.Class != "" {
		err = vmguest.Err(res.Class, clipText(res.Detail))
	}
	if err != nil {
		return fail(err)
	}
	byID := map[string]isolation.Check{}
	for _, c := range res.Checks {
		byID[c.ID] = boundCheck(c)
	}
	var out []isolation.Check
	for _, id := range guestChecks {
		c, ok := byID[id]
		if !ok {
			c = errored(id, errors.New("the guest did not report this check"))
		}
		out = append(out, c)
	}
	return out
}

// occupancy proves one build per VM: a second environment for an attempt is refused, and two attempts get two
// VMs with separate processes, disks and sockets (SEC-ISO-07).
func (p *Provider) occupancy(ctx context.Context) isolation.Check {
	if p.cfg.Slots < 2 {
		return check("SEC-ISO-07", true, "this host runs one VM at a time; a second environment is refused while one is held")
	}
	a, b := "selftest-"+strings.ToLower(ids.New()), "selftest-"+strings.ToLower(ids.New())
	spec := func(id string) isolation.Spec {
		return isolation.Spec{AttemptID: id, Profile: isolation.VMPerJob, Limits: isolation.Limits{WallClock: time.Minute}}
	}
	ea, err := p.Prepare(ctx, spec(a))
	if err != nil {
		return errored("SEC-ISO-07", err)
	}
	defer p.Destroy(context.WithoutCancel(ctx), ea)
	_, dup := p.Prepare(ctx, spec(a))
	eb, err := p.Prepare(ctx, spec(b))
	if err != nil {
		return errored("SEC-ISO-07", err)
	}
	defer p.Destroy(context.WithoutCancel(ctx), eb)
	x, _ := p.get(ea)
	y, _ := p.get(eb)
	separate := x != nil && y != nil && x.vm != y.vm && x.vm.cmd.Process.Pid != y.vm.cmd.Process.Pid && x.vm.dir != y.vm.dir && x.vm.socket != y.vm.socket
	return check("SEC-ISO-07", errors.Is(dup, isolation.ErrOccupied) && separate, "a second environment for one attempt was refused and each attempt has its own VM, disk and vsock")
}

func (p *Provider) selfTestBuild(ctx context.Context, dockerfile string, limits isolation.Limits) (isolation.RunResult, isolation.Env, error) {
	src, err := os.MkdirTemp("", "samuori-selftest-")
	if err != nil {
		return isolation.RunResult{}, isolation.Env{}, err
	}
	defer os.RemoveAll(src)
	if err := os.WriteFile(filepath.Join(src, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return isolation.RunResult{}, isolation.Env{}, err
	}
	out, err := os.MkdirTemp("", "samuori-selftest-out-")
	if err != nil {
		return isolation.RunResult{}, isolation.Env{}, err
	}
	defer os.RemoveAll(out)
	if limits.WallClock == 0 {
		limits.WallClock = 15 * time.Minute
	}
	env, err := p.Prepare(ctx, isolation.Spec{AttemptID: "selftest-" + strings.ToLower(ids.New()), SourceDir: src, OutputDir: out, Profile: isolation.VMPerJob, Limits: limits})
	if err != nil {
		return isolation.RunResult{}, env, err
	}
	defer p.Destroy(context.WithoutCancel(ctx), env)
	plan, _ := buildplan.Plan{Version: 1, Method: "dockerfile", Platforms: []string{"linux/" + runtime.GOARCH}, Cache: buildplan.PlanCache{Mode: "none"},
		Dockerfile: &buildplan.PlanDockerfile{Context: ".", Path: "Dockerfile"}, Validation: buildplan.PlanValidation{Profile: buildplan.ProfileNone},
		Egress: buildplan.PlanEgress{Presets: []string{}, Allow: []buildplan.EgressRule{}}}.Encode()
	res, err := p.Run(ctx, env, plan)
	return res, env, err
}

// noCarryOver writes state into a build cache mount, which persists between builds of one daemon, and checks
// that the next attempt's VM starts without it and that the first VM left nothing on the host (SEC-ISO-09).
func (p *Provider) noCarryOver(ctx context.Context) isolation.Check {
	marker := strings.ToLower(ids.New())
	cacheRun := "RUN --mount=type=cache,target=/cache,id=samuori-selftest-state "
	_, first, err := p.selfTestBuild(ctx, "FROM "+rootlesshost.SelfTestImage+"\n"+cacheRun+"sh -c 'echo "+marker+" > /cache/marker && echo "+marker+" > /tmp/marker'\n", isolation.Limits{})
	if err != nil {
		return errored("SEC-ISO-09", err)
	}
	if _, err := os.Stat(filepath.Join(p.cfg.Dir, first.ID)); !errors.Is(err, os.ErrNotExist) {
		return check("SEC-ISO-09", false, "the first VM's directory still exists after destruction")
	}
	res, _, err := p.selfTestBuild(ctx, "FROM "+rootlesshost.SelfTestImage+"\n"+cacheRun+"sh -c 'echo SELFTEST-STATE $(cat /cache/marker 2>/dev/null || echo absent)'\n", isolation.Limits{})
	if err != nil {
		return errored("SEC-ISO-09", err)
	}
	logs := string(res.Logs)
	switch {
	case strings.Contains(logs, "SELFTEST-STATE "+marker):
		return check("SEC-ISO-09", false, "the second VM's build read the first build's cache mount")
	case !strings.Contains(logs, "SELFTEST-STATE absent"):
		return errored("SEC-ISO-09", errors.New("the probe build did not report"))
	}
	return check("SEC-ISO-09", true, "a cache mount written in one VM was absent in the next; the first VM's directory and disk were deleted")
}

// diskQuota fills more than the VM's scratch disk and expects a resource-limit failure (SEC-DOS-04).
func (p *Provider) diskQuota(ctx context.Context) isolation.Check {
	const disk = 4 << 30
	if disk > p.cfg.MaxDiskBytes {
		return errored("SEC-DOS-04", errors.New("the self-test needs a 4 GiB VM disk"))
	}
	_, _, err := p.selfTestBuild(ctx, "FROM "+rootlesshost.SelfTestImage+"\nRUN [\"dd\", \"if=/dev/zero\", \"of=/fill\", \"bs=1M\", \"count=6144\"]\n",
		isolation.Limits{VCPUs: p.cfg.DefaultVCPUs, MemoryBytes: p.cfg.DefaultMemoryBytes, DiskBytes: disk})
	return check("SEC-DOS-04", errors.Is(err, isolation.ErrResourceLimit), "writing 6 GiB on a 4 GiB VM disk ended as: "+clipN(errText(err), 300))
}
