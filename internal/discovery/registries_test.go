package discovery

import (
	"strings"
	"testing"
)

func destinations(needs []NetworkNeed) map[string]NetworkNeed {
	out := map[string]NetworkNeed{}
	for _, n := range needs {
		for _, d := range n.Destinations {
			out[d] = n
		}
	}
	return out
}

func TestPythonIndexConfiguration(t *testing.T) {
	pyproject := `[project]
name = "orders"
requires-python = ">=3.12"

[[tool.poetry.source]]
name = "internal"
url = "https://poetry.example.com/simple/"
priority = "primary"

[[tool.pdm.source]]
name = "pdm"
url = "https://pdm.example.com/simple"

[[tool.pdm.source]]
url = "https://${PDM_HOST}/simple"

[[tool.uv.index]]
name = "uv"
url = "https://uv.example.com/simple"

[tool.uv]
extra-index-url = ["https://uv-extra.example.com/simple"]

[tool.other]
url = "https://not-an-index.example.com"
`
	pipConf := `[global]
index-url = https://pipconf.example.com/simple
extra-index-url =
    https://pipconf-extra.example.com/simple
    https://pipconf-third.example.com/simple
timeout = 60
`
	uvToml := `index-url = "https://uvtoml.example.com/simple"

[[index]]
url = "https://uvtoml-index.example.com/simple"
`
	df := "FROM python:3.12-slim\nCOPY . .\nRUN pip install -r requirements.txt\n"
	r := Analyse(Input{Files: []InputFile{
		{Path: "Dockerfile", Content: []byte(df)},
		{Path: "requirements.txt", Content: []byte("--extra-index-url https://req.example.com/simple\nflask\n")},
		{Path: "pyproject.toml", Content: []byte(pyproject)},
		{Path: "pip.conf", Content: []byte(pipConf)},
		{Path: "uv.toml", Content: []byte(uvToml)},
		{Path: "app.py"},
	}})
	if len(r.Dockerfiles) != 1 {
		t.Fatalf("dockerfiles = %+v", r.Dockerfiles)
	}
	got := destinations(r.Dockerfiles[0].NetworkNeeds)
	for dest, path := range map[string]string{
		"req.example.com:443":           "requirements.txt",
		"poetry.example.com:443":        "pyproject.toml",
		"pdm.example.com:443":           "pyproject.toml",
		"uv.example.com:443":            "pyproject.toml",
		"uv-extra.example.com:443":      "pyproject.toml",
		"pipconf.example.com:443":       "pip.conf",
		"pipconf-extra.example.com:443": "pip.conf",
		"pipconf-third.example.com:443": "pip.conf",
		"uvtoml.example.com:443":        "uv.toml",
		"uvtoml-index.example.com:443":  "uv.toml",
	} {
		n, ok := got[dest]
		if !ok || n.Path != path || n.Kind != "registry" {
			t.Errorf("%s: need = %+v; want a registry need from %s", dest, n, path)
		}
	}
	if n := got["pipconf.example.com:443"]; n.Confidence != Possible {
		t.Errorf("pip.conf confidence = %q; pip reads it only where the build installs it", n.Confidence)
	}
	for _, bad := range []string{"not-an-index.example.com:443"} {
		if _, ok := got[bad]; ok {
			t.Errorf("%s is not a package index", bad)
		}
	}
	for d := range got {
		if strings.Contains(d, "$") {
			t.Errorf("a URL with a variable became a destination: %s", d)
		}
	}
	offline := Analyse(Input{Files: []InputFile{
		{Path: "Dockerfile", Content: []byte("FROM python:3.12-slim\nCOPY . .\nRUN python app.py\n")},
		{Path: "pyproject.toml", Content: []byte(pyproject)},
	}})
	if _, ok := destinations(offline.Dockerfiles[0].NetworkNeeds)["poetry.example.com:443"]; ok {
		t.Error("a Dockerfile that installs nothing does not need the project's indexes")
	}
}

