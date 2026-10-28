package discovery

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// shellCommands splits a shell script into simple commands of literal words. It reads quoting and separators only;
// nothing is expanded or evaluated, so words holding $ stay as written and are treated as unknown.
func shellCommands(s string) [][]string {
	var out [][]string
	var cur []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			cur = append(cur, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		endWord()
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			if s[i] != '\n' {
				word.WriteByte(s[i])
				inWord = true
			}
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			word.WriteString(s[i+1 : i+1+j])
			inWord = true
			i += j + 1
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				word.WriteByte(s[i])
			}
			inWord = true
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == '\n' || c == ';' || c == '|' || c == '&' || c == '(' || c == ')' || c == '`':
			endCommand()
		case c == '#' && !inWord:
			for i < len(s) && s[i] != '\n' {
				i++
			}
			endCommand()
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endCommand()
	return out
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

var wrappers = map[string]bool{"sudo": true, "exec": true, "env": true, "time": true, "nice": true, "nohup": true, "command": true,
	"then": true, "do": true, "else": true, "if": true, "elif": true, "while": true, "until": true, "!": true, "{": true, "}": true}

// simple drops leading variable assignments, which it returns as the command's own environment, and wrappers such
// as sudo or env.
func simple(args []string) ([]string, map[string]string) {
	env := map[string]string{}
	for len(args) > 0 {
		switch {
		case assignment.MatchString(args[0]):
			k, v, _ := strings.Cut(args[0], "=")
			env[k] = v
		case wrappers[args[0]], args[0] == "-i" || args[0] == "-E" || args[0] == "--":
		default:
			return args, env
		}
		args = args[1:]
	}
	return nil, env
}

// step is what one command is predicted to do.
type step struct {
	needs         []NetworkNeed
	readiness     []Readiness
	nodeInstall   bool
	omitDev       bool
	ignoreScripts bool
	script        string
	pipInstall    bool
	gradle        bool
	npmRegistry   string
	triggers      [][]string
}

func need(kind, cause, preset string, conf string, dests ...string) NetworkNeed {
	return NetworkNeed{Kind: kind, Cause: cause, Preset: preset, Destinations: dests, Confidence: conf, Severity: WillFail, Fixer: Administrator}
}

func flagValue(args []string, names ...string) (string, bool) {
	for i, a := range args {
		for _, n := range names {
			switch {
			case a == n && i+1 < len(args):
				return args[i+1], true
			case strings.HasPrefix(a, n+"="):
				return a[len(n)+1:], true
			}
		}
	}
	return "", false
}

func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		if slices.Contains(names, a) {
			return true
		}
	}
	return false
}

