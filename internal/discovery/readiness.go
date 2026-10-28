package discovery

import (
	"encoding/json"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
)

// Build readiness predicts, statically, what a build will need and where it is likely to break (ADR 0050,
// docs/architecture/discovery.md#build-readiness). Nothing here runs a command or contacts the network.

const maxPredictions = 100

type nodeInstall struct {
	line          int
	cause         string
	env           map[string]string
	omitDev       bool
	ignoreScripts bool
}

type scriptRun struct {
	line   int
	script string
	env    map[string]string
}

type stageImage struct {
	image string
	line  int
}

type dockerfileSteps struct {
	nodeInstalls []nodeInstall
	scriptRuns   []scriptRun
	pipLines     []int
	gradleLines  []int
	images       []stageImage
	start        []string
	startLine    int
}

// stage tracks what a RUN step inherits: the stage's base image and its ENV values.
type stage struct {
	image string
	env   map[string]string
}

func stageLine(s instructions.Stage) int {
	if len(s.Location) > 0 {
		return s.Location[0].Start.Line
	}
	return 0
}

// setStart keeps the last CMD or ENTRYPOINT of the final stage that runs a known interpreter.
func (s *dockerfileSteps) setStart(c instructions.ShellDependantCmdLine, ln int) {
	argv := c.CmdLine
	if c.PrependShell {
		if cmds := shellCommands(strings.Join(argv, " ")); len(cmds) > 0 {
			argv, _ = simple(cmds[0])
		}
	}
	if len(argv) > 1 && startPrograms[path.Base(argv[0])] {
		s.start, s.startLine = argv, ln
	}
}

func line(c instructions.Command) int {
	if loc := c.Location(); len(loc) > 0 {
		return loc[0].Start.Line
	}
	return 0
}

// inspectRun records the predictions for one RUN instruction.
func (a *DockerfileAnalysis) inspectRun(s *stage, cmd *instructions.RunCommand) {
	if instructions.GetNetwork(cmd) == instructions.NetworkNone {
		return
	}
	ln := line(cmd)
	var commands [][]string
	if cmd.PrependShell {
		commands = shellCommands(strings.Join(cmd.CmdLine, " "))
		for _, f := range cmd.Files {
			commands = append(commands, shellCommands(f.Data)...)
		}
	} else if len(cmd.CmdLine) > 0 {
		commands = [][]string{cmd.CmdLine}
	}
	for _, c := range commands {
		args, own := simple(c)
		env := mergeEnv(s.env, own)
		st := classify(args, env, s.image)
		if st.npmRegistry != "" && !st.nodeInstall {
			s.env["npm_config_registry"] = st.npmRegistry
		}
		a.record(st, ln, env, quote(args))
	}
}

func (a *DockerfileAnalysis) record(st step, ln int, env map[string]string, cause string) {
	for _, n := range st.needs {
		n.Path, n.Line = a.Path, ln
		a.addNeed(n)
	}
	for _, t := range st.triggers {
		for _, n := range commandDownloaders(t, quote(t)) {
			if !skipped(n, env) {
				n.Path, n.Line = a.Path, ln
				a.addNeed(n)
			}
		}
	}
	for _, r := range st.readiness {
		r.Path, r.Line = a.Path, ln
		a.addReadiness(r)
	}
	if st.nodeInstall {
		a.steps.nodeInstalls = append(a.steps.nodeInstalls, nodeInstall{line: ln, cause: cause, env: env, omitDev: st.omitDev, ignoreScripts: st.ignoreScripts})
	}
	if st.script != "" {
		a.steps.scriptRuns = append(a.steps.scriptRuns, scriptRun{line: ln, script: st.script, env: env})
	}
	if st.pipInstall {
		a.steps.pipLines = append(a.steps.pipLines, ln)
	}
	if st.gradle {
		a.steps.gradleLines = append(a.steps.gradleLines, ln)
	}
}

func skipped(n NetworkNeed, env map[string]string) bool {
	for _, k := range n.skipEnv {
		if truthy(env[k]) {
			return true
		}
	}
	return false
}

