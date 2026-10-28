package validation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/policy"
)

// CheckVulnerabilities is the controller check that runs the configured vulnerability scanner.
const CheckVulnerabilities = "vulnerabilities"

// Vulnerability is one finding; severity is lowercase (critical, high, medium, low, negligible or unknown).
type Vulnerability struct {
	ID           string `json:"id"`
	Severity     string `json:"severity"`
	Package      string `json:"package"`
	Version      string `json:"version"`
	FixedVersion string `json:"fixedVersion,omitempty"`
	Outcome      string `json:"outcome,omitempty"`
}

// VulnerabilityScanner scans an OCI archive. No scanner is selected by default (ADR 0021, OD-10): operators install
// one and choose it.
type VulnerabilityScanner interface {
	Scan(ctx context.Context, archive string) ([]Vulnerability, Tool, error)
}

// NewVulnerabilityScanner returns the named scanner if its binary is installed: grype or trivy. An empty name means
// no scanner.
func NewVulnerabilityScanner(name string) (VulnerabilityScanner, error) {
	switch name {
	case "":
		return nil, nil
	case "grype", "trivy":
		path, err := exec.LookPath(name)
		if err != nil {
			return nil, fmt.Errorf("vulnerability scanner %s: %w", name, err)
		}
		if name == "grype" {
			return &Grype{Path: path, Timeout: 15 * time.Minute}, nil
		}
		return &Trivy{Path: path, Timeout: 15 * time.Minute}, nil
	}
	return nil, fmt.Errorf("unknown vulnerability scanner %q: use grype or trivy", name)
}

// runTool runs a scanner with a scratch home, a timeout and an output limit.
func runTool(ctx context.Context, timeout time.Duration, env []string, path string, args ...string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "samuori-vuln-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"HOME=" + dir, "TMPDIR=" + dir, "PATH=/usr/local/bin:/usr/bin:/bin"}, env...)
	out := &limited{max: 64 << 20}
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

// cacheDir keeps the scanner's database in its usual per-user location, or where the operator's variable points,
// because each scan runs with a scratch HOME.
func cacheDir(variable, tool string) string {
	if dir := os.Getenv(variable); dir != "" {
		return dir
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, tool)
	}
	return ""
}

// Grype runs an operator-installed grype. Its database lives in GRYPE_DB_CACHE_DIR or grype's usual cache directory,
// and grype's own update settings apply.
type Grype struct {
	Path    string
	Timeout time.Duration
}

func (g *Grype) Scan(ctx context.Context, archive string) ([]Vulnerability, Tool, error) {
	env := []string{"GRYPE_CHECK_FOR_APP_UPDATE=false"}
	if dir := cacheDir("GRYPE_DB_CACHE_DIR", filepath.Join("grype", "db")); dir != "" {
		env = append(env, "GRYPE_DB_CACHE_DIR="+dir)
	}
	raw, err := runTool(ctx, g.Timeout, env, g.Path, "oci-archive:"+archive, "-o", "json", "-q")
	if err != nil {
		return nil, Tool{Name: "grype"}, err
	}
	return ParseGrype(raw)
}

// ParseGrype reads grype's JSON report.
func ParseGrype(raw []byte) ([]Vulnerability, Tool, error) {
	var r struct {
		Matches []struct {
			Vulnerability struct {
				ID       string `json:"id"`
				Severity string `json:"severity"`
				Fix      struct {
					Versions []string `json:"versions"`
				} `json:"fix"`
			} `json:"vulnerability"`
			Artifact struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"artifact"`
		} `json:"matches"`
		Descriptor struct {
			Name    string         `json:"name"`
			Version string         `json:"version"`
			DB      map[string]any `json:"db"`
		} `json:"descriptor"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, Tool{Name: "grype"}, fmt.Errorf("grype report: %w", err)
	}
	tool := Tool{Name: "grype", Version: r.Descriptor.Version, Database: describeDB(r.Descriptor.DB)}
	out := []Vulnerability{}
	for _, m := range r.Matches {
		v := Vulnerability{ID: m.Vulnerability.ID, Severity: strings.ToLower(m.Vulnerability.Severity), Package: m.Artifact.Name, Version: m.Artifact.Version}
		if len(m.Vulnerability.Fix.Versions) > 0 {
			v.FixedVersion = m.Vulnerability.Fix.Versions[0]
		}
		out = append(out, v)
	}
	return out, tool, nil
}

// describeDB summarizes the database fields grype reports, which differ between grype versions.
func describeDB(db map[string]any) string {
	var parts []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for _, k := range []string{"schemaVersion", "built", "from", "location", "valid"} {
			if v, ok := m[k]; ok {
				parts = append(parts, prefix+k+"="+fmt.Sprint(v))
			}
		}
		if st, ok := m["status"].(map[string]any); ok {
			walk("", st)
		}
	}
	walk("", db)
	return strings.Join(parts, " ")
}

