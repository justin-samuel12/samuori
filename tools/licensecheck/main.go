// Command licensecheck classifies the licenses of everything Samuori links or bundles, enforces the policy in
// docs/dependencies.md, and writes an SPDX SBOM and a third-party notices file.
//
//	go run ./tools/licensecheck -root . [-sbom out.spdx.json] [-notices NOTICES.txt]
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/google/licensecheck"
)

// allowed are the licenses linked or bundled code may carry without review (docs/dependencies.md, license policy).
var allowed = map[string]bool{"Apache-2.0": true, "MIT": true, "BSD-2-Clause": true, "BSD-3-Clause": true, "ISC": true, "0BSD": true,
	"Zlib": true, "PostgreSQL": true, "blessing": true, "Unlicense": true, "CC0-1.0": true, "BSD-3-Clause-Clear": true, "MIT-0": true}

var denied = []string{"GPL", "AGPL", "SSPL", "BUSL", "Elastic", "CC-BY-NC", "Commons-Clause"}

// Component is one module or package that ships in a Samuori binary or the UI bundle.
type Component struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	License  string `json:"license"`
	Files    []string
	Notices  []string
	Packages []string
	Verdict  string `json:"verdict"`
	Reviewed string `json:"reviewed,omitempty"`
}

// review records an engineering license review. When Packages is set, the review covers only those packages of
// the module, for modules whose files carry different licenses.
type review struct {
	Name     string   `json:"name"`
	License  string   `json:"license"`
	Packages []string `json:"packages,omitempty"`
	Reason   string   `json:"reason"`
}

func main() {
	root := flag.String("root", ".", "repository root")
	sbom := flag.String("sbom", "", "write an SPDX 2.3 JSON SBOM here")
	notices := flag.String("notices", "", "write the third-party license and notice texts here")
	flag.Parse()
	reviews, err := loadReviews(filepath.Join(*root, "tools", "licensecheck", "reviewed.json"))
	if err != nil {
		fatal(err)
	}
	var comps []Component
	for _, target := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}} {
		cs, err := goModules(*root, target[0], target[1])
		if err != nil {
			fatal(err)
		}
		comps = merge(comps, cs)
	}
	ui, err := npmPackages(*root)
	if err != nil {
		fatal(err)
	}
	comps = append(comps, ui...)
	failed := 0
	for i := range comps {
		judge(&comps[i], reviews)
		if comps[i].Verdict == "fail" {
			failed++
			fmt.Fprintf(os.Stderr, "license-check: %s %s %s: %s\n", comps[i].Kind, comps[i].Name, comps[i].Version, comps[i].License)
		}
	}
	counts := map[string]int{}
	for _, c := range comps {
		counts[c.License]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("%d shipped components (Go modules linked into samuori-server, samuori-worker and samuori; UI runtime packages)\n", len(comps))
	for _, k := range keys {
		fmt.Printf("  %-40s %d\n", k, counts[k])
	}
	if *sbom != "" {
		if err := writeSBOM(*sbom, comps); err != nil {
			fatal(err)
		}
	}
	if *notices != "" {
		if err := writeNotices(*notices, comps); err != nil {
			fatal(err)
		}
	}
	if failed > 0 {
		fatal(fmt.Errorf("%d components need a license review (record it in tools/licensecheck/reviewed.json and docs/dependencies.md)", failed))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "license-check:", err)
	os.Exit(1)
}

func loadReviews(path string) (map[string]review, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]review{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []review
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]review{}
	for _, r := range list {
		out[r.Name] = r
	}
	return out, nil
}

// judge applies the policy for linked and bundled code: every license must be allowed, or reviewed for exactly
// the license now detected.
func judge(c *Component, reviews map[string]review) {
	ok := c.License != "" && c.License != "NOASSERTION"
	for _, id := range strings.FieldsFunc(strings.NewReplacer(" AND ", "|", " OR ", "|").Replace(c.License), func(r rune) bool { return r == '|' }) {
		if !allowed[strings.TrimSpace(id)] {
			ok = false
		}
	}
	c.Verdict = "pass"
	if ok {
		return
	}
	for _, d := range denied {
		if strings.Contains(c.License, d) {
			c.Verdict = "fail"
			return
		}
	}
	if r, found := reviews[c.Name]; found && r.License == c.License && covers(r.Packages, c.Packages) {
		c.Verdict, c.Reviewed = "pass", r.Reason
		return
	}
	c.Verdict = "fail"
}

