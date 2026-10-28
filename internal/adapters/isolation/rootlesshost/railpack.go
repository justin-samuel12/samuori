package rootlesshost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/adapters/build/buildkit"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/engine"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/redact"
)

// DefaultRailpackDir holds an installed Railpack: the railpack binary and a VERSION file (deploy/systemd/install.sh
// --railpack, docs/operators/providers.md).
const DefaultRailpackDir = "/usr/local/lib/samuori/providers/railpack"

const (
	railpackPlanFile = "railpack-plan.json"
	railpackInfoFile = "railpack-info.json"
	// railpackTransient is the exit status Railpack documents for a failure worth retrying, such as a download error.
	railpackTransient = 75
	maxRailpackOutput = 1 << 20
)

var railpackVersionRe = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)

// ProxyEnv are the variables Railpack build steps receive the egress proxy in. They arrive as BuildKit secrets
// because Railpack copies step variables into the image configuration.
var ProxyEnv = buildspec.RailpackProxyEnv

// installedRailpack reports the version of the Railpack installed in dir, if any.
func installedRailpack(dir string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "VERSION"))
	if err != nil {
		return "", false
	}
	st, err := os.Stat(filepath.Join(dir, "railpack"))
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	v := strings.TrimSpace(string(raw))
	if !railpackVersionRe.MatchString(v) {
		return "", false
	}
	return "v" + strings.TrimPrefix(v, "v"), true
}

// railpackRun is one attempt's Railpack planning: everything lives under dir, which the daemon's cleanup removes.
type railpackRun struct {
	p     *Provider
	e     *env
	plan  buildplan.Plan
	rp    *buildplan.PlanRailpack
	dir   string
	out   string
	mask  *redact.Masker
	proxy string
	noPx  []string
}

// railpack plans the build inside the build isolation and points req at the Railpack frontend (ADR 0047). It
// returns the lineage records and planning log.
func (p *Provider) railpack(ctx context.Context, e *env, plan buildplan.Plan, req *buildkit.Request, outDir, proxyURL string, mask *redact.Masker) ([]isolation.Record, string, error) {
	rp := plan.Railpack
	switch {
	case rp == nil:
		return nil, "", &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "the plan names the railpack method without a Railpack pin"}
	case p.cfg.Engine != engine.BuildKit:
		return nil, "", &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "Railpack builds need the BuildKit engine"}
	case !filepath.IsLocal(filepath.FromSlash(rp.Context)) && rp.Context != ".":
		return nil, "", &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "the Railpack context must stay inside the source"}
	}
	if proxyURL != "" {
		mask.Add("egress-proxy", []byte(proxyURL))
	}
	r := &railpackRun{p: p, e: e, plan: plan, rp: rp, dir: filepath.Join(e.daemonDir, "railpack"), out: outDir, mask: mask, proxy: proxyURL, noPx: req.NoProxy}
	bin, err := r.stage()
	if err != nil {
		return nil, "", err
	}
	log, err := r.prepare(ctx, bin)
	if err != nil {
		return nil, log, err
	}
	planRaw, err := os.ReadFile(filepath.Join(r.dir, "out", railpackPlanFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, log, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	infoRaw, _ := os.ReadFile(filepath.Join(r.dir, "out", railpackInfoFile))
	var records []isolation.Record
	if len(infoRaw) > 0 && len(infoRaw) <= 4<<20 {
		rec, err := writeRecord(filepath.Join(outDir, railpackInfoFile), infoRaw, "railpack-info")
		if err != nil {
			return nil, log, err
		}
		records = append(records, rec)
	}
	built, err := CheckRailpackPlan(planRaw, infoRaw, rp, boundEnv(plan))
	if err != nil {
		return records, log, fmt.Errorf("%w: %s", engine.ErrBuild, mask.Mask([]byte(err.Error())))
	}
	rec, err := writeRecord(filepath.Join(outDir, railpackPlanFile), built, "railpack-plan")
	if err != nil {
		return records, log, err
	}
	records = append(records, rec)
	feDir := filepath.Join(outDir, "railpack-frontend")
	if err := os.MkdirAll(feDir, 0o700); err != nil {
		return records, log, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	run := built
	if proxyURL != "" {
		if run, err = WithProxySecrets(built); err != nil {
			return records, log, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
		}
	}
	if err := os.WriteFile(filepath.Join(feDir, railpackPlanFile), run, 0o600); err != nil {
		return records, log, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	req.ContextDir = filepath.Join(e.spec.SourceDir, filepath.FromSlash(rp.Context))
	req.DockerfileDir, req.Filename = feDir, railpackPlanFile
	req.Frontend = rp.Frontend
	req.FrontendOpts = map[string]string{"build-arg:cache-key": rp.CacheKey, "build-arg:secrets-hash": secretsHash(plan)}
	req.Platforms = plan.Platforms
	req.BuildArgs = nil
	req.Secrets = railpackSecrets(req.Secrets, rp.SecretEnv, proxyURL, req.NoProxy)
	return records, log, nil
}

// stage lays out the planning directory and copies the context and a verified copy of the binary into it, so the
// file executed is the one whose checksum matched the approval.
func (r *railpackRun) stage() (string, error) {
	want := r.rp.Binaries[runtime.GOARCH]
	if want == "" {
		return "", &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "no Railpack binary is approved for " + runtime.GOARCH}
	}
	for _, d := range []string{"", "bin", "out", "mise", "home", "rk"} {
		if err := r.mkdir(filepath.Join(r.dir, d), 0o770); err != nil {
			return "", infra(err)
		}
	}
	bin := filepath.Join(r.dir, "bin", "railpack")
	got, err := copyVerified(filepath.Join(r.p.cfg.RailpackDir, "railpack"), bin, 0o750)
	if err != nil {
		return "", &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "railpack is not installed on this worker: " + err.Error()}
	}
	if got != want {
		_ = os.Remove(bin)
		return "", &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: fmt.Sprintf("the installed railpack binary (%s) is not the approved one (%s); it was not run", got, want)}
	}
	if err := r.share(bin, 0o750); err != nil {
		return "", infra(err)
	}
	if err := r.stageMise(); err != nil {
		return "", err
	}
	if r.proxy != "" {
		exports, err := proxyExports(r.proxy, r.noPx)
		if err != nil {
			return "", &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
		}
		if err := os.WriteFile(r.proxyEnvFile(), []byte(exports), 0o640); err != nil {
			return "", infra(err)
		}
		if err := r.share(r.proxyEnvFile(), 0o640); err != nil {
			return "", infra(err)
		}
	}
	src := filepath.Join(r.e.spec.SourceDir, filepath.FromSlash(r.rp.Context))
	if err := r.copyTree(src, filepath.Join(r.dir, "app")); err != nil {
		return "", infra(err)
	}
	return bin, nil
}

