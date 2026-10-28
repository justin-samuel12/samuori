package discovery

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const sharpDockerfile = `FROM node:18-alpine
WORKDIR /app
COPY package*.json ./
RUN npm ci --only=production=false
COPY . .
EXPOSE 3000
CMD ["node", "server.js"]
`

func sharpLock(version string) string {
	return `{"name":"shop","lockfileVersion":3,"packages":{"":{"name":"shop"},` +
		`"node_modules/express":{"version":"4.21.2","resolved":"https://registry.npmjs.org/express/-/express-4.21.2.tgz"},` +
		`"node_modules/sharp":{"version":"` + version + `","resolved":"https://registry.npmjs.org/sharp/-/sharp-` + version + `.tgz","hasInstallScript":true}}}`
}

func sharpProject(version string) Input {
	pkg := `{"name":"shop","scripts":{"start":"node server.js"},"engines":{"node":">=20"},"dependencies":{"express":"^4.21.0","sharp":"^` + version + `"}}`
	return Input{Files: []InputFile{
		{Path: "Dockerfile", Content: []byte(sharpDockerfile)},
		{Path: "package.json", Content: []byte(pkg)},
		{Path: "package-lock.json", Content: []byte(sharpLock(version))},
		{Path: "server.js"},
	}}
}

func needFor(needs []NetworkNeed, match func(NetworkNeed) bool) *NetworkNeed {
	for i := range needs {
		if match(needs[i]) {
			return &needs[i]
		}
	}
	return nil
}

func codes(rs []Readiness) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Code)
	}
	return out
}

func TestSharpBeforeZeroThirtyThreeNeedsGitHub(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) }
	defer func() { now = time.Now }()
	r := Analyse(sharpProject("0.32.6"))
	if len(r.Dockerfiles) != 1 {
		t.Fatalf("dockerfiles = %+v", r.Dockerfiles)
	}
	df := r.Dockerfiles[0]
	npm := needFor(df.NetworkNeeds, func(n NetworkNeed) bool { return n.Preset == "npm" })
	if npm == nil || npm.Line != 4 || npm.Confidence != Certain || npm.Path != "Dockerfile" {
		t.Errorf("npm need = %+v; want the npm preset at line 4", npm)
	}
	sharp := needFor(df.NetworkNeeds, func(n NetworkNeed) bool { return n.Kind == "downloader" })
	if sharp == nil || !slices.Contains(sharp.Destinations, "github.com:443") || !slices.Contains(sharp.Destinations, "objects.githubusercontent.com:443") ||
		!slices.Contains(sharp.Destinations, "release-assets.githubusercontent.com:443") || sharp.Path != "package-lock.json" ||
		!strings.Contains(sharp.Cause, "sharp 0.32.6") || !strings.Contains(sharp.Fix, "0.33") || sharp.Severity != WillFail {
		t.Errorf("sharp need = %+v", sharp)
	}
	got := codes(df.Readiness)
	for _, want := range []string{"npm-only-flag", "runtime-end-of-life", "runtime-version-mismatch"} {
		if !slices.Contains(got, want) {
			t.Errorf("readiness %v lacks %s", got, want)
		}
	}
	if slices.Contains(got, "start-file-missing") {
		t.Errorf("server.js is uploaded, but readiness reports it missing: %+v", df.Readiness)
	}
	for _, it := range df.Readiness {
		if it.Code == "npm-only-flag" && !strings.Contains(it.Fix, "--include=dev") {
			t.Errorf("--only=production=false advice = %q; want --include=dev", it.Fix)
		}
	}
}

func TestSharpZeroThirtyThreeNeedsOnlyNpm(t *testing.T) {
	r := Analyse(sharpProject("0.33.5"))
	df := r.Dockerfiles[0]
	if n := needFor(df.NetworkNeeds, func(n NetworkNeed) bool { return n.Kind == "downloader" }); n != nil {
		t.Errorf("sharp 0.33.5 predicted a download: %+v", n)
	}
	if needFor(df.NetworkNeeds, func(n NetworkNeed) bool { return n.Preset == "npm" }) == nil {
		t.Error("npm ci needs the npm preset")
	}
}