func covers(reviewed, linked []string) bool {
	if len(reviewed) == 0 {
		return true
	}
	ok := map[string]bool{}
	for _, p := range reviewed {
		ok[p] = true
	}
	for _, p := range linked {
		if !ok[p] {
			return false
		}
	}
	return true
}

func goModules(root, goos, goarch string) ([]Component, error) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if .Module}}{{.Module.Path}}\t{{.Module.Version}}\t{{.Module.Dir}}\t{{.ImportPath}}{{end}}", "./cmd/...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list (%s/%s): %v: %s", goos, goarch, err, stderr.String())
	}
	byPath := map[string]*Component{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || strings.HasPrefix(f[0], "samuori.invalid/") {
			continue
		}
		c, ok := byPath[f[0]]
		if !ok {
			c = &Component{Kind: "go", Name: f[0], Version: f[1]}
			classify(c, f[2])
			byPath[f[0]] = c
			order = append(order, f[0])
		}
		c.Packages = append(c.Packages, f[3])
	}
	comps := make([]Component, 0, len(order))
	for _, p := range order {
		comps = append(comps, *byPath[p])
	}
	return comps, nil
}

func merge(a, b []Component) []Component {
	have := map[string]int{}
	for i, c := range a {
		have[c.Name] = i
	}
	for _, c := range b {
		i, ok := have[c.Name]
		if !ok {
			a = append(a, c)
			continue
		}
		for _, p := range c.Packages {
			if !slices.Contains(a[i].Packages, p) {
				a[i].Packages = append(a[i].Packages, p)
			}
		}
	}
	sort.Slice(a, func(i, j int) bool { return a[i].Name < a[j].Name })
	return a
}

var licenseNames = []string{"LICENSE", "LICENCE", "COPYING", "LICENSE-APACHE", "LICENSE-MIT"}

// classify reads the license and notice files at a component's root and names the licenses they match.
func classify(c *Component, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		c.License = "NOASSERTION"
		return
	}
	ids := map[string]bool{}
	for _, e := range entries {
		name := strings.ToUpper(e.Name())
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(name, "NOTICE") {
			c.Notices = append(c.Notices, path)
			continue
		}
		isLicense := false
		for _, n := range licenseNames {
			if strings.HasPrefix(name, n) {
				isLicense = true
			}
		}
		if !isLicense {
			continue
		}
		text, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		c.Files = append(c.Files, path)
		cov := licensecheck.Scan(text)
		if cov.Percent < 75 {
			ids["NOASSERTION"] = true
			continue
		}
		for _, m := range cov.Match {
			ids[m.ID] = true
		}
	}
	if len(ids) == 0 {
		c.License = "NOASSERTION"
		return
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	sort.Strings(list)
	c.License = strings.Join(list, " AND ")
}

// npmPackages lists the UI's production packages from the lockfile; their code ships in the UI bundle.
func npmPackages(root string) ([]Component, error) {
	raw, err := os.ReadFile(filepath.Join(root, "ui", "package-lock.json"))
	if err != nil {
		return nil, err
	}
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
			License any    `json:"license"`
			Dev     bool   `json:"dev"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		return nil, err
	}
	var comps []Component
	for key, p := range lock.Packages {
		if key == "" || p.Dev || !strings.HasPrefix(key, "node_modules/") {
			continue
		}
		name := key[strings.LastIndex(key, "node_modules/")+len("node_modules/"):]
		c := Component{Kind: "npm", Name: name, Version: p.Version, License: "NOASSERTION"}
		if s, ok := p.License.(string); ok && s != "" {
			c.License = strings.Trim(s, "()")
		}
		dir := filepath.Join(root, "ui", filepath.FromSlash(key))
		if _, err := os.Stat(dir); err == nil {
			declared := c.License
			classify(&c, dir)
			if declared != "NOASSERTION" {
				c.License = declared
			}
		}
		comps = append(comps, c)
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].Name < comps[j].Name })
	return comps, nil
}