// Trivy runs an operator-installed trivy against the archive without contacting a registry.
type Trivy struct {
	Path    string
	Timeout time.Duration
}

func (tv *Trivy) Scan(ctx context.Context, archive string) ([]Vulnerability, Tool, error) {
	env := []string{}
	if dir := cacheDir("TRIVY_CACHE_DIR", "trivy"); dir != "" {
		env = append(env, "TRIVY_CACHE_DIR="+dir)
	}
	raw, err := runTool(ctx, tv.Timeout, env, tv.Path, "image", "--input", archive, "--format", "json", "--quiet", "--scanners", "vuln")
	if err != nil {
		return nil, Tool{Name: "trivy"}, err
	}
	vulns, tool, err := ParseTrivy(raw)
	if err != nil {
		return nil, tool, err
	}
	if version, err := runTool(ctx, time.Minute, env, tv.Path, "--version", "--format", "json"); err == nil {
		tool.Version, tool.Database = parseTrivyVersion(version)
	}
	return vulns, tool, nil
}

// ParseTrivy reads trivy's JSON report.
func ParseTrivy(raw []byte) ([]Vulnerability, Tool, error) {
	var r struct {
		Results []struct {
			Vulnerabilities []struct {
				ID       string `json:"VulnerabilityID"`
				Package  string `json:"PkgName"`
				Version  string `json:"InstalledVersion"`
				Fixed    string `json:"FixedVersion"`
				Severity string `json:"Severity"`
			} `json:"Vulnerabilities"`
		} `json:"Results"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, Tool{Name: "trivy"}, fmt.Errorf("trivy report: %w", err)
	}
	out := []Vulnerability{}
	for _, res := range r.Results {
		for _, v := range res.Vulnerabilities {
			out = append(out, Vulnerability{ID: v.ID, Severity: strings.ToLower(v.Severity), Package: v.Package, Version: v.Version, FixedVersion: v.Fixed})
		}
	}
	return out, Tool{Name: "trivy"}, nil
}

func parseTrivyVersion(raw []byte) (string, string) {
	var v struct {
		Version         string `json:"Version"`
		VulnerabilityDB struct {
			Version   int    `json:"Version"`
			UpdatedAt string `json:"UpdatedAt"`
		} `json:"VulnerabilityDB"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return "", ""
	}
	db := ""
	if v.VulnerabilityDB.UpdatedAt != "" {
		db = fmt.Sprintf("schemaVersion=%d updated=%s", v.VulnerabilityDB.Version, v.VulnerabilityDB.UpdatedAt)
	}
	return v.Version, db
}

// vulnerabilityCheck runs the configured scanner. Findings at or above the policy's failOnSeverity fail the check
// unless listed in ignoreVulnerabilities; with no scanner the check is skipped, which fails closed when required.
func (s *Service) vulnerabilityCheck(ctx context.Context, r Run, eff policy.Effective) Check {
	c := Check{ID: CheckVulnerabilities, Category: "scan", Origin: "controller", Outcome: "errored"}
	if s.vulns == nil {
		c.Outcome, c.Reason = "skipped", "no vulnerability scanner is configured; an administrator chooses one (no default until database terms are verified, ADR 0021)"
		return c
	}
	archive, err := s.blobs.File(r.ArtifactDigest)
	if err != nil {
		c.Reason = "the image is not in internal storage"
		return c
	}
	name := archive.Name()
	archive.Close()
	vulns, tool, err := s.vulns.Scan(ctx, name)
	c.Tool = tool
	if err != nil {
		c.Reason = "the vulnerability scan did not complete: " + clip(err.Error(), 300)
		return c
	}
	threshold := slices.Index(policy.Severities, eff.Validation.FailOnSeverity)
	counts := map[string]int{}
	failing := 0
	for i := range vulns {
		v := &vulns[i]
		counts[v.Severity]++
		rank := slices.Index(policy.Severities, v.Severity)
		switch {
		case slices.Contains(eff.Validation.IgnoreVulnerabilities, v.ID):
			v.Outcome = "ignored"
		case threshold >= 0 && rank >= 0 && rank <= threshold:
			v.Outcome = "fail"
			failing++
		default:
			v.Outcome = "report"
		}
	}
	c.Vulnerabilities = vulns
	summary := fmt.Sprintf("%d critical, %d high, %d medium, %d low", counts["critical"], counts["high"], counts["medium"], counts["low"])
	if failing > 0 {
		c.Outcome, c.Reason = "failed", fmt.Sprintf("%d finding(s) at or above %s (%s)", failing, eff.Validation.FailOnSeverity, summary)
	} else {
		c.Outcome, c.Reason = "passed", summary
	}
	if tool.Database != "" {
		c.Reason += "; database " + clip(tool.Database, 200)
	}
	return c
}