func needKey(n NetworkNeed) string {
	return n.Kind + "|" + n.Preset + "|" + strings.Join(n.Destinations, ",") + "|" + n.Cause + "|" + n.Path + "|" + itoa(n.Line)
}

func (a *DockerfileAnalysis) addNeed(n NetworkNeed) {
	if len(a.NetworkNeeds) >= maxPredictions || slices.ContainsFunc(a.NetworkNeeds, func(x NetworkNeed) bool { return needKey(x) == needKey(n) }) {
		return
	}
	a.NetworkNeeds = append(a.NetworkNeeds, n)
}

func (a *DockerfileAnalysis) addReadiness(r Readiness) {
	if len(a.Readiness) >= maxPredictions || slices.Contains(a.Readiness, r) {
		return
	}
	a.Readiness = append(a.Readiness, r)
}

// predict adds the manifest- and lockfile-based needs to findings, then links each Dockerfile to the findings in
// its build context: their install-time needs apply when the Dockerfile installs packages.
func predict(t *Tree, res *Result, in Input) {
	for i := range res.Findings {
		f := &res.Findings[i]
		switch f.DetectorID {
		case "node":
			nodePredictions(t, f)
		case "python":
			pythonPredictions(t, f)
		}
	}
	for i := range res.Dockerfiles {
		df := &res.Dockerfiles[i]
		ctx := in.Context
		if ctx == "" {
			ctx = path.Dir(df.Path)
		}
		var exact, below []Finding
		for _, f := range res.Findings {
			switch {
			case f.SourceSubpath == ctx:
				exact = append(exact, f)
			case ctx == "." || strings.HasPrefix(f.SourceSubpath, ctx+"/"):
				below = append(below, f)
			}
		}
		if len(exact) == 0 {
			exact = below
		}
		for _, f := range exact {
			link(df, f)
		}
		gradleRepositoryNeeds(t, ctx, df)
		if r, ok := startFileMissing(t, ctx, df.steps); ok {
			r.Path = df.Path
			df.addReadiness(r)
		}
		sortPredictions(df.NetworkNeeds, df.Readiness)
	}
	for i := range res.Findings {
		sortPredictions(res.Findings[i].NetworkNeeds, res.Findings[i].Readiness)
	}
}

