package validation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/discovery/sandbox"
	"samuori.invalid/samuori/internal/imagescan"
	"samuori.invalid/samuori/internal/policy"
)

// ScanLimits bound the image-scan subprocess (SEC-OUT-07).
func ScanLimits() sandbox.Limits {
	l := sandbox.DefaultLimits()
	l.Timeout, l.CPUSeconds, l.AddressSpaceBytes, l.MaxInputBytes, l.MaxOutputBytes = 15*time.Minute, 900, 4<<30, 1<<20, 8<<20
	return l
}

type scanRequest struct {
	ExtraRules []policy.SensitiveRule `json:"extraRules"`
	Limits     imagescan.Limits       `json:"limits"`
}

// SandboxScanner runs imagescan in the restricted subprocess started by Command ("samuori-server scan-image").
type SandboxScanner struct {
	Runner sandbox.Runner
}

func (s SandboxScanner) Scan(ctx context.Context, archive string, rules []policy.SensitiveRule) (imagescan.Report, bool, error) {
	f, err := os.Open(archive)
	if err != nil {
		return imagescan.Report{}, false, err
	}
	defer f.Close()
	payload, _ := json.Marshal(scanRequest{ExtraRules: rules, Limits: imagescan.DefaultLimits()})
	out, sb, err := s.Runner.Exec(ctx, payload, f)
	if err != nil {
		return imagescan.Report{}, false, err
	}
	rep, err := decodeReport(out)
	return rep, !sb.Seccomp, err
}

// decodeReport treats the subprocess output as untrusted: strict decoding and bounded fields.
func decodeReport(raw []byte) (imagescan.Report, error) {
	var rep imagescan.Report
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		return imagescan.Report{}, fmt.Errorf("invalid scanner output: %w", err)
	}
	if len(rep.Findings) > imagescan.DefaultLimits().MaxFindings || len(rep.Images) > 64 || len(rep.Reason) > 2048 {
		return imagescan.Report{}, errors.New("invalid scanner output: out of bounds")
	}
	for _, f := range rep.Findings {
		if len(f.Rule) > 64 || len(f.Class) > 64 || len(f.Path) > 4096 || len(f.Layer) > 80 || len(f.SecretID) > 64 || f.Size < 0 {
			return imagescan.Report{}, errors.New("invalid scanner output: oversized finding")
		}
	}
	return rep, nil
}

// ScanChildMain is the subprocess entry point: restrict first, then read the archive from file descriptor 3.
func ScanChildMain(stdin io.Reader, stdout io.Writer) error {
	if err := sandbox.Restrict(sandbox.ChildLimits()); err != nil {
		return err
	}
	var req scanRequest
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&req); err != nil {
		return fmt.Errorf("reading request: %w", err)
	}
	if req.Limits == (imagescan.Limits{}) {
		req.Limits = imagescan.DefaultLimits()
	}
	f := os.NewFile(3, "image")
	if f == nil {
		return errors.New("no image archive")
	}
	rules := imagescan.DefaultRules()
	for _, r := range req.ExtraRules {
		rules = append(rules, imagescan.Rule{ID: "policy-" + r.ID, Class: r.Class, Paths: []string{r.Path}})
	}
	a, err := imagescan.Open(f, req.Limits.MaxEntries)
	if err != nil {
		return json.NewEncoder(stdout).Encode(imagescan.Report{Images: []string{}, Findings: []imagescan.Finding{}, Reason: err.Error()})
	}
	return json.NewEncoder(stdout).Encode(imagescan.Scan(a, imagescan.Options{Rules: rules, Metadata: true, Limits: req.Limits}))
}

// Syft generates SPDX SBOMs with an installed syft binary, without network access.
type Syft struct {
	Path    string
	Timeout time.Duration

	once    sync.Once
	version string
}

// NewSyft finds syft on PATH; it returns nil when syft is not installed.
func NewSyft() *Syft {
	p, err := exec.LookPath("syft")
	if err != nil {
		return nil
	}
	return &Syft{Path: p, Timeout: 10 * time.Minute}
}

func (s *Syft) env(dir string) []string {
	return []string{"HOME=" + dir, "TMPDIR=" + dir, "SYFT_CHECK_FOR_APP_UPDATE=false", "PATH=/usr/bin:/bin"}
}

func (s *Syft) run(ctx context.Context, max int64, args ...string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "samuori-syft-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	try := func(isolated bool) ([]byte, error) {
		cmd := exec.CommandContext(ctx, s.Path, args...)
		cmd.Env, cmd.Dir = s.env(dir), dir
		if isolated {
			cmd.SysProcAttr = sandbox.Isolated()
		}
		out := &limited{max: max}
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = out, &limited{max: 16 << 10, buf: &stderr}
		if err := cmd.Run(); err != nil {
			if out.over {
				return nil, errors.New("output too large")
			}
			return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
		}
		return out.bytes(), nil
	}
	out, err := try(true)
	if err != nil && ctx.Err() == nil && strings.Contains(err.Error(), "operation not permitted") {
		out, err = try(false)
	}
	return out, err
}

func (s *Syft) Generate(ctx context.Context, archive string) ([]byte, Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	s.once.Do(func() {
		if out, err := s.run(ctx, 64<<10, "version"); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Version" {
					s.version = strings.TrimSpace(v)
				}
			}
		}
	})
	tool := Tool{Name: "syft", Version: s.version}
	abs, err := filepath.Abs(archive)
	if err != nil {
		return nil, tool, err
	}
	out, err := s.run(ctx, 256<<20, "scan", "oci-archive:"+abs, "-o", "spdx-json", "-q")
	if err != nil {
		return nil, tool, err
	}
	if !json.Valid(out) {
		return nil, tool, errors.New("syft produced invalid JSON")
	}
	return out, tool, nil
}

type limited struct {
	max  int64
	buf  *bytes.Buffer
	over bool
}

func (l *limited) Write(p []byte) (int, error) {
	if l.buf == nil {
		l.buf = &bytes.Buffer{}
	}
	if int64(l.buf.Len()+len(p)) > l.max {
		l.over = true
		return 0, errors.New("output limit")
	}
	return l.buf.Write(p)
}

func (l *limited) bytes() []byte {
	if l.buf == nil {
		return nil
	}
	return l.buf.Bytes()
}