// subcommand is the first word after the program that is not an option.
func subcommand(args []string) string {
	for _, a := range args[1:] {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

func truthy(v string) bool {
	v = strings.ToLower(v)
	return v != "" && v != "0" && v != "false" && v != "no"
}

// destination turns a literal http(s) or git URL into host:port; URLs with variables are unknown.
func destination(raw string) (string, bool) {
	if strings.Contains(raw, "$") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	if host == "" || !hostOrIP.MatchString(host) {
		return "", false
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port, true
	}
	return host + ":" + port, true
}

var hostOrIP = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$|^[0-9a-f:.]{2,45}$`)

func quote(args []string) string {
	return clipTo(strings.Join(args, " "), 120)
}

func clipTo(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

var installVerbs = map[string]map[string]bool{
	"npm":  {"ci": true, "install": true, "i": true, "in": true, "add": true, "isntall": true, "update": true, "up": true, "upgrade": true},
	"pnpm": {"install": true, "i": true, "add": true, "update": true, "up": true, "fetch": true},
	"yarn": {"": true, "install": true, "add": true, "upgrade": true},
	"bun":  {"install": true, "i": true, "add": true, "update": true},
}

var runVerbs = map[string]bool{"run": true, "run-script": true}

// classify predicts what one simple command needs: env is the stage environment plus the command's own
// assignments, image the stage's base image. Package scripts and RUN steps share it.
func classify(args []string, env map[string]string, image string) step {
	var st step
	if len(args) == 0 {
		return st
	}
	prog := path.Base(args[0])
	cause := quote(args)
	if (prog == "sh" || prog == "bash" || prog == "ash" || prog == "dash") && len(args) > 2 && args[1] == "-c" {
		for _, c := range shellCommands(args[2]) {
			inner, own := simple(c)
			merged := mergeEnv(env, own)
			sub := classify(inner, merged, image)
			st.merge(sub)
		}
		return st
	}
	if strings.HasPrefix(prog, "python") && len(args) > 2 && args[1] == "-m" {
		args = args[2:]
		prog = path.Base(args[0])
	}
	sub := subcommand(args)
	switch prog {
	case "npm", "pnpm", "yarn", "bun":
		if prog == "yarn" && sub != "" && !installVerbs["yarn"][sub] && !runVerbs[sub] && sub != "dlx" && sub != "exec" && sub != "workspace" && sub != "config" {
			st.script = sub
			return st
		}
		if v, ok := flagValue(args, "--registry"); ok {
			st.npmRegistry = v
		}
		switch {
		case installVerbs[prog][sub]:
			st.nodeInstall = true
			st.ignoreScripts = hasFlag(args, "--ignore-scripts") || truthy(env["npm_config_ignore_scripts"]) || truthy(env["NPM_CONFIG_IGNORE_SCRIPTS"])
			omit, _ := flagValue(args, "--omit")
			include, _ := flagValue(args, "--include")
			only, hasOnly := flagValue(args, "--only")
			st.omitDev = (omit == "dev" || hasFlag(args, "--production", "--prod") || (hasOnly && strings.HasPrefix(only, "prod") && !strings.Contains(only, "=false")) ||
				env["NODE_ENV"] == "production" || truthy(env["npm_config_production"])) && include != "dev"
			if prog == "npm" && hasOnly {
				fix := "Use --omit=dev to leave out devDependencies."
				if strings.Contains(only, "false") || strings.HasPrefix(only, "dev") {
					fix = "Use --include=dev to install devDependencies as well."
				}
				st.readiness = append(st.readiness, Readiness{Code: "npm-only-flag", Severity: Advice, Fixer: Developer, Confidence: Certain,
					Message: "`" + cause + "` uses --only=" + only + ", which npm 7 and later no longer honour, so the install may not include what you expect.", Fix: fix})
			}
			if prog == "npm" && hasFlag(args, "--production") {
				st.readiness = append(st.readiness, Readiness{Code: "npm-production-flag", Severity: Advice, Fixer: Developer, Confidence: Certain,
					Message: "`" + cause + "` uses --production, which npm deprecates.", Fix: "Use --omit=dev instead."})
			}
			if hasFlag(args, "--offline") {
				return st
			}
			reg := st.npmRegistry
			if reg == "" {
				reg = firstNonEmpty(env["npm_config_registry"], env["NPM_CONFIG_REGISTRY"])
			}
			switch dest, ok := destination(reg); {
			case reg != "" && ok:
				st.needs = append(st.needs, need("registry", cause, "", Certain, dest))
			case reg != "":
			case prog == "yarn":
				st.needs = append(st.needs, need("package-manager", cause, "yarn", Certain))
			default:
				st.needs = append(st.needs, need("package-manager", cause, "npm", Certain))
			}
		case runVerbs[sub]:
			if rest := afterWord(args, sub); len(rest) > 0 {
				st.script = rest[0]
			}
		case prog == "npm" && sub == "config" && len(args) >= 5 && args[2] == "set" && args[3] == "registry":
			st.npmRegistry = args[4]
		case sub == "exec" || sub == "x" || sub == "dlx":
			st.triggers = append(st.triggers, afterWord(args, sub))
		}
	case "npx", "pnpx", "bunx":
		st.triggers = append(st.triggers, withoutOptions(args[1:]))
	case "pip", "pip3", "uv", "poetry", "pipenv", "pipx":
		python(&st, prog, sub, args, env, cause)
	case "apk":
		if (sub == "add" || sub == "update" || sub == "upgrade" || sub == "fetch") && !hasFlag(args, "--no-network") {
			st.needs = append(st.needs, need("package-manager", cause, "alpine", Certain))
			if v, ok := flagValue(args, "--repository", "-X"); ok {
				if d, ok := destination(v); ok {
					st.needs = append(st.needs, need("registry", cause, "", Certain, d))
				}
			}
		}
	case "apt-get", "apt", "aptitude":
		switch sub {
		case "update", "install", "upgrade", "dist-upgrade", "full-upgrade", "build-dep", "source", "download":
			preset := "debian"
			if strings.Contains(image, "ubuntu") {
				preset = "ubuntu"
			}
			st.needs = append(st.needs, need("package-manager", cause, preset, Certain))
		}
	case "dnf", "yum", "microdnf", "tdnf", "zypper":
		switch sub {
		case "install", "update", "upgrade", "makecache", "groupinstall", "in", "up", "refresh":
			st.readiness = append(st.readiness, Readiness{Code: "rpm-mirrors", Severity: WillFail, Fixer: Administrator, Confidence: Likely,
				Message: "`" + cause + "` downloads from the distribution's package mirrors, which no egress preset covers.",
				Fix:     "Ask an organization administrator to add the mirror hosts the base image uses as egress destinations.", Alternative: "Use a base image that already contains the packages."})
		}
	case "go":
		goNeeds(&st, sub, env, cause)
	case "dotnet":
		switch {
		case sub == "restore", sub == "tool" && len(args) > 2 && args[2] == "install",
			(sub == "build" || sub == "publish" || sub == "test" || sub == "run" || sub == "pack") && !hasFlag(args, "--no-restore"):
			if v, ok := flagValue(args, "--source", "-s"); ok {
				if d, ok := destination(v); ok {
					st.needs = append(st.needs, need("registry", cause, "", Certain, d))
				}
				return st
			}
			conf := Certain
			if sub != "restore" && sub != "tool" {
				conf = Likely
			}
			st.needs = append(st.needs, need("package-manager", cause, "nuget", conf))
		}
	case "cargo":
		switch sub {
		case "build", "fetch", "install", "vendor", "test", "run", "update", "generate-lockfile":
			if !hasFlag(args, "--offline", "--frozen") && !truthy(env["CARGO_NET_OFFLINE"]) {
				st.needs = append(st.needs, need("package-manager", cause, "crates-io", Certain))
			}
		}
	case "mvn", "mvnw":
		if !hasFlag(args, "-o", "--offline") {
			st.needs = append(st.needs, need("package-manager", cause, "maven-central", Likely))
		}
	case "gradle", "gradlew":
		if !hasFlag(args, "--offline") {
			st.gradle = true
			st.needs = append(st.needs, need("package-manager", cause, "maven-central", Likely))
			if prog == "gradlew" {
				st.needs = append(st.needs, need("url", cause, "", Likely, "services.gradle.org:443"))
			}
		}
	case "gem":
		if sub == "install" || sub == "update" {
			st.needs = append(st.needs, need("package-manager", cause, "", Certain, "rubygems.org:443", "index.rubygems.org:443"))
		}
	case "bundle", "bundler":
		if sub == "install" || sub == "update" || sub == "" {
			st.needs = append(st.needs, need("package-manager", cause, "", Certain, "rubygems.org:443", "index.rubygems.org:443"))
		}
	case "curl", "wget":
		for _, a := range args[1:] {
			if d, ok := destination(a); ok {
				st.needs = append(st.needs, need("url", cause, "", Certain, d))
			}
		}
	case "git":
		switch sub {
		case "clone", "fetch", "pull", "ls-remote", "submodule":
			for _, a := range args[2:] {
				if d, ok := destination(a); ok {
					st.needs = append(st.needs, need("url", cause, "", Certain, d))
				}
			}
		}
	default:
		st.triggers = append(st.triggers, args)
	}
	return st
}

func python(st *step, prog, sub string, args []string, env map[string]string, cause string) {
	switch prog {
	case "pip", "pip3":
		if sub != "install" && sub != "download" && sub != "wheel" {
			return
		}
	case "uv":
		if sub == "pip" && len(args) > 2 {
			sub = args[2]
		}
		if sub != "install" && sub != "sync" && sub != "add" && sub != "lock" && sub != "compile" && sub != "tool" {
			return
		}
	case "poetry":
		if sub != "install" && sub != "add" && sub != "lock" && sub != "update" {
			return
		}
	case "pipenv":
		if sub != "install" && sub != "sync" && sub != "lock" && sub != "update" {
			return
		}
	case "pipx":
		if sub != "install" && sub != "run" {
			return
		}
	}
	if hasFlag(args, "--no-index", "--offline") {
		return
	}
	st.pipInstall = true
	index, hasIndex := flagValue(args, "--index-url", "-i", "--default-index")
	if !hasIndex {
		index = firstNonEmpty(env["PIP_INDEX_URL"], env["UV_INDEX_URL"], env["UV_DEFAULT_INDEX"])
		hasIndex = index != ""
	}
	if hasIndex {
		if d, ok := destination(index); ok {
			st.needs = append(st.needs, need("registry", cause, "", Certain, d))
		}
	} else {
		st.needs = append(st.needs, need("package-manager", cause, "pypi", Certain))
	}
	for _, flag := range []string{"--extra-index-url", "--find-links", "-f", "--index"} {
		if v, ok := flagValue(args, flag); ok {
			if d, ok := destination(v); ok {
				st.needs = append(st.needs, need("registry", cause, "", Certain, d))
			}
		}
	}
}

func goNeeds(st *step, sub string, env map[string]string, cause string) {
	conf := Certain
	switch sub {
	case "mod", "get", "install":
	case "build", "test", "run", "generate", "vet", "list":
		conf = Likely
	default:
		return
	}
	if strings.Contains(env["GOFLAGS"], "-mod=vendor") || env["GOPROXY"] == "off" {
		return
	}
	if proxy := env["GOPROXY"]; proxy != "" && !strings.Contains(proxy, "proxy.golang.org") {
		for _, p := range strings.FieldsFunc(proxy, func(r rune) bool { return r == ',' || r == '|' }) {
			if d, ok := destination(p); ok {
				st.needs = append(st.needs, need("registry", cause, "", conf, d))
			}
		}
		if env["GOSUMDB"] != "off" && env["GONOSUMDB"] == "" && env["GOFLAGS"] == "" {
			st.needs = append(st.needs, need("url", cause, "", conf, "sum.golang.org:443"))
		}
		return
	}
	st.needs = append(st.needs, need("package-manager", cause, "go-proxy", conf))
}

func (s *step) merge(o step) {
	s.needs = append(s.needs, o.needs...)
	s.readiness = append(s.readiness, o.readiness...)
	s.triggers = append(s.triggers, o.triggers...)
	s.nodeInstall = s.nodeInstall || o.nodeInstall
	s.omitDev = s.omitDev || o.omitDev
	s.ignoreScripts = s.ignoreScripts || o.ignoreScripts
	s.pipInstall = s.pipInstall || o.pipInstall
	s.gradle = s.gradle || o.gradle
	if o.script != "" {
		s.script = o.script
	}
	if o.npmRegistry != "" {
		s.npmRegistry = o.npmRegistry
	}
}

func afterWord(args []string, w string) []string {
	for i, a := range args {
		if a == w {
			return withoutOptions(args[i+1:])
		}
	}
	return nil
}

func withoutOptions(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

func mergeEnv(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }
