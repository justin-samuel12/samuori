package discovery

import (
	_ "embed"
	"encoding/json"
	"path"
	"strings"
	"time"
)

// downloader is one entry of the install-time downloader catalog: a package whose install script, or a command,
// downloads files from fixed hosts (ADR 0050).
type downloader struct {
	Ecosystem    string   `json:"ecosystem"`
	Package      string   `json:"package"`
	Command      []string `json:"command"`
	Versions     string   `json:"versions"`
	What         string   `json:"what"`
	Destinations []string `json:"destinations"`
	Severity     string   `json:"severity"`
	Confidence   string   `json:"confidence"`
	Fixer        string   `json:"fixer"`
	Fix          string   `json:"fix"`
	SkipEnv      []string `json:"skipEnv"`
	Source       string   `json:"source"`
}

type downloaderCatalog struct {
	ID      string       `json:"id"`
	Version string       `json:"version"`
	AsOf    string       `json:"asOf"`
	Entries []downloader `json:"entries"`
}

//go:embed downloaders-1.0.0.json
var downloadersJSON []byte

//go:embed runtime-eol-2026-10.json
var eolJSON []byte

var downloaders = mustDecode[downloaderCatalog](downloadersJSON)

type eolData struct {
	AsOf     string `json:"asOf"`
	Runtimes []struct {
		Name      string            `json:"name"`
		Images    []string          `json:"images"`
		Parts     int               `json:"parts"`
		Source    string            `json:"source"`
		EndOfLife map[string]string `json:"endOfLife"`
	} `json:"runtimes"`
}

var runtimeEOL = mustDecode[eolData](eolJSON)

func mustDecode[T any](raw []byte) T {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		panic("discovery: embedded data: " + err.Error())
	}
	return v
}

// DownloaderCatalog names the catalog version predictions use.
func DownloaderCatalog() string { return downloaders.ID + "@" + downloaders.Version }

func (d downloader) need(cause string) NetworkNeed {
	return NetworkNeed{Kind: "downloader", Cause: cause, Destinations: d.Destinations, Confidence: d.Confidence, Severity: d.Severity,
		Fixer: d.Fixer, Fix: d.Fix, skipEnv: d.SkipEnv}
}

// packageDownloaders matches one installed npm package against the catalog.
func packageDownloaders(name, ver string, exact bool) []NetworkNeed {
	var out []NetworkNeed
	for _, d := range downloaders.Entries {
		if d.Ecosystem != "npm" || d.Package != name {
			continue
		}
		v, ok := version{}, false
		if exact {
			v, _, ok = parseVersion(ver)
		} else {
			v, ok = lowest(ver)
		}
		if !ok && d.Versions != "*" {
			continue
		}
		if ok {
			if match, understood := satisfies(v, d.Versions); !understood || !match {
				continue
			}
		}
		label := name + " " + ver
		if !exact {
			label = name + " " + ver + " (no lockfile version; the range in package.json)"
		}
		n := d.need(label + " " + d.What)
		if !exact {
			n.Confidence = Possible
		}
		out = append(out, n)
	}
	return out
}

// commandDownloaders matches a command line, such as playwright install chromium, against the catalog.
func commandDownloaders(words []string, cause string) []NetworkNeed {
	if len(words) == 0 {
		return nil
	}
	first := path.Base(words[0])
	if at := strings.LastIndex(first, "@"); at > 0 {
		first = first[:at]
	}
	var out []NetworkNeed
	for _, d := range downloaders.Entries {
		if d.Ecosystem != "command" || len(words) < len(d.Command) || first != d.Command[0] {
			continue
		}
		match := true
		for i, w := range d.Command[1:] {
			match = match && words[i+1] == w
		}
		if match {
			out = append(out, d.need("`"+cause+"` "+d.What))
		}
	}
	return out
}

var now = time.Now

// eolAdvice reports a base image whose tag names a runtime release that is past, or within 90 days of, its end
// of life according to the dated EOL data.
func eolAdvice(image string) (Readiness, bool) {
	ref := image
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	colon := strings.LastIndex(ref, ":")
	if colon < 0 || strings.Contains(ref[colon:], "/") {
		return Readiness{}, false
	}
	repo, tag := ref[:colon], ref[colon+1:]
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	for _, rt := range runtimeEOL.Runtimes {
		matched := false
		for _, img := range rt.Images {
			matched = matched || repo == img
		}
		if !matched {
			continue
		}
		v, n, ok := parseVersion(tag)
		if !ok || n < rt.Parts {
			return Readiness{}, false
		}
		key := itoa(v[0])
		if rt.Parts == 2 {
			key += "." + itoa(v[1])
		}
		date, ok := rt.EndOfLife[key]
		if !ok {
			return Readiness{}, false
		}
		end, err := time.Parse(time.DateOnly, date)
		if err != nil {
			return Readiness{}, false
		}
		r := Readiness{Code: "runtime-end-of-life", Severity: Advice, Fixer: Developer, Confidence: Certain,
			Fix: "Move to a supported " + rt.Name + " release in the FROM line, and test the application on it."}
		switch {
		case now().After(end):
			r.Message = image + " is " + rt.Name + " " + key + ", which reached end of life on " + date + " and no longer receives security fixes (" + rt.Source + ", data as of " + runtimeEOL.AsOf + ")."
		case now().Add(90 * 24 * time.Hour).After(end):
			r.Message = image + " is " + rt.Name + " " + key + ", which reaches end of life on " + date + " (" + rt.Source + ")."
		default:
			return Readiness{}, false
		}
		return r, true
	}
	return Readiness{}, false
}