func TestInstallSkipsDownloaders(t *testing.T) {
	for name, df := range map[string]string{
		"ignore scripts": "FROM node:22\nRUN npm ci --ignore-scripts\n",
		"no network":     "FROM node:22\nRUN --network=none npm ci\n",
	} {
		in := sharpProject("0.32.6")
		in.Files[0].Content = []byte(df)
		if n := needFor(Analyse(in).Dockerfiles[0].NetworkNeeds, func(n NetworkNeed) bool { return n.Kind == "downloader" }); n != nil {
			t.Errorf("%s: %+v", name, n)
		}
	}
	in := sharpProject("0.32.6")
	in.Files[0].Content = []byte("FROM node:22\nENV PUPPETEER_SKIP_DOWNLOAD=true\nRUN npm ci\n")
	in.Files[2].Content = []byte(`{"lockfileVersion":3,"packages":{"node_modules/puppeteer":{"version":"23.0.0"}}}`)
	if n := needFor(Analyse(in).Dockerfiles[0].NetworkNeeds, func(n NetworkNeed) bool { return n.Kind == "downloader" }); n != nil {
		t.Errorf("PUPPETEER_SKIP_DOWNLOAD set, yet %+v", n)
	}
}

func TestLiteralURLsAndPackageManagers(t *testing.T) {
	df := `FROM debian:bookworm-slim AS base
RUN apt-get update && apt-get install -y curl \
    && curl -fsSL https://example.org/x -o /x \
    && wget "http://downloads.example.net:8080/tool.tgz"
RUN curl https://$HOST/y
FROM python:3.12-slim
RUN pip install --no-cache-dir -r requirements.txt
RUN pip install --index-url https://pypi.internal.example/simple flask
FROM ubuntu:24.04
RUN apt-get update
RUN dnf install -y gcc
`
	r := Analyse(Input{Files: []InputFile{{Path: "Dockerfile", Content: []byte(df)}}})
	a := r.Dockerfiles[0]
	want := map[string]int{"example.org:443": 2, "downloads.example.net:8080": 2, "pypi.internal.example:443": 8}
	for dest, ln := range want {
		if n := needFor(a.NetworkNeeds, func(n NetworkNeed) bool { return slices.Contains(n.Destinations, dest) }); n == nil || n.Line != ln {
			t.Errorf("%s: need = %+v; want line %d", dest, n, ln)
		}
	}
	for preset, ln := range map[string]int{"debian": 2, "pypi": 7, "ubuntu": 10} {
		if n := needFor(a.NetworkNeeds, func(n NetworkNeed) bool { return n.Preset == preset }); n == nil || n.Line != ln {
			t.Errorf("preset %s: need = %+v; want line %d", preset, n, ln)
		}
	}
	for _, n := range a.NetworkNeeds {
		for _, d := range n.Destinations {
			if strings.Contains(d, "$") || strings.Contains(d, "HOST") {
				t.Errorf("a URL with a variable became a destination: %+v", n)
			}
		}
	}
	if !slices.Contains(codes(a.Readiness), "rpm-mirrors") {
		t.Errorf("dnf install: readiness %v", codes(a.Readiness))
	}
}

func TestStartFileMissing(t *testing.T) {
	in := Input{Files: []InputFile{
		{Path: "Dockerfile", Content: []byte("FROM node:22-alpine\nCOPY . .\nCMD [\"node\", \"index.js\"]\n")},
		{Path: "server.js"},
	}}
	if !slices.Contains(codes(Analyse(in).Dockerfiles[0].Readiness), "start-file-missing") {
		t.Error("index.js is not in the upload, but no readiness item says so")
	}
	in.Files[1].Path = "src/index.ts"
	if slices.Contains(codes(Analyse(in).Dockerfiles[0].Readiness), "start-file-missing") {
		t.Error("index.js compiled from src/index.ts was reported missing")
	}
}