func nodePredictions(t *Tree, f *Finding) {
	manifest := join(f.SourceSubpath, "package.json")
	raw, ok := t.Read(manifest)
	var pkg packageJSON
	if !ok || json.Unmarshal(raw, &pkg) != nil {
		return
	}
	f.scripts = pkg.Scripts
	var pkgs []lockedPackage
	lockPath := ""
	for _, l := range []string{"package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml"} {
		raw, ok := t.Read(join(f.SourceSubpath, l))
		if !ok {
			continue
		}
		switch l {
		case "yarn.lock":
			pkgs = yarnLock(raw)
		case "pnpm-lock.yaml":
			pkgs = pnpmLock(raw)
		default:
			pkgs, ok = npmLock(raw)
		}
		if ok && len(pkgs) > 0 {
			lockPath = join(f.SourceSubpath, l)
			break
		}
	}
	exact := lockPath != ""
	if !exact {
		lockPath = manifest
		pkgs = nil
		for name, rng := range pkg.Dependencies {
			pkgs = append(pkgs, lockedPackage{name: name, version: rng})
		}
		for name, rng := range pkg.DevDeps {
			pkgs = append(pkgs, lockedPackage{name: name, version: rng, dev: true})
		}
		sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].name < pkgs[j].name })
	}
	add := func(n NetworkNeed) {
		if len(f.NetworkNeeds) < maxPredictions && !slices.ContainsFunc(f.NetworkNeeds, func(x NetworkNeed) bool { return needKey(x) == needKey(n) }) {
			f.NetworkNeeds = append(f.NetworkNeeds, n)
		}
	}
	var scripted []string
	registries := map[string]bool{}
	for _, p := range pkgs {
		found := packageDownloaders(p.name, p.version, exact)
		for _, n := range found {
			n.Path, n.trigger, n.DevOnly = lockPath, "install", p.dev
			add(n)
		}
		if p.installScript && len(found) == 0 && !slices.Contains(scripted, p.name) && !inCatalog(p.name) {
			scripted = append(scripted, p.name)
		}
		if d, ok := destination(p.resolved); ok && d != "registry.npmjs.org:443" && d != "registry.yarnpkg.com:443" {
			registries[d] = true
		}
	}
	for _, d := range sortedKeys(registries) {
		n := need("registry", "packages in "+path.Base(lockPath)+" are downloaded from "+d, "", Likely, d)
		n.Path, n.trigger = lockPath, "install"
		add(n)
	}
	for _, file := range []string{".npmrc", ".yarnrc.yml"} {
		raw, ok := t.Read(join(f.SourceSubpath, file))
		if !ok {
			continue
		}
		def, scoped := npmrcRegistries(raw)
		if file == ".yarnrc.yml" {
			def, scoped = yarnrcRegistries(raw)
		}
		for _, r := range append([]string{def}, scoped...) {
			if d, ok := destination(r); ok {
				n := need("registry", file+" sets the registry "+clipTo(r, 120), "", Likely, d)
				n.Path, n.trigger = join(f.SourceSubpath, file), "install"
				if r == def {
					n.trigger = "registry"
				}
				add(n)
			}
		}
	}
	if len(scripted) > 0 {
		names := scripted
		if len(names) > 8 {
			names = append(slices.Clone(names[:8]), "…")
		}
		f.Readiness = append(f.Readiness, Readiness{Code: "install-scripts", Severity: Advice, Fixer: Developer, Confidence: Possible, Path: lockPath,
			Message: scriptCount(len(scripted)) + " (" + strings.Join(names, ", ") + "). Samuori knows of no downloads they make, but an install script can download files from hosts the egress policy does not allow.",
			Fix:     "If a build fails while installing one of them, the build page lists the hosts it tried to reach."})
	}
	for _, s := range []string{"preinstall", "install", "postinstall", "prepare"} {
		for _, n := range scriptNeeds(pkg.Scripts, s, manifest, nil, 0) {
			n.trigger = "install"
			add(n)
		}
	}
	for _, n := range scriptNeeds(pkg.Scripts, "build", manifest, nil, 0) {
		n.trigger = "script:build"
		add(n)
	}
}

func inCatalog(name string) bool {
	return slices.ContainsFunc(downloaders.Entries, func(d downloader) bool { return d.Ecosystem == "npm" && d.Package == name })
}

// scriptNeeds predicts the needs of one package.json script, following npm run to other scripts.
func scriptNeeds(scripts map[string]string, name, manifest string, env map[string]string, depth int) []NetworkNeed {
	body := scripts[name]
	if body == "" || depth > 4 {
		return nil
	}
	var out []NetworkNeed
	for _, c := range shellCommands(body) {
		args, own := simple(c)
		e := mergeEnv(env, own)
		st := classify(args, e, "")
		for _, n := range st.needs {
			if n.Kind == "url" || n.Kind == "registry" {
				n.Path, n.Cause = manifest, "scripts."+name+": "+n.Cause
				out = append(out, n)
			}
		}
		for _, t := range st.triggers {
			for _, n := range commandDownloaders(t, quote(t)) {
				if !skipped(n, e) {
					n.Path, n.Cause = manifest, "scripts."+name+": "+n.Cause
					out = append(out, n)
				}
			}
		}
		if st.script != "" && st.script != name {
			out = append(out, scriptNeeds(scripts, st.script, manifest, e, depth+1)...)
		}
	}
	return out
}