// railpackMise is the mise each Railpack release plans with, and the SHA-256 of the musl binary in mise's release
// archive for each architecture (the archive checked against upstream SHASUMS256.txt).
var railpackMise = map[string]struct {
	Version string
	SHA256  map[string]string
}{
	"v0.43.0": {"2026.10.14", map[string]string{
		"arm64": "sha256:c71eec4d0015f6ced74ac9d5cd755b56674df04e899d0d1566d533c95511ae6c",
		"amd64": "sha256:b0e388c8014f9ac14a5ddf7a4042b0db28043f7f9ce485ffc2521bff02247f8e",
	}},
}

// stageMise puts a verified mise where Railpack looks for it, so Railpack neither downloads an unverified one nor
// runs it with only PATH set: with strict egress the copy is reached through a wrapper that adds the build's egress
// proxy, mise's only route out (docs/spikes/e2e-providers.md).
func (r *railpackRun) stageMise() error {
	src := filepath.Join(r.p.cfg.RailpackDir, "mise")
	if _, err := os.Stat(src); err != nil {
		if r.proxy == "" {
			return nil
		}
		return &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "this worker has no pinned mise for Railpack, and Railpack runs its own without the egress proxy; reinstall with install.sh --railpack (docs/operators/providers.md)"}
	}
	pin, ok := railpackMise["v"+strings.TrimPrefix(r.rp.Version, "v")]
	want := pin.SHA256[runtime.GOARCH]
	if !ok || want == "" {
		return &isolation.Failure{Class: isolation.ErrUnsupported, Detail: fmt.Sprintf("no mise is pinned for Railpack %s on %s", r.rp.Version, runtime.GOARCH)}
	}
	dir := filepath.Join(r.dir, "mise", "mise")
	if err := r.mkdir(dir, 0o770); err != nil {
		return infra(err)
	}
	name := filepath.Join(dir, "mise-"+pin.Version)
	real := name
	if r.proxy != "" {
		// mise chooses its behaviour from the name it runs under, so the binary behind the wrapper is called mise.
		if err := r.mkdir(filepath.Join(dir, ".samuori"), 0o750); err != nil {
			return infra(err)
		}
		real = filepath.Join(dir, ".samuori", "mise")
	}
	got, err := copyVerified(src, real, 0o750)
	if err != nil {
		return infra(err)
	}
	if got != want {
		_ = os.Remove(real)
		return &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: fmt.Sprintf("the installed mise (%s) is not the one pinned for Railpack %s (%s); it was not run", got, r.rp.Version, want)}
	}
	if err := r.share(real, 0o750); err != nil {
		return infra(err)
	}
	if r.proxy == "" {
		return nil
	}
	wrapper, err := miseWrapper(real, r.proxy, r.noPx)
	if err != nil {
		return &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	if err := os.WriteFile(name, wrapper, 0o750); err != nil {
		return infra(err)
	}
	return r.share(name, 0o750)
}

