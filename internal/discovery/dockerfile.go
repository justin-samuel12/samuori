package discovery

import (
	"bytes"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
)

var logicalSecretID = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

var secretLookingArg = regexp.MustCompile(`(?i)(password|passwd|secret|token|api_?key|private_?key|credential)`)

func isDockerfile(n string) bool {
	l := strings.ToLower(n)
	return l == "dockerfile" || l == "containerfile" || strings.HasSuffix(l, ".dockerfile") || strings.HasPrefix(l, "dockerfile.")
}

// detectDockerfiles analyses every Dockerfile in the tree, or only only, whatever its name.
func detectDockerfiles(t *Tree, only string) []DockerfileAnalysis {
	var out []DockerfileAnalysis
	match := func(p string) bool { return isDockerfile(p[strings.LastIndex(p, "/")+1:]) }
	if only != "" {
		match = func(p string) bool { return p == only }
	}
	for _, p := range t.Under(".", match) {
		raw, ok := t.Read(p)
		if !ok {
			out = append(out, DockerfileAnalysis{Path: p, Stages: []string{}, Args: []string{}, SecretMounts: []SecretMount{},
				Ambiguities:     []Warning{{"unreadable", "This Dockerfile is too large to analyse.", p}},
				RefusedFeatures: []Warning{}})
			continue
		}
		out = append(out, analyseDockerfile(p, raw, t.approved))
	}
	return out
}

