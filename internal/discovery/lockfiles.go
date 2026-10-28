package discovery

import (
	"bufio"
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

type lockedPackage struct {
	name, version, resolved string
	dev, installScript      bool
}

// npmLock reads package-lock.json or npm-shrinkwrap.json, lockfile versions 1 to 3.
func npmLock(raw []byte) ([]lockedPackage, bool) {
	var lock struct {
		Packages map[string]struct {
			Version          string   `json:"version"`
			Resolved         string   `json:"resolved"`
			Dev              bool     `json:"dev"`
			HasInstallScript bool     `json:"hasInstallScript"`
			Link             bool     `json:"link"`
			OS               []string `json:"os"`
		} `json:"packages"`
		Dependencies map[string]npmV1Dep `json:"dependencies"`
	}
	if json.Unmarshal(raw, &lock) != nil {
		return nil, false
	}
	var out []lockedPackage
	if len(lock.Packages) > 0 {
		for key, p := range lock.Packages {
			i := strings.LastIndex(key, "node_modules/")
			if i < 0 || p.Link || !installsOnLinux(p.OS) {
				continue
			}
			out = append(out, lockedPackage{name: key[i+len("node_modules/"):], version: p.Version, resolved: p.Resolved, dev: p.Dev, installScript: p.HasInstallScript})
		}
	} else {
		var walk func(map[string]npmV1Dep, int)
		walk = func(deps map[string]npmV1Dep, depth int) {
			if depth > 64 {
				return
			}
			for name, d := range deps {
				out = append(out, lockedPackage{name: name, version: d.Version, resolved: d.Resolved, dev: d.Dev})
				walk(d.Dependencies, depth+1)
			}
		}
		walk(lock.Dependencies, 0)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name+"@"+out[i].version < out[j].name+"@"+out[j].version })
	return out, true
}

type npmV1Dep struct {
	Version      string              `json:"version"`
	Resolved     string              `json:"resolved"`
	Dev          bool                `json:"dev"`
	Dependencies map[string]npmV1Dep `json:"dependencies"`
}

// yarnLock reads yarn.lock (classic and Berry): an unindented header names the package, an indented version line
// gives the version.
func yarnLock(raw []byte) []lockedPackage {
	var out []lockedPackage
	var cur *lockedPackage
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case !strings.HasPrefix(line, " "):
			spec := strings.Trim(strings.SplitN(strings.TrimSuffix(line, ":"), ",", 2)[0], `" `)
			name := spec
			if at := strings.LastIndex(spec, "@"); at > 0 {
				name = spec[:at]
			}
			out = append(out, lockedPackage{name: name})
			cur = &out[len(out)-1]
		case cur != nil:
			t := strings.TrimSpace(line)
			k, v, _ := strings.Cut(t, " ")
			v = strings.Trim(v, `" `)
			switch strings.TrimSuffix(k, ":") {
			case "version":
				cur.version = v
			case "resolved":
				cur.resolved = v
			}
		}
	}
	return out
}

var pnpmKey = regexp.MustCompile(`^  ['"]?/?((?:@[^/@\s'"]+/)?[^/@\s'"(]+)[@/](\d+\.\d+\.\d+[^:'"\s(]*)`)

// pnpmLock reads the package keys of pnpm-lock.yaml (formats 5 to 9) without a YAML parser.
func pnpmLock(raw []byte) []lockedPackage {
	var out []lockedPackage
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line != "" && !strings.HasPrefix(line, " ") {
			section = strings.TrimSuffix(line, ":")
			continue
		}
		if section != "packages" && section != "snapshots" {
			continue
		}
		if m := pnpmKey.FindStringSubmatch(line); m != nil {
			out = append(out, lockedPackage{name: m[1], version: m[2]})
		}
	}
	return out
}

// npmrcRegistries reads registry lines of an .npmrc; values with variables are unknown and skipped.
func npmrcRegistries(raw []byte) (def string, scoped []string) {
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") || strings.HasPrefix(k, ";") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case k == "registry":
			def = v
		case strings.HasPrefix(k, "@") && strings.HasSuffix(k, ":registry"):
			scoped = append(scoped, v)
		}
	}
	return def, scoped
}

// pipIndexes reads index options from a requirements file.
func pipIndexes(raw []byte) []string {
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(strings.ReplaceAll(strings.TrimSpace(line), "=", " "))
		if len(f) >= 2 && (f[0] == "--index-url" || f[0] == "-i" || f[0] == "--extra-index-url" || f[0] == "--find-links" || f[0] == "-f") {
			out = append(out, f[1])
		}
	}
	return out
}

// installsOnLinux applies npm's os field: builds run on Linux, so packages limited to other systems are skipped.
func installsOnLinux(osList []string) bool {
	if len(osList) == 0 {
		return true
	}
	allowed := false
	for _, o := range osList {
		switch {
		case o == "!linux":
			return false
		case strings.HasPrefix(o, "!"):
			allowed = true
		case o == "linux":
			return true
		}
	}
	return allowed
}
