package discovery

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type pyproject struct {
	BuildSystem struct {
		Backend string `toml:"build-backend"`
	} `toml:"build-system"`
	Project struct {
		Name           string            `toml:"name"`
		RequiresPython string            `toml:"requires-python"`
		Dependencies   []string          `toml:"dependencies"`
		Scripts        map[string]string `toml:"scripts"`
	} `toml:"project"`
	Tool struct {
		Poetry struct {
			Dependencies map[string]any    `toml:"dependencies"`
			Scripts      map[string]string `toml:"scripts"`
		} `toml:"poetry"`
	} `toml:"tool"`
}

var pyManifests = named("pyproject.toml", "requirements.txt", "Pipfile", "setup.py", "setup.cfg")

var pyFrameworks = []struct{ dep, name, kind string }{
	{"django", "Django", "http-service"}, {"fastapi", "FastAPI", "http-service"}, {"flask", "Flask", "http-service"},
	{"starlette", "Starlette", "http-service"}, {"aiohttp", "aiohttp", "http-service"}, {"tornado", "Tornado", "http-service"},
	{"sanic", "Sanic", "http-service"}, {"streamlit", "Streamlit", "http-service"}, {"celery", "Celery", "worker"},
	{"rq", "RQ", "worker"}, {"dramatiq", "Dramatiq", "worker"},
}