func pythonPredictions(t *Tree, f *Finding) {
	files := append(t.Names(f.SourceSubpath, func(n string) bool { return strings.HasPrefix(n, "requirements") && strings.HasSuffix(n, ".txt") }),
		"pip.conf", "pyproject.toml", "uv.toml")
	for _, n := range files {
		p := join(f.SourceSubpath, n)
		raw, ok := t.Read(p)
		if !ok {
			continue
		}
		var indexes []string
		conf, verb := Likely, " installs from "
		switch n {
		case "pip.conf":
			// pip reads it only where the build installs it as its configuration.
			indexes, conf, verb = pipConfIndexes(raw), Possible, " sets the package index "
		case "pyproject.toml":
			indexes, verb = tomlIndexes(raw, false), " declares the package index "
		case "uv.toml":
			indexes, verb = tomlIndexes(raw, true), " declares the package index "
		default:
			indexes = pipIndexes(raw)
		}
		for _, idx := range indexes {
			if d, ok := destination(idx); ok {
				nd := need("registry", n+verb+clipTo(idx, 120), "", conf, d)
				nd.Path, nd.trigger = p, "pip"
				if len(f.NetworkNeeds) < maxPredictions && !slices.ContainsFunc(f.NetworkNeeds, func(x NetworkNeed) bool { return needKey(x) == needKey(nd) }) {
					f.NetworkNeeds = append(f.NetworkNeeds, nd)
				}
			}
		}
	}
}

// gradleRepositoryNeeds adds the repositories that Gradle scripts in a Dockerfile's context declare, when the
// Dockerfile runs Gradle online.
func gradleRepositoryNeeds(t *Tree, ctx string, df *DockerfileAnalysis) {
	if len(df.steps.gradleLines) == 0 {
		return
	}
	at := " (" + path.Base(df.Path) + " line " + itoa(df.steps.gradleLines[0]) + ")"
	scripts := t.Under(ctx, func(p string) bool { return gradleScript(path.Base(p)) })
	if len(scripts) > 50 {
		scripts = scripts[:50]
	}
	for _, p := range scripts {
		raw, ok := t.Read(p)
		if !ok {
			continue
		}
		for _, u := range gradleRepositories(raw) {
			if d, ok := destination(u); ok {
				n := need("registry", path.Base(p)+" declares the repository "+clipTo(u, 120)+", used by Gradle"+at, "", Likely, d)
				n.Path = p
				df.addNeed(n)
			}
		}
	}
}

// link applies a finding's install-time needs to a Dockerfile that installs its packages, and compares its
// runtime constraints with the Dockerfile's base images.
func link(df *DockerfileAnalysis, f Finding) {
	steps := df.steps
	switch f.DetectorID {
	case "node":
		for _, inst := range steps.nodeInstalls {
			for _, n := range f.NetworkNeeds {
				if n.trigger == "registry" {
					df.NetworkNeeds = slices.DeleteFunc(df.NetworkNeeds, func(x NetworkNeed) bool {
						return x.Kind == "package-manager" && (x.Preset == "npm" || x.Preset == "yarn") && x.Line == inst.line
					})
				}
				if n.trigger != "install" && n.trigger != "registry" {
					continue
				}
				if (n.DevOnly && inst.omitDev) || (inst.ignoreScripts && n.Kind == "downloader") || skipped(n, inst.env) {
					continue
				}
				n.Cause += ", installed by `" + inst.cause + "` (" + path.Base(df.Path) + " line " + itoa(inst.line) + ")"
				df.addNeed(n)
			}
			for _, r := range f.Readiness {
				df.addReadiness(r)
			}
		}
		for _, run := range steps.scriptRuns {
			for _, n := range scriptNeeds(f.scripts, run.script, join(f.SourceSubpath, "package.json"), run.env, 0) {
				n.Cause += " (" + path.Base(df.Path) + " line " + itoa(run.line) + ")"
				df.addNeed(n)
			}
		}
		if c := f.RuntimeConstraints["node"]; c != "" {
			df.constraint(c, "node", "Node.js", "engines.node or .nvmrc")
		}
	case "python":
		if len(steps.pipLines) > 0 {
			for _, n := range f.NetworkNeeds {
				if n.trigger == "pip" {
					df.addNeed(n)
				}
			}
		}
		if c := f.RuntimeConstraints["python"]; c != "" {
			df.constraint(c, "python", "Python", "requires-python or the Python version file")
		}
	}
}