func miseWrapper(real, proxy string, noProxy []string) ([]byte, error) {
	exports, err := proxyExports(proxy, noProxy)
	if err != nil || strings.ContainsAny(real, "'\n") {
		return nil, errors.New("the egress proxy settings cannot be passed to mise")
	}
	return fmt.Appendf(nil, "#!/bin/sh\n%sexec '%s' \"$@\"\n", exports, real), nil
}

// prepare runs railpack prepare as the build user in a user and mount namespace whose /tmp/railpack, where Railpack
// keeps mise, is this attempt's own directory. Its only network route is the build's egress proxy.
func (r *railpackRun) prepare(ctx context.Context, bin string) (string, error) {
	timeout := time.Duration(max(r.rp.TimeoutSeconds, 1)) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := filepath.Join(r.dir, "out")
	args := []string{"prepare", filepath.Join(r.dir, "app"), "--plan-out", filepath.Join(out, railpackPlanFile), "--info-out", filepath.Join(out, railpackInfoFile), "--hide-pretty-plan"}
	if r.rp.BuildCommand != "" {
		args = append(args, "--build-cmd", r.rp.BuildCommand)
	}
	if r.rp.StartCommand != "" {
		args = append(args, "--start-cmd", r.rp.StartCommand)
	}
	argv := r.prepareArgv(bin, args)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	logs := &cappedBuf{max: maxRailpackOutput}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		return "", infra(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		r.kill(cmd)
		<-done
		text := string(r.mask.Mask([]byte(logs.String())))
		return text, &isolation.Failure{Class: isolation.ErrResourceLimit, Detail: fmt.Sprintf("railpack prepare exceeded its %s limit", timeout)}
	}
	text := string(r.mask.Mask([]byte(logs.String())))
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() == railpackTransient:
		return text, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "railpack prepare reported a transient failure (exit 75), such as a download error"}
	case errors.As(err, &exit):
	default:
		return text, infra(err)
	}
	return text, nil
}

