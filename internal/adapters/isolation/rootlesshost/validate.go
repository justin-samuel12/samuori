package rootlesshost

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/engine"
	"samuori.invalid/samuori/internal/imagescan"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/runtimeprobe"
)

func findings(fs []imagescan.Finding) []isolation.CheckFinding {
	out := make([]isolation.CheckFinding, 0, len(fs))
	for _, f := range fs {
		out = append(out, isolation.CheckFinding{Rule: f.Rule, Class: f.Class, Path: f.Path, Layer: f.Layer, Size: f.Size, SecretID: f.SecretID})
	}
	return out
}

// canary searches the final image for the exact bytes of every value delivered to the build (SEC-SEC-10). It is
// supplementary: transformed values are not detectable.
func canary(archive string, values map[string][]byte) isolation.Check {
	c := isolation.Check{ID: isolation.CheckCanary, Category: "sensitive-content", Outcome: "errored"}
	if len(values) == 0 {
		c.Outcome, c.Reason = "passed", "no secret values were delivered to this build"
		return c
	}
	f, err := os.Open(archive)
	if err != nil {
		c.Reason = "the image archive could not be opened"
		return c
	}
	defer f.Close()
	a, err := imagescan.Open(f, imagescan.DefaultLimits().MaxEntries)
	if err != nil {
		c.Reason = "the image archive could not be read"
		return c
	}
	rep := imagescan.Scan(a, imagescan.Options{Values: values, Limits: imagescan.DefaultLimits()})
	switch {
	case !rep.Complete:
		c.Reason = "the canary scan did not complete: " + rep.Reason
	case len(rep.Findings) > 0:
		c.Outcome, c.Reason, c.Findings = "blocked", fmt.Sprintf("delivered secret bytes found in %d place(s) in the image", len(rep.Findings)), findings(rep.Findings)
	default:
		c.Outcome, c.Reason = "passed", fmt.Sprintf("no delivered secret bytes in %d layer(s)", rep.Layers)
	}
	return c
}

// Canary scans an image archive for the values delivered to its build; it runs wherever the values are held.
func Canary(archive string, values map[string][]byte) isolation.Check { return canary(archive, values) }

var layoutName = regexp.MustCompile(`^(oci-layout|index\.json|blobs/sha256/[a-f0-9]{64})$`)

// unpackLayout extracts the OCI layout from the archive, accepting only layout file names.
func unpackLayout(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag != tar.TypeReg || !layoutName.MatchString(name) {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		out.Close()
		if err != nil {
			return err
		}
	}
}