// analyseDockerfile parses statically: no instruction runs, no image or frontend is fetched (ADR 0028).
func analyseDockerfile(p string, raw []byte, approved []string) DockerfileAnalysis {
	a := DockerfileAnalysis{Path: p, Stages: []string{}, Args: []string{}, SecretMounts: []SecretMount{}, Ambiguities: []Warning{}, RefusedFeatures: []Warning{}}
	ambiguous := func(code, msg string) { a.Ambiguities = append(a.Ambiguities, Warning{code, msg, p}) }
	refuse := func(code, msg string) { a.RefusedFeatures = append(a.RefusedFeatures, Warning{code, msg, p}) }

	if syntax, _, _, ok := parser.DetectSyntax(raw); ok {
		a.Frontend = syntax
		for _, f := range approved {
			if f == syntax {
				a.FrontendApproved = true
			}
		}
		if !a.FrontendApproved {
			refuse("frontend-not-approved", "The # syntax= frontend "+syntax+" is not on the administrator's approved list; custom frontends run code inside the build engine.")
		}
	}
	res, err := parser.Parse(bytes.NewReader(raw))
	if err != nil {
		ambiguous("parse-error", "The Dockerfile could not be parsed: "+clip(err.Error()))
		return a
	}
	stages, metaArgs, err := instructions.Parse(res.AST, nil)
	if err != nil {
		ambiguous("parse-error", "The Dockerfile has an invalid instruction: "+clip(err.Error()))
		return a
	}
	argNames := map[string]bool{}
	addArg := func(name string) {
		if !argNames[name] {
			argNames[name] = true
			a.Args = append(a.Args, name)
			if secretLookingArg.MatchString(name) {
				ambiguous("secret-like-arg", "Build argument "+name+" looks like a secret. Build arguments are stored in image history; use a secret mount instead.")
			}
		}
	}
	for _, m := range metaArgs {
		for _, kv := range m.Args {
			addArg(kv.Key)
		}
	}
	names := map[string]bool{}
	images := map[string]bool{}
	ports := map[string]bool{}
	byName := map[string]*stage{}
	for i, s := range stages {
		name := s.Name
		if name == "" {
			name = strconv.Itoa(i)
		}
		a.Stages = append(a.Stages, name)
		st := &stage{image: s.BaseName, env: map[string]string{}}
		switch {
		case strings.Contains(s.BaseName, "$"):
			ambiguous("base-image-variable", "Stage "+name+" uses a base image chosen by a build argument ("+s.BaseName+"); confirm the image.")
		case names[strings.ToLower(s.BaseName)] || s.BaseName == "scratch":
			if parent := byName[strings.ToLower(s.BaseName)]; parent != nil {
				st = &stage{image: parent.image, env: mergeEnv(parent.env, nil)}
			}
		default:
			images[s.BaseName] = true
			a.steps.images = append(a.steps.images, stageImage{image: s.BaseName, line: stageLine(s)})
			if r, ok := eolAdvice(s.BaseName); ok {
				r.Path, r.Line = p, stageLine(s)
				a.addReadiness(r)
			}
		}
		names[strings.ToLower(s.Name)] = true
		byName[strings.ToLower(s.Name)] = st
		last := i == len(stages)-1
		for _, c := range s.Commands {
			switch cmd := c.(type) {
			case *instructions.EnvCommand:
				for _, kv := range cmd.Env {
					st.env[kv.Key] = kv.Value
				}
			case *instructions.CmdCommand:
				if last {
					a.steps.setStart(cmd.ShellDependantCmdLine, line(cmd))
				}
			case *instructions.EntrypointCommand:
				if last {
					a.steps.setStart(cmd.ShellDependantCmdLine, line(cmd))
				}
			case *instructions.ArgCommand:
				for _, kv := range cmd.Args {
					addArg(kv.Key)
				}
			case *instructions.ExposeCommand:
				for _, port := range cmd.Ports {
					ports[port] = true
				}
			case *instructions.RunCommand:
				if err := cmd.Expand(literal); err != nil {
					ambiguous("mount-unreadable", "A RUN --mount option could not be read: "+clip(err.Error()))
					continue
				}
				for _, m := range instructions.GetMounts(cmd) {
					switch m.Type {
					case instructions.MountTypeSecret:
						id := m.CacheID
						if id == "" && m.Target != "" {
							id = m.Target[strings.LastIndex(m.Target, "/")+1:]
						}
						if strings.Contains(id, "$") || !logicalSecretID.MatchString(id) {
							ambiguous("secret-id-variable", "A secret mount has an ID that is not a fixed logical name ("+clip(id)+"); confirm which secret it needs.")
							continue
						}
						a.SecretMounts = append(a.SecretMounts, SecretMount{ID: id, Target: m.Target, Required: m.Required, EnvExposed: m.Env != nil})
					case instructions.MountTypeSSH:
						refuse("ssh-mount", "RUN --mount=type=ssh is not supported yet.")
					case instructions.MountTypeBind:
						if m.From != "" && !names[strings.ToLower(m.From)] {
							ambiguous("bind-from-context", "A bind mount reads from "+m.From+", which is not a stage in this file; it needs a named build context.")
						}
					}
				}
				a.inspectRun(st, cmd)
			case *instructions.AddCommand:
				for _, src := range cmd.SourcePaths {
					if remote(src) {
						refuse("remote-add", "ADD from "+clip(src)+" downloads during the build, bypassing egress controls. Download it in a reviewed step or add it to the source.")
					}
				}
			case *instructions.CopyCommand:
				if cmd.From != "" && !names[strings.ToLower(cmd.From)] {
					if _, err := strconv.Atoi(cmd.From); err != nil {
						ambiguous("external-copy-source", "COPY --from="+cmd.From+" refers to an image or named context, not a stage in this file; confirm it.")
					}
				}
			}
		}
	}
	for img := range images {
		a.BaseImages = append(a.BaseImages, img)
	}
	for port := range ports {
		a.ExposedPorts = append(a.ExposedPorts, port)
	}
	sort.Strings(a.BaseImages)
	sort.Strings(a.ExposedPorts)
	if len(a.ExposedPorts) > 1 {
		ambiguous("multiple-ports", "Several ports are exposed ("+strings.Join(a.ExposedPorts, ", ")+"); choose the one the application serves on.")
	}
	return a
}

// literal keeps variable references unexpanded so they are reported as ambiguous rather than evaluated.
func literal(word string) (string, error) { return word, nil }

func remote(src string) bool {
	l := strings.ToLower(src)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "git@") || strings.HasPrefix(l, "git://") || strings.HasSuffix(l, ".git")
}

func clip(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