func TestYarnBerryRegistries(t *testing.T) {
	yarnrc := `nodeLinker: node-modules
npmRegistryServer: "https://yarn.example.com"  # company mirror
npmScopes:
  acme:
    npmRegistryServer: 'https://acme.example.com/npm'
  vars:
    npmRegistryServer: "${SCOPE_REGISTRY}"
`
	df := "FROM node:22-slim\nCOPY . .\nRUN yarn install --immutable\nCMD [\"node\", \"server.js\"]\n"
	r := Analyse(Input{Files: []InputFile{
		{Path: "Dockerfile", Content: []byte(df)},
		{Path: "package.json", Content: []byte(`{"name":"web","packageManager":"yarn@4.5.0","dependencies":{"@acme/ui":"^1.0.0"}}`)},
		{Path: ".yarnrc.yml", Content: []byte(yarnrc)},
		{Path: "server.js"},
	}})
	needs := r.Dockerfiles[0].NetworkNeeds
	got := destinations(needs)
	for _, dest := range []string{"yarn.example.com:443", "acme.example.com:443"} {
		if n, ok := got[dest]; !ok || n.Path != ".yarnrc.yml" {
			t.Errorf("%s: need = %+v; want one from .yarnrc.yml", dest, n)
		}
	}
	if n := needFor(needs, func(n NetworkNeed) bool { return n.Preset == "yarn" && n.Line == 3 }); n != nil {
		t.Errorf("the default registry replaces the yarn preset at the install line: %+v", n)
	}
	if len(got) != 2 {
		t.Errorf("destinations = %v; want exactly the two literal registries", got)
	}
}

func TestGradleRepositories(t *testing.T) {
	build := `plugins { id 'java' }
ext { docs = "https://docs.example.org" }
repositories {
    mavenCentral()
    maven { url "https://maven.example.com/releases" }
    // maven { url "https://commented.example.com/maven" }
    maven {
        url = uri("https://nested.example.com/m2")
        credentials { username = "u" }
    }
    maven { url "${repoBase}/libs" }
}
`
	settings := `pluginManagement {
    repositories {
        gradlePluginPortal()
        maven("https://plugins.example.net/m2")
    }
}
rootProject.name = "orders"
`
	files := func(df string) Input {
		return Input{Files: []InputFile{
			{Path: "Dockerfile", Content: []byte(df)},
			{Path: "build.gradle", Content: []byte(build)},
			{Path: "settings.gradle.kts", Content: []byte(settings)},
			{Path: "lib/build.gradle.kts", Content: []byte(`repositories { maven { url = uri("https://lib.example.com/maven") } }`)},
		}}
	}
	r := Analyse(files("FROM gradle:8-jdk21\nCOPY . .\nRUN gradle build\n"))
	got := destinations(r.Dockerfiles[0].NetworkNeeds)
	for dest, path := range map[string]string{
		"maven.example.com:443":   "build.gradle",
		"nested.example.com:443":  "build.gradle",
		"plugins.example.net:443": "settings.gradle.kts",
		"lib.example.com:443":     "lib/build.gradle.kts",
	} {
		if n, ok := got[dest]; !ok || n.Path != path || n.Kind != "registry" {
			t.Errorf("%s: need = %+v; want a registry need from %s", dest, n, path)
		}
	}
	for _, bad := range []string{"docs.example.org:443", "commented.example.com:443"} {
		if _, ok := got[bad]; ok {
			t.Errorf("%s is not a declared repository", bad)
		}
	}
	offline := Analyse(files("FROM gradle:8-jdk21\nCOPY . .\nRUN gradle --offline build\n"))
	if _, ok := destinations(offline.Dockerfiles[0].NetworkNeeds)["maven.example.com:443"]; ok {
		t.Error("an offline Gradle build needs no repository")
	}
}
