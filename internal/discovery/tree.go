package discovery

import (
	"path"
	"sort"
	"strings"
)

// Tree is the read-only view detectors receive.
type Tree struct {
	files    map[string]InputFile
	dirs     map[string][]string
	approved []string
}

func NewTree(in Input) *Tree {
	t := &Tree{files: map[string]InputFile{}, dirs: map[string][]string{}, approved: in.ApprovedFrontends}
	for _, f := range in.Files {
		t.files[f.Path] = f
		d := path.Dir(f.Path)
		t.dirs[d] = append(t.dirs[d], path.Base(f.Path))
	}
	for d := range t.dirs {
		sort.Strings(t.dirs[d])
	}
	return t
}

func (t *Tree) Has(p string) bool { _, ok := t.files[p]; return ok }

// Read returns content the controller supplied; absent or unsent files return false.
func (t *Tree) Read(p string) ([]byte, bool) {
	f, ok := t.files[p]
	if !ok || f.Content == nil {
		return nil, false
	}
	return f.Content, true
}

// Dirs returns directories containing a file whose base name satisfies match, sorted.
func (t *Tree) Dirs(match func(name string) bool) []string {
	var out []string
	for d, names := range t.dirs {
		for _, n := range names {
			if match(n) {
				out = append(out, d)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Names lists base names in dir matching match.
func (t *Tree) Names(dir string, match func(name string) bool) []string {
	var out []string
	for _, n := range t.dirs[dir] {
		if match(n) {
			out = append(out, n)
		}
	}
	return out
}

// Under lists files below dir (recursively) matching match.
func (t *Tree) Under(dir string, match func(p string) bool) []string {
	var out []string
	for p := range t.files {
		if (dir == "." || strings.HasPrefix(p, dir+"/")) && match(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func join(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

func ext(exts ...string) func(string) bool {
	return func(n string) bool {
		l := strings.ToLower(n)
		for _, e := range exts {
			if strings.HasSuffix(l, e) {
				return true
			}
		}
		return false
	}
}

func named(names ...string) func(string) bool {
	return func(n string) bool {
		for _, x := range names {
			if n == x {
				return true
			}
		}
		return false
	}
}

var wantedNames = map[string]bool{
	"package.json": true, "tsconfig.json": true, ".nvmrc": true, ".node-version": true, ".npmrc": true,
	"pyproject.toml": true, "Pipfile": true, "setup.cfg": true, "setup.py": true, ".python-version": true, "runtime.txt": true, "Procfile": true,
	"global.json": true, "pip.conf": true, "uv.toml": true, ".yarnrc.yml": true,
	"build.gradle": true, "build.gradle.kts": true, "settings.gradle": true, "settings.gradle.kts": true,
}

// pythonEntries are conventional Python entry modules, read as text so discovery can name the ASGI or WSGI
// application a server starts; they are never imported or run.
var pythonEntries = map[string]bool{"main.py": true, "app.py": true, "asgi.py": true, "wsgi.py": true, "server.py": true}

var lockfileNames = map[string]bool{"package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true}

const (
	maxEntryContent    = 256 << 10
	maxFileContent     = 1 << 20
	maxLockfileContent = 8 << 20
)

// MaxContent is the largest size of a file whose content detectors read, or -1 for files listed by path and size
// only.
func MaxContent(p string) int64 {
	name := path.Base(p)
	l := strings.ToLower(name)
	switch {
	case lockfileNames[name]:
		return maxLockfileContent
	case pythonEntries[name]:
		return maxEntryContent
	case wantedNames[name],
		strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt"),
		strings.HasSuffix(l, ".ps1"), strings.HasSuffix(l, ".psm1"), strings.HasSuffix(l, ".psd1"),
		strings.HasSuffix(l, ".csproj"), strings.HasSuffix(l, ".fsproj"), strings.HasSuffix(l, ".vbproj"),
		isDockerfile(name):
		return maxFileContent
	}
	return -1
}
