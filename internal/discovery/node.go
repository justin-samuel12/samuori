package discovery

import (
	"encoding/json"
	"sort"
	"strings"
)

type packageJSON struct {
	Name           string            `json:"name"`
	Main           string            `json:"main"`
	Bin            json.RawMessage   `json:"bin"`
	Scripts        map[string]string `json:"scripts"`
	Dependencies   map[string]string `json:"dependencies"`
	DevDeps        map[string]string `json:"devDependencies"`
	Engines        map[string]string `json:"engines"`
	PackageManager string            `json:"packageManager"`
	Workspaces     json.RawMessage   `json:"workspaces"`
	Type           string            `json:"type"`
}

var nodeLockfiles = []struct{ file, manager string }{
	{"package-lock.json", "npm"}, {"npm-shrinkwrap.json", "npm"}, {"yarn.lock", "yarn"},
	{"pnpm-lock.yaml", "pnpm"}, {"bun.lock", "bun"}, {"bun.lockb", "bun"},
}

var nodeHTTPFrameworks = []struct{ dep, name string }{
	{"next", "Next.js"}, {"nuxt", "Nuxt"}, {"@nestjs/core", "NestJS"}, {"@remix-run/node", "Remix"},
	{"@sveltejs/kit", "SvelteKit"}, {"express", "Express"}, {"fastify", "Fastify"}, {"koa", "Koa"},
	{"@hapi/hapi", "hapi"}, {"hono", "Hono"}, {"@adonisjs/core", "AdonisJS"},
}

var nodeStaticTools = []struct{ dep, name string }{
	{"vite", "Vite"}, {"react-scripts", "Create React App"}, {"@angular/core", "Angular"}, {"astro", "Astro"},
	{"gatsby", "Gatsby"}, {"@vue/cli-service", "Vue CLI"},
}

var nodeWorkerLibs = []string{"bullmq", "bull", "agenda", "bee-queue", "kafkajs", "amqplib", "node-cron"}

func detectNode(t *Tree) []Finding {
	var out []Finding
	for _, dir := range t.Dirs(named("package.json")) {
		out = append(out, nodeFinding(t, dir))
	}
	return out
}