// staticProbe checks that the probe binary can run inside any image: a static ELF for the worker's architecture.
func staticProbe(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return errors.New("the runtime probe is not a Linux executable")
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return errors.New("the runtime probe is dynamically linked; build samuori-worker with CGO_ENABLED=0")
		}
	}
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]
	if f.Machine != want {
		return errors.New("the runtime probe is built for another architecture")
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// runtimeTest runs the plan's runtime test against the built image for the worker's own platform (SEC-RTS-01).
func (p *Provider) runtimeTest(ctx context.Context, e *env, v buildplan.PlanValidation, archive string) (isolation.Check, string) {
	c := isolation.Check{ID: isolation.CheckRuntimePref + v.Profile, Category: "test", Outcome: "errored"}
	native := "linux/" + runtime.GOARCH
	f, err := os.Open(archive)
	if err != nil {
		c.Reason = "the image archive could not be opened"
		return c, ""
	}
	a, err := imagescan.Open(f, imagescan.DefaultLimits().MaxEntries)
	var imgs []imagescan.Image
	if err == nil {
		imgs, err = a.Images()
	}
	f.Close()
	if err != nil {
		c.Reason = "the image could not be read"
		return c, ""
	}
	var img *imagescan.Image
	for i := range imgs {
		if imgs[i].Platform == native {
			img = &imgs[i]
		}
	}
	if img == nil {
		c.Outcome, c.Reason = "skipped", "runtime tests run only for the worker's platform ("+native+")"
		return c, ""
	}
	if p.cfg.ProbeBinary == "" {
		c.Reason = "this worker has no runtime probe configured"
		return c, ""
	}
	if err := staticProbe(p.cfg.ProbeBinary); err != nil {
		c.Reason = err.Error()
		return c, ""
	}
	port := v.Port
	if port == 0 && (v.Profile == buildplan.ProfileHTTPStartup || v.Profile == buildplan.ProfileStaticServe) {
		for exposed := range img.Config.Config.ExposedPorts {
			if n, proto, _ := strings.Cut(exposed, "/"); proto == "" || proto == "tcp" {
				port, _ = strconv.Atoi(n)
				break
			}
		}
	}
	spec := runtimeprobe.Spec{Profile: v.Profile, Command: append(append([]string{}, img.Config.Config.Entrypoint...), img.Config.Config.Cmd...),
		User: img.Config.Config.User, Port: port, Path: v.Path, TimeoutSeconds: v.TimeoutSeconds}
	rawSpec, _ := json.Marshal(spec)
	run, _ := json.Marshal([]string{"/.samuori-probe/samuori-probe", "runtime-probe", string(rawSpec)})
	df := "FROM samuoriimage AS samuori-test\nUSER 0\nRUN --network=none --mount=type=bind,from=samuoriprobe,target=/.samuori-probe " + string(run) +
		"\nFROM scratch\nCOPY --from=samuori-test " + runtimeprobe.ResultPath + " /result.json\n"
	dirs := map[string]string{}
	for _, d := range []string{"layout", "probe", "test", "result"} {
		dirs[d] = filepath.Join(e.dir, "runtime-test", d)
		if err := os.MkdirAll(dirs[d], 0o700); err != nil {
			c.Reason = "the runtime test could not be prepared"
			return c, ""
		}
	}
	defer os.RemoveAll(filepath.Join(e.dir, "runtime-test"))
	if err := unpackLayout(archive, dirs["layout"]); err != nil {
		c.Reason = "the image layout could not be prepared"
		return c, ""
	}
	if err := copyFile(p.cfg.ProbeBinary, filepath.Join(dirs["probe"], "samuori-probe"), 0o755); err != nil {
		c.Reason = "the runtime probe could not be staged"
		return c, ""
	}
	if err := os.WriteFile(filepath.Join(dirs["test"], "Dockerfile"), []byte(df), 0o600); err != nil {
		c.Reason = "the runtime test could not be prepared"
		return c, ""
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(v.TimeoutSeconds+120)*time.Second)
	defer cancel()
	res, err := e.runner.Test(tctx, engine.TestRequest{DockerfileDir: dirs["test"], Layout: dirs["layout"], Manifest: img.Manifest, ProbeDir: dirs["probe"], OutputDir: dirs["result"], Platform: native})
	if err != nil {
		c.Reason = "the runtime test did not complete"
		return c, res.Log
	}
	raw, err := os.ReadFile(filepath.Join(dirs["result"], "result.json"))
	if err != nil || len(raw) > 64<<10 {
		c.Reason = "the runtime test produced no result"
		return c, res.Log
	}
	return probeCheck(c, raw), res.Log
}

// probeCheck accepts a probe result only when it is well formed and confirms the sandbox constraints.
func probeCheck(c isolation.Check, raw []byte) isolation.Check {
	var r runtimeprobe.Result
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || len(r.Reason) > 1024 {
		c.Outcome, c.Reason = "errored", "the runtime test result is malformed"
		return c
	}
	switch r.Outcome {
	case "passed", "failed", "errored":
	default:
		c.Outcome, c.Reason = "errored", "the runtime test result is malformed"
		return c
	}
	cons := r.Constraints
	if r.Outcome == "passed" && (!cons.NoNewPrivileges || !cons.CapabilitiesDropped || cons.UID == 0 || cons.Network != "loopback only") {
		c.Outcome, c.Reason = "errored", "the runtime test did not confirm its sandbox constraints"
		return c
	}
	c.Outcome, c.Reason = r.Outcome, r.Reason
	return c
}