func (r *railpackRun) prepareArgv(bin string, args []string) []string {
	p := r.p
	argv := append([]string{}, p.opts.RunAs...)
	if p.cfg.CgroupLimits && p.runtime != "" {
		mem := r.e.spec.Limits.MemoryBytes
		if mem == 0 {
			mem = buildplan.DefaultLimits.MemoryBytes
		}
		argv = append(argv, "env", "XDG_RUNTIME_DIR="+p.runtime, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(p.runtime, "bus"),
			"systemd-run", "--user", "--scope", "--quiet", "--unit=samuori-rp-"+strings.ToLower(r.e.spec.AttemptID),
			"-p", fmt.Sprintf("MemoryMax=%d", mem), "-p", "MemorySwapMax=0", "-p", fmt.Sprintf("TasksMax=%d", p.cfg.Tasks), "--")
	}
	argv = append(argv, "env", "-i", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME="+filepath.Join(r.dir, "home"), "RAILPACK_VERBOSE=")
	envFile := ""
	if r.proxy != "" {
		envFile = r.proxyEnvFile()
	}
	argv = append(argv, p.cfg.Tools.Rootlesskit, "--net=host", "--state-dir="+filepath.Join(r.dir, "rk"),
		"sh", "-c", `mkdir -p /tmp/railpack && mount --bind "$1" /tmp/railpack && { [ -z "$2" ] || . "$2"; } && shift 2 && exec "$@"`, "samuori-railpack", filepath.Join(r.dir, "mise"), envFile, bin)
	return append(argv, args...)
}

// proxyEnvFile is where planning reads the egress proxy from, so the credential never appears on a command line
// (sudo logs those to the journal).
func (r *railpackRun) proxyEnvFile() string { return filepath.Join(r.dir, "proxy.env") }

// proxyExports are shell exports of the egress proxy for planning and for mise.
func proxyExports(proxy string, noProxy []string) (string, error) {
	np := strings.Join(noProxy, ",")
	if strings.ContainsAny(proxy+np, "'\n") {
		return "", errors.New("the egress proxy settings cannot be passed to planning")
	}
	var b strings.Builder
	for _, k := range ProxyEnv {
		v := proxy
		if strings.EqualFold(k, "NO_PROXY") {
			v = np
		}
		fmt.Fprintf(&b, "export %s='%s'\n", k, v)
	}
	return b.String(), nil
}

// kill stops railpack and everything it started; processes run as the build user, so they are matched by this
// attempt's unique directory.
func (r *railpackRun) kill(cmd *exec.Cmd) {
	if len(r.p.opts.RunAs) > 0 {
		argv := append(append([]string{}, r.p.opts.RunAs...), "pkill", "-KILL", "-f", "--", r.dir)
		_ = exec.Command(argv[0], argv[1:]...).Run()
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (r *railpackRun) gid() (int, bool) {
	if r.p.cfg.BuildGroup == "" {
		return 0, false
	}
	g, err := user.LookupGroup(r.p.cfg.BuildGroup)
	if err != nil {
		return 0, false
	}
	id, err := strconv.Atoi(g.Gid)
	return id, err == nil
}

// share hands p to the build group with perm; without a build group everything stays private to the worker.
func (r *railpackRun) share(p string, perm os.FileMode) error {
	gid, ok := r.gid()
	if !ok {
		return os.Chmod(p, perm&0o700)
	}
	if err := os.Chown(p, -1, gid); err != nil {
		return err
	}
	return os.Chmod(p, perm)
}

// mkdir creates a directory whose new files keep the build group.
func (r *railpackRun) mkdir(p string, perm os.FileMode) error {
	if err := os.MkdirAll(p, 0o700); err != nil {
		return err
	}
	if _, ok := r.gid(); ok {
		perm |= os.ModeSetgid
	}
	return r.share(p, perm)
}

// copyTree copies the context for planning, readable but not writable by the build user. Symbolic links are copied
// as links and never followed.
func (r *railpackRun) copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return r.mkdir(target, 0o750)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			if _, err := copyVerified(p, target, 0o640); err != nil {
				return err
			}
			return r.share(target, 0o640)
		}
		return nil
	})
}

// copyVerified copies src to a new file dst and returns the SHA-256 of the bytes written.
func copyVerified(src, dst string, mode os.FileMode) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func writeRecord(path string, raw []byte, kind string) (isolation.Record, error) {
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return isolation.Record{}, infra(err)
	}
	sum := sha256.Sum256(raw)
	return isolation.Record{Kind: kind, File: path, Digest: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

func infra(err error) error {
	return &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
}

// boundEnv lists the variables bound build secrets reach Railpack steps as; unbound optional secrets are left out,
// because Railpack mounts every listed secret as required.
func boundEnv(plan buildplan.Plan) []string {
	bound := map[string]bool{}
	for _, s := range plan.Secrets {
		bound[s.ID] = s.BindingID != ""
	}
	var out []string
	for env, id := range plan.Railpack.SecretEnv {
		if bound[id] {
			out = append(out, env)
		}
	}
	sort.Strings(out)
	return out
}

// secretsHash changes when a bound secret's binding version changes; it is derived from binding identities, never
// from values, which the worker does not hold before the build asks for them.
func secretsHash(plan buildplan.Plan) string {
	h := sha256.New()
	for _, s := range plan.Secrets {
		fmt.Fprintf(h, "%s=%s@%d\n", s.ID, s.BindingID, s.BindingVersion)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// railpackSecrets serves Railpack's secret requests, which name environment variables: proxy variables get the
// build's egress proxy, plan secrets resolve to their logical IDs through base, and anything else fails the step.
func railpackSecrets(base buildkit.SecretSource, env map[string]string, proxyURL string, noProxy []string) buildkit.SecretSource {
	return func(ctx context.Context, id string) ([]byte, error) {
		if proxyURL != "" {
			switch id {
			case "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy":
				return []byte(proxyURL), nil
			case "NO_PROXY", "no_proxy":
				return []byte(strings.Join(noProxy, ",")), nil
			}
		}
		logical, ok := env[id]
		if !ok {
			return nil, fmt.Errorf("unauthorized secret mount %q: it is not in the build plan", id)
		}
		return base(ctx, logical)
	}
}

type cappedBuf struct {
	b   []byte
	max int
}

func (c *cappedBuf) Write(p []byte) (int, error) {
	if room := c.max - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (c *cappedBuf) String() string { return string(c.b) }