// constraint reports a base image whose tag names a runtime series the project's own constraint excludes.
func (df *DockerfileAnalysis) constraint(c, repo, name, where string) {
	for _, img := range df.steps.images {
		r, tag, ok := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(img.image, "docker.io/"), "library/"), ":")
		if !ok || r != repo {
			continue
		}
		v, n, ok := parseVersion(tag)
		if !ok {
			continue
		}
		if fits, understood := admitsSeries(v, n, c); understood && !fits {
			df.addReadiness(Readiness{Code: "runtime-version-mismatch", Severity: MayFail, Fixer: Developer, Confidence: Likely, Path: df.Path, Line: img.line,
				Message: "The project asks for " + name + " " + clipTo(c, 60) + " (" + where + "), but " + img.image + " provides " + name + " " + tag + ".",
				Fix:     "Use a base image whose version satisfies " + clipTo(c, 60) + ", or change the project's constraint."})
			return
		}
	}
}

var buildOutputDirs = map[string]bool{"dist": true, "build": true, "out": true, ".next": true, "lib": true, "target": true, "bin": true, "publish": true, ".output": true}

var startPrograms = map[string]bool{"node": true, "nodejs": true, "python": true, "python3": true, "bun": true, "ts-node": true, "tsx": true}

// startFileMissing reports a literal start file in the final stage's CMD or ENTRYPOINT that exists nowhere in the
// build context, unless a build step could produce it.
func startFileMissing(t *Tree, ctx string, s dockerfileSteps) (Readiness, bool) {
	argv := s.start
	if len(argv) < 2 {
		return Readiness{}, false
	}
	file := ""
	for _, a := range argv[1:] {
		if !strings.HasPrefix(a, "-") {
			file = a
			break
		}
	}
	ext := path.Ext(file)
	if file == "" || strings.ContainsAny(file, "$*") || !slices.Contains([]string{".js", ".mjs", ".cjs", ".ts", ".py"}, ext) {
		return Readiness{}, false
	}
	rel := strings.TrimPrefix(path.Clean(file), "/")
	if first, _, _ := strings.Cut(rel, "/"); buildOutputDirs[first] {
		return Readiness{}, false
	}
	stem := strings.TrimSuffix(path.Base(rel), ext)
	sources := []string{path.Base(rel)}
	if ext == ".js" || ext == ".mjs" || ext == ".cjs" {
		sources = append(sources, stem+".ts", stem+".tsx", stem+".mts", stem+".cts", stem+".jsx")
	}
	if len(t.Under(ctx, func(p string) bool { return slices.Contains(sources, path.Base(p)) })) > 0 {
		return Readiness{}, false
	}
	return Readiness{Code: "start-file-missing", Severity: MayFail, Fixer: Developer, Confidence: Possible, Line: s.startLine,
		Message: "The image starts with `" + quote(argv) + "`, but no file named " + path.Base(rel) + " is in the build context and no build step is known to create it.",
		Fix:     "Check the file name in CMD or ENTRYPOINT, or upload the file."}, true
}

func sortPredictions(needs []NetworkNeed, rs []Readiness) {
	sort.SliceStable(needs, func(i, j int) bool {
		return needs[i].Path < needs[j].Path || needs[i].Path == needs[j].Path && needs[i].Line < needs[j].Line
	})
	SortReadiness(rs)
}

var severityRank = map[string]int{WillFail: 0, MayFail: 1, Advice: 2}

// SortReadiness orders items by severity, then by place.
func SortReadiness(rs []Readiness) {
	sort.SliceStable(rs, func(i, j int) bool {
		if severityRank[rs[i].Severity] != severityRank[rs[j].Severity] {
			return severityRank[rs[i].Severity] < severityRank[rs[j].Severity]
		}
		return rs[i].Path < rs[j].Path || rs[i].Path == rs[j].Path && rs[i].Line < rs[j].Line
	})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func scriptCount(n int) string {
	if n == 1 {
		return "1 installed package runs an install script"
	}
	return itoa(n) + " installed packages run install scripts"
}