func TestPostinstallScriptNeeds(t *testing.T) {
	pkg := `{"name":"api","scripts":{"postinstall":"prisma generate","build":"curl -fsSL https://assets.example.com/a.tgz | tar xz && tsc"},"dependencies":{"express":"^4"}}`
	in := Input{Files: []InputFile{
		{Path: "Dockerfile", Content: []byte("FROM node:22\nRUN npm install\nRUN npm run build\n")},
		{Path: "package.json", Content: []byte(pkg)},
	}}
	a := Analyse(in).Dockerfiles[0]
	if n := needFor(a.NetworkNeeds, func(n NetworkNeed) bool { return slices.Contains(n.Destinations, "binaries.prisma.sh:443") }); n == nil || !strings.Contains(n.Cause, "postinstall") {
		t.Errorf("prisma generate in postinstall: %+v", n)
	}
	if n := needFor(a.NetworkNeeds, func(n NetworkNeed) bool { return slices.Contains(n.Destinations, "assets.example.com:443") }); n == nil || !strings.Contains(n.Cause, "line 3") {
		t.Errorf("curl in the build script run at line 3: %+v", n)
	}
}

func TestVersionRanges(t *testing.T) {
	for _, c := range []struct {
		v, rng string
		want   bool
	}{
		{"0.32.6", "<0.33.0", true}, {"0.33.0", "<0.33.0", false}, {"18.20.1", ">=20", false}, {"20.1.0", ">=18 <21", true},
		{"0.32.9", "^0.32.6", true}, {"0.33.0", "^0.32.6", false}, {"3.9.1", ">=3.8,<4", true}, {"3.12.0", "~=3.8", true},
		{"5.1.0", "4.x || 5.x", true}, {"6.0.0", "<6.0.0", false},
	} {
		v, _, _ := parseVersion(c.v)
		if got, ok := satisfies(v, c.rng); !ok || got != c.want {
			t.Errorf("satisfies(%s, %q) = %v, %v; want %v", c.v, c.rng, got, ok, c.want)
		}
	}
	v, n, _ := parseVersion("18")
	if ok, _ := admitsSeries(v, n, ">=18.17"); !ok {
		t.Error("node 18 admits >=18.17")
	}
	if ok, _ := admitsSeries(v, n, ">=20"); ok {
		t.Error("node 18 does not admit >=20")
	}
}

// Readiness prediction must stay static: the package may not import anything that runs processes, opens files or
// reaches the network (ADR 0024, ADR 0050).
func TestPredictionStaysStatic(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "os" || p == "os/exec" || p == "net" || p == "net/http" || p == "syscall" || strings.HasPrefix(p, "golang.org/x/sys") {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
	if _, err := os.Stat("downloaders-1.0.0.json"); err != nil {
		t.Fatal(err)
	}
	for _, d := range downloaders.Entries {
		if d.Source == "" || len(d.Destinations) == 0 || (d.Severity != WillFail && d.Severity != MayFail) || d.Fix == "" {
			t.Errorf("catalog entry %s%v is incomplete", d.Package, d.Command)
		}
	}
}

func TestLockfileSkipsPackagesForOtherSystems(t *testing.T) {
	for _, c := range []struct {
		os   []string
		want bool
	}{{nil, true}, {[]string{"darwin"}, false}, {[]string{"linux"}, true}, {[]string{"!win32"}, true}, {[]string{"!linux"}, false}, {[]string{"darwin", "linux"}, true}} {
		if got := installsOnLinux(c.os); got != c.want {
			t.Errorf("installsOnLinux(%v) = %v, want %v", c.os, got, c.want)
		}
	}
	lock := `{"lockfileVersion":3,"packages":{"":{},"node_modules/fsevents":{"version":"2.3.3","hasInstallScript":true,"optional":true,"os":["darwin"]},"node_modules/esbuild":{"version":"0.20.0","hasInstallScript":true}}}`
	pkgs, ok := npmLock([]byte(lock))
	if !ok || len(pkgs) != 1 || pkgs[0].name != "esbuild" {
		t.Errorf("packages = %+v; fsevents installs only on macOS and must be skipped", pkgs)
	}
	if scriptCount(1) != "1 installed package runs an install script" || scriptCount(2) != "2 installed packages run install scripts" {
		t.Error("install script count wording")
	}
}
