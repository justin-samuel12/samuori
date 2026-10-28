package discovery

import (
	"regexp"
	"strings"
)

// Static readers for registry and repository settings in project files (ADR 0050). Each reads literal values only;
// values with variables are unknown and skipped by destination.

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// yarnrcRegistries reads npmRegistryServer from a Yarn Berry .yarnrc.yml: the top-level value is the default
// registry, indented ones belong to npmScopes.
func yarnrcRegistries(raw []byte) (def string, scoped []string) {
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok || strings.TrimSpace(k) != "npmRegistryServer" {
			continue
		}
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		v = unquote(v)
		if line[0] == ' ' || line[0] == '\t' {
			scoped = append(scoped, v)
		} else {
			def = v
		}
	}
	return def, scoped
}

var pipConfKeys = map[string]bool{"index-url": true, "extra-index-url": true, "find-links": true}

// pipConfIndexes reads index-url, extra-index-url and find-links from a pip.conf, with continuation lines.
func pipConfIndexes(raw []byte) []string {
	var out []string
	current := ""
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed[0] == '#' || trimmed[0] == ';' {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if current != "" {
				out = append(out, strings.Fields(trimmed)...)
			}
			continue
		}
		current = ""
		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		if k = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(k)), "_", "-"); pipConfKeys[k] {
			current = k
			out = append(out, strings.Fields(v)...)
		}
	}
	return out
}

var tomlString = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)

func tomlStrings(v string) []string {
	var out []string
	for _, m := range tomlString.FindAllStringSubmatch(v, -1) {
		out = append(out, m[1]+m[2])
	}
	return out
}

// Index tables whose url key names a package index, and keys of plain tables whose values do.
var (
	indexTables = map[string]bool{"tool.poetry.source": true, "tool.pdm.source": true, "tool.uv.index": true, "index": true}
	indexKeys   = map[string]map[string]bool{
		"tool.uv": {"index-url": true, "extra-index-url": true, "find-links": true},
		"":        {"index-url": true, "extra-index-url": true, "find-links": true},
	}
)

// tomlIndexes reads package indexes from pyproject.toml (Poetry, PDM and uv sources) or uv.toml. Only single-line
// values are read.
func tomlIndexes(raw []byte, uvFile bool) []string {
	var out []string
	table, array := "", false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			array = strings.HasPrefix(trimmed, "[[")
			table = strings.TrimSpace(strings.Trim(strings.SplitN(trimmed, "#", 2)[0], " []"))
			continue
		}
		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		name := table
		if !uvFile && (name == "" || name == "index") {
			continue
		}
		switch {
		case array && indexTables[name] && k == "url":
			out = append(out, tomlStrings(v)...)
		case !array && indexKeys[name][k]:
			out = append(out, tomlStrings(v)...)
		}
	}
	return out
}

var gradleRepositoryURL = regexp.MustCompile(`["'](https?://[^"'\s]+)["']`)

// gradleRepositories reads literal http(s) URLs inside the repositories blocks of a Gradle build or settings
// script, Groovy or Kotlin.
func gradleRepositories(raw []byte) []string {
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines[i] = ""
		}
	}
	src := strings.Join(lines, "\n")
	var out []string
	for i := 0; i < len(src); {
		j := strings.Index(src[i:], "repositories")
		if j < 0 {
			break
		}
		start := i + j + len("repositories")
		i = start
		k := start
		for k < len(src) && (src[k] == ' ' || src[k] == '\t' || src[k] == '\n' || src[k] == '\r') {
			k++
		}
		if k >= len(src) || src[k] != '{' {
			continue
		}
		depth, end := 0, len(src)
		for p := k; p < len(src); p++ {
			if src[p] == '{' {
				depth++
			} else if src[p] == '}' {
				if depth--; depth == 0 {
					end = p
					break
				}
			}
		}
		for _, m := range gradleRepositoryURL.FindAllStringSubmatch(src[k:end], -1) {
			out = append(out, m[1])
		}
		i = end
	}
	return out
}

func gradleScript(name string) bool {
	switch name {
	case "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts":
		return true
	}
	return false
}