func nodeFinding(t *Tree, dir string) Finding {
	f := newFinding("node", "1.0.0", dir)
	f.Language = "javascript"
	manifest := join(dir, "package.json")
	raw, ok := t.Read(manifest)
	var pkg packageJSON
	if !ok || json.Unmarshal(raw, &pkg) != nil {
		f.add(Weak, "manifest", manifest, "", "package.json exists but could not be read")
		f.warn("manifest-unreadable", "package.json is not valid JSON or is too large to inspect.", manifest)
		return f.done()
	}

	var managers []string
	for _, l := range nodeLockfiles {
		if t.Has(join(dir, l.file)) {
			managers = append(managers, l.manager)
			f.add(Strong, "lockfile", join(dir, l.file), "", l.file+" matches package.json")
		}
	}
	managers = unique(managers)
	switch {
	case len(managers) == 0:
		f.add(Moderate, "manifest", manifest, "", "package.json without a lockfile")
		f.warn("no-lockfile", "No lockfile was found, so dependency versions are not pinned.", manifest)
		f.PackageManager = "npm"
	case len(managers) == 1:
		f.PackageManager = managers[0]
	default:
		f.add(Contradicting, "lockfile", dir, "", "lockfiles for "+strings.Join(managers, " and "))
		f.PackageManager = managers[0]
	}
	if pm, _, _ := strings.Cut(pkg.PackageManager, "@"); pm != "" {
		f.add(Moderate, "manifest", manifest, "/packageManager", "packageManager field names "+pm)
		if len(managers) > 0 && pm != f.PackageManager {
			f.add(Contradicting, "manifest", manifest, "/packageManager", "packageManager disagrees with the lockfile")
		}
		f.PackageManager = pm
	}
	if v := pkg.Engines["node"]; v != "" {
		f.RuntimeConstraints["node"] = v
		f.add(Strong, "manifest", manifest, "/engines/node", "requires Node.js "+v)
	}
	for _, n := range []string{".nvmrc", ".node-version"} {
		if v, ok := t.Read(join(dir, n)); ok {
			ver := strings.TrimSpace(string(v))
			if len(ver) > 0 && len(ver) < 40 {
				if _, set := f.RuntimeConstraints["node"]; !set {
					f.RuntimeConstraints["node"] = ver
				}
				f.add(Moderate, "version-file", join(dir, n), "", "pins Node.js "+ver)
			}
		}
	}
	// The typescript package alone is often a tool dependency of a JavaScript app; tsconfig.json or a TypeScript
	// entry point decides the language.
	switch {
	case t.Has(join(dir, "tsconfig.json")) || strings.HasSuffix(pkg.Main, ".ts"):
		f.Language = "typescript"
		f.add(Moderate, "manifest", manifest, "", "TypeScript is configured")
	case has(pkg.DevDeps, "typescript") || has(pkg.Dependencies, "typescript"):
		f.add(Weak, "dependency", manifest, "", "the typescript package is installed but not configured; treated as JavaScript")
	}

	deps := merge(pkg.Dependencies, nil)
	if len(pkg.Workspaces) > 0 && string(pkg.Workspaces) != "null" {
		f.warn("workspace-root", "This is a workspace root; choose the package to build from the other findings.", manifest)
		f.add(Moderate, "manifest", manifest, "/workspaces", "declares workspaces")
	}
	kinds := map[string]bool{}
	for _, fw := range nodeHTTPFrameworks {
		if has(deps, fw.dep) {
			if f.Framework == "" {
				f.Framework = fw.name
			}
			kinds["http-service"] = true
			f.add(Moderate, "dependency", manifest, "/dependencies/"+fw.dep, fw.name+" is a dependency")
		}
	}
	if f.Framework == "" {
		for _, st := range nodeStaticTools {
			if has(deps, st.dep) || has(pkg.DevDeps, st.dep) {
				f.Framework = st.name
				kinds["static-site"] = true
				f.add(Moderate, "dependency", manifest, "", st.name+" builds front-end assets")
				break
			}
		}
	}
	for _, w := range nodeWorkerLibs {
		if has(deps, w) {
			kinds["worker"] = true
			f.add(Weak, "dependency", manifest, "/dependencies/"+w, w+" suggests background processing")
		}
	}
	if len(pkg.Bin) > 0 && string(pkg.Bin) != "null" {
		kinds["cli-task"] = true
		f.add(Moderate, "manifest", manifest, "/bin", "declares command-line executables")
	}
	f.ApplicationKind = pickKind(f, kinds, manifest)

	run := map[string]string{"npm": "npm run ", "yarn": "yarn ", "pnpm": "pnpm run ", "bun": "bun run "}[f.PackageManager]
	frozen := map[string]string{"npm": "npm ci", "yarn": "yarn install --frozen-lockfile", "pnpm": "pnpm install --frozen-lockfile", "bun": "bun install --frozen-lockfile"}[f.PackageManager]
	if len(managers) == 0 {
		frozen = f.PackageManager + " install"
	}
	f.SuggestedCommands["install"] = frozen
	if s := pkg.Scripts["build"]; s != "" {
		f.SuggestedCommands["build"] = run + "build"
		f.add(Moderate, "script", manifest, "/scripts/build", "build script defined")
	}
	if s := pkg.Scripts["test"]; s != "" && !strings.Contains(s, "no test specified") {
		f.SuggestedCommands["test"] = run + "test"
	}
	switch {
	case pkg.Scripts["start"] != "":
		f.SuggestedCommands["start"] = run + "start"
		f.add(Moderate, "script", manifest, "/scripts/start", "start script defined")
	case pkg.Main != "" && f.ApplicationKind != "static-site":
		f.SuggestedCommands["start"] = "node " + pkg.Main
		f.add(Weak, "manifest", manifest, "/main", "main entry point "+pkg.Main)
		f.warn("start-inferred", "No start script; the start command was inferred from the main field. Confirm it.", manifest)
	case f.ApplicationKind != "static-site":
		f.warn("start-unknown", "No start command was found. Tell us how the application starts.", manifest)
	}
	return f.done()
}

// pickKind never assumes HTTP: one candidate is used, several are flagged for the user to choose.
func pickKind(f *finding, kinds map[string]bool, p string) string {
	switch len(kinds) {
	case 0:
		f.warn("kind-unknown", "Could not tell whether this is a web service, background worker, command-line tool or static site.", p)
		return ""
	case 1:
		for k := range kinds {
			return k
		}
	}
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	f.warn("kind-ambiguous", "Evidence points to more than one kind of application ("+strings.Join(names, ", ")+"); choose one.", p)
	if kinds["http-service"] {
		return "http-service"
	}
	return names[0]
}

func has(m map[string]string, k string) bool { _, ok := m[k]; return ok }

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func unique(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