var reqName = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)`)

func detectPython(t *Tree) []Finding {
	var out []Finding
	for _, dir := range t.Dirs(pyManifests) {
		out = append(out, pythonFinding(t, dir))
	}
	return out
}

func pythonFinding(t *Tree, dir string) Finding {
	f := newFinding("python", "1.0.0", dir)
	f.Language = "python"
	deps := map[string]bool{}
	var scripts map[string]string

	if raw, ok := t.Read(join(dir, "pyproject.toml")); ok {
		p := join(dir, "pyproject.toml")
		var pp pyproject
		if err := toml.Unmarshal(raw, &pp); err != nil {
			f.add(Weak, "manifest", p, "", "pyproject.toml exists but could not be parsed")
			f.warn("manifest-unreadable", "pyproject.toml is not valid TOML or is too large to inspect.", p)
		} else {
			if pp.BuildSystem.Backend != "" {
				f.add(Strong, "manifest", p, "build-system.build-backend", "build backend "+pp.BuildSystem.Backend)
			} else {
				f.add(Moderate, "manifest", p, "", "pyproject.toml present")
			}
			if v := pp.Project.RequiresPython; v != "" {
				f.RuntimeConstraints["python"] = v
				f.add(Strong, "manifest", p, "project.requires-python", "requires Python "+v)
			}
			for _, d := range pp.Project.Dependencies {
				if m := reqName.FindStringSubmatch(d); m != nil {
					deps[normalizePy(m[1])] = true
				}
			}
			for d := range pp.Tool.Poetry.Dependencies {
				deps[normalizePy(d)] = true
			}
			scripts = pp.Project.Scripts
			if len(scripts) == 0 {
				scripts = pp.Tool.Poetry.Scripts
			}
			if len(pp.Tool.Poetry.Dependencies) > 0 {
				f.PackageManager = "poetry"
			}
		}
	}
	for _, req := range t.Names(dir, func(n string) bool { return strings.HasPrefix(n, "requirements") && strings.HasSuffix(n, ".txt") }) {
		p := join(dir, req)
		f.add(Moderate, "manifest", p, "", req+" lists dependencies")
		if raw, ok := t.Read(p); ok {
			sc := bufio.NewScanner(bytes.NewReader(raw))
			for sc.Scan() {
				line := sc.Text()
				if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.HasPrefix(strings.TrimSpace(line), "-") {
					continue
				}
				if m := reqName.FindStringSubmatch(line); m != nil {
					deps[normalizePy(m[1])] = true
				}
			}
		}
		if f.PackageManager == "" {
			f.PackageManager = "pip"
		}
	}
	if t.Has(join(dir, "setup.py")) {
		f.add(Weak, "manifest", join(dir, "setup.py"), "", "setup.py present (read as text only, never run)")
	}
	var found []string
	for _, l := range [][2]string{{"poetry.lock", "poetry"}, {"uv.lock", "uv"}, {"Pipfile.lock", "pipenv"}, {"pdm.lock", "pdm"}} {
		file, pm := l[0], l[1]
		if t.Has(join(dir, file)) {
			found = append(found, pm)
			f.add(Strong, "lockfile", join(dir, file), "", file+" pins dependencies")
			f.PackageManager = pm
		}
	}
	if len(found) > 1 {
		f.add(Contradicting, "lockfile", dir, "", "lockfiles for "+strings.Join(found, " and "))
	}
	if t.Has(join(dir, "Pipfile")) && f.PackageManager == "" {
		f.PackageManager = "pipenv"
		f.add(Moderate, "manifest", join(dir, "Pipfile"), "", "Pipfile present")
	}
	for _, n := range []string{".python-version", "runtime.txt"} {
		if raw, ok := t.Read(join(dir, n)); ok {
			v := strings.TrimPrefix(strings.TrimSpace(string(raw)), "python-")
			if v != "" && len(v) < 40 {
				if _, set := f.RuntimeConstraints["python"]; !set {
					f.RuntimeConstraints["python"] = v
				}
				f.add(Moderate, "version-file", join(dir, n), "", "pins Python "+v)
			}
		}
	}

	kinds := map[string]bool{}
	for _, fw := range pyFrameworks {
		if deps[fw.dep] {
			if f.Framework == "" {
				f.Framework = fw.name
			}
			kinds[fw.kind] = true
			f.add(Moderate, "dependency", dir, "", fw.name+" is a dependency")
		}
	}
	if t.Has(join(dir, "manage.py")) {
		f.Framework = "Django"
		kinds["http-service"] = true
		f.add(Moderate, "file", join(dir, "manage.py"), "", "Django manage.py present")
	}
	if len(scripts) > 0 {
		kinds["cli-task"] = true
		f.add(Moderate, "manifest", join(dir, "pyproject.toml"), "project.scripts", "declares console scripts")
	}
	f.ApplicationKind = pickKind(f, kinds, dir)

	switch f.PackageManager {
	case "poetry":
		f.SuggestedCommands["install"] = "poetry install --no-root --only main"
	case "uv":
		f.SuggestedCommands["install"] = "uv sync --frozen --no-dev"
	case "pipenv":
		f.SuggestedCommands["install"] = "pipenv install --deploy"
	case "pdm":
		f.SuggestedCommands["install"] = "pdm sync --prod"
	default:
		if t.Has(join(dir, "requirements.txt")) {
			f.SuggestedCommands["install"] = "pip install -r requirements.txt"
		} else {
			f.SuggestedCommands["install"] = "pip install ."
		}
	}
	if start, ok := procfileWeb(t, dir); ok {
		f.SuggestedCommands["start"] = start
		f.add(Strong, "procfile", join(dir, "Procfile"), "web", "Procfile declares the web process")
	} else if start, file, ok := pythonServer(t, dir, deps); ok {
		f.SuggestedCommands["start"] = start
		f.add(Moderate, "source", file, "", "creates the web application that "+strings.Fields(start)[0]+" starts")
	} else if f.ApplicationKind != "" {
		f.warn("start-unknown", "The start command could not be determined from the files. Tell us how the application starts.", dir)
	}
	return f.done()
}

func normalizePy(n string) string { return strings.ReplaceAll(strings.ToLower(n), "_", "-") }

func procfileWeb(t *Tree, dir string) (string, bool) {
	raw, ok := t.Read(join(dir, "Procfile"))
	if !ok {
		return "", false
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "web" {
			return strings.TrimSpace(v), strings.TrimSpace(v) != ""
		}
	}
	return "", false
}

var pythonAppRE = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)\s*(?::[^=\n]*)?=\s*(?:fastapi\.)?(FastAPI|Starlette|Flask)\(`)

// pythonServer finds the module-level application object in a conventional entry module and the server the
// project depends on to run it, listening on port 8000.
func pythonServer(t *Tree, dir string, deps map[string]bool) (string, string, bool) {
	for _, rel := range []string{"main.py", "app.py", "asgi.py", "wsgi.py", "server.py", "app/main.py", "src/main.py", "app/app.py"} {
		file := join(dir, rel)
		raw, ok := t.Read(file)
		if !ok {
			continue
		}
		m := pythonAppRE.FindSubmatch(raw)
		if m == nil {
			continue
		}
		target := strings.ReplaceAll(strings.TrimSuffix(rel, ".py"), "/", ".") + ":" + string(m[1])
		switch {
		case string(m[2]) != "Flask" && deps["uvicorn"]:
			return "uvicorn " + target + " --host 0.0.0.0 --port 8000", file, true
		case string(m[2]) == "Flask" && deps["gunicorn"]:
			return "gunicorn --bind 0.0.0.0:8000 " + target, file, true
		}
		return "", "", false
	}
	return "", "", false
}
