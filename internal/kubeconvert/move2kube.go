package kubeconvert

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/convertjob"
)

// Move2Kube runs "move2kube transform" once, non-interactively: it plans and transforms in the same run, answers
// every question with its default or the answers in Samuori's configuration file, and records what it asked in its QA
// cache. Samuori returns those questions to the user and re-runs with the answers (ADR 0048,
// docs/architecture/kubernetes-conversion.md#move2kube).
type Move2Kube struct{}

const (
	m2kOut        = "m2k"
	m2kQA         = m2kOut + "/.m2k-qa"
	m2kCache      = ".m2k-qa/m2kqacache.yaml"
	m2kConfigName = "samuori-move2kube.yml"
	m2kOther      = "Other (specify custom option)"
)

// m2kTransformers are the transformers Samuori lets Move2Kube run: analysers of Compose files, Dockerfiles and source,
// and the Kubernetes and parameterizing (Helm, Kustomize) generators. Transformers that need a container runtime
// (CNBContainerizer), write build or push scripts, CI/CD (Tekton, Argo CD, BuildConfig), Knative or operator
// output, read Cloud Foundry manifests or existing Kubernetes YAML, or target Windows are not offered.
var m2kTransformers = []string{"ClusterSelector", "ComposeAnalyser", "DockerfileDetector", "DockerfileParser", "DotNetCore-Dockerfile", "EarAnalyser", "EarRouter",
	"Golang-Dockerfile", "Gradle", "Jar", "Jboss", "Kubernetes", "Liberty", "Maven", "Nodejs-Dockerfile", "PHP-Dockerfile", "Parameterizer", "Python-Dockerfile",
	"Ruby-Dockerfile", "Rust-Dockerfile", "Tomcat", "WarAnalyser", "WarRouter", "ZuulAnalyser"}

func (Move2Kube) Name() string      { return "move2kube" }
func (Move2Kube) Formats() []string { return []string{FormatManifests, FormatHelm, FormatKustomize} }
func (Move2Kube) Inputs() []string {
	return []string{InputCompose, InputSource, InputBuild, InputPublication}
}

// m2kFormatDir is where Move2Kube writes each format below <output>/<name>.
func m2kFormatDir(format, name string) string {
	switch format {
	case FormatHelm:
		return "deploy/yamls-parameterized/helm-chart/" + name
	case FormatKustomize:
		return "deploy/yamls-parameterized/kustomize"
	}
	return "deploy/yamls"
}

func (m Move2Kube) Plan(in PlanInput) (ToolPlan, error) {
	src := convertjob.InputRoot
	var notes []Finding
	switch in.Input.Kind {
	case InputSource:
		if in.Input.SourceDir != "" {
			src += "/" + in.Input.SourceDir
		}
	default:
		if !convertjob.LocalPath(in.ComposeFile) {
			return ToolPlan{}, invalid("the Compose file path must stay inside the input")
		}
		dir := path.Dir(in.ComposeFile)
		if dir != "." {
			src += "/" + dir
		}
		if in.Input.Kind == InputCompose {
			notes = append(notes, Finding{Severity: SeverityInfo, Code: "move2kube-reads-folder", Source: SourceProvider,
				Message: fmt.Sprintf("Move2Kube analyses the whole folder %s that holds the Compose file, including any Dockerfiles and source in it", or(path.Dir(in.ComposeFile), "of the upload"))})
		}
	}
	cfg, err := m2kConfig(in)
	if err != nil {
		return ToolPlan{}, err
	}
	if len(cfg) > convertjob.MaxJobFileSize {
		return ToolPlan{}, invalid("the answers make a configuration larger than %d KiB", convertjob.MaxJobFileSize>>10)
	}
	out := convertjob.OutputRoot + "/" + m2kOut
	args := []string{"transform", "--source", src, "--output", out, "--name", in.Name, "--qa-skip",
		"--qa-cache-out", convertjob.OutputRoot + "/" + m2kQA, "--config-out", convertjob.OutputRoot + "/" + m2kQA,
		"--disable-local-execution", "--ignore-env", "--config", convertjob.FilesRoot + "/" + m2kConfigName}
	keep := []string{m2kOut + "/" + m2kCache, m2kOut + "/" + in.Name + "/" + m2kFormatDir(in.Options.Format, in.Name)}
	return ToolPlan{Args: args, Mkdirs: []string{m2kOut, m2kQA}, OutputDir: m2kOut, Files: map[string]string{m2kConfigName: cfg}, Keep: keep, Notes: notes}, nil
}

// m2kConfig is the configuration Samuori gives Move2Kube: the user's validated answers, then Samuori's fixed settings, which
// always win: no containers, the chosen replicas, the Kubernetes cluster type and only the offered transformers.
func m2kConfig(in PlanInput) (string, error) {
	root := map[string]any{}
	ids := make([]string, 0, len(in.Answers))
	for id := range in.Answers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		q, ok := findQuestion(in.Questions, id)
		if !ok {
			continue
		}
		segs := idPath(id)
		v := in.Answers[id]
		switch q.Kind {
		case "Confirm":
			setPath(root, segs, v == "true")
		case "MultiSelect":
			var picked []string
			if err := json.Unmarshal([]byte(v), &picked); err != nil {
				return "", invalid("the answer to %s is not a list", id)
			}
			if i := slices.Index(segs, "[]"); i >= 0 {
				for _, o := range q.Options {
					p := slices.Clone(segs)
					p[i] = o
					setPath(root, p, slices.Contains(picked, o))
				}
			} else {
				setPath(root, segs, picked)
			}
		default:
			setPath(root, segs, v)
		}
	}
	replicas := in.Options.Replicas
	if replicas <= 0 {
		replicas = 1
	}
	for _, f := range []struct {
		p string
		v any
	}{
		{"move2kube.spawncontainers", false},
		{"move2kube.minreplicas", strconv.Itoa(replicas)},
		{"move2kube.transformerselector", ""},
		{"move2kube.transformers.types", m2kTransformers},
		{"move2kube.target.default.clustertype", "Kubernetes"},
	} {
		setPath(root, strings.Split(f.p, "."), f.v)
	}
	b, err := marshalYAML(root)
	return string(b), err
}

func findQuestion(qs []Question, id string) (Question, bool) {
	i := slices.IndexFunc(qs, func(q Question) bool { return q.ID == id })
	if i < 0 {
		return Question{}, false
	}
	return qs[i], true
}

// idPath splits a Move2Kube question id into its configuration keys: dots separate keys outside quotes, quotes are
// dropped, and empty keys (as in storage.type."db"..options) are skipped.
func idPath(id string) []string {
	var segs []string
	var cur strings.Builder
	quoted := false
	flush := func() {
		if cur.Len() > 0 {
			segs = append(segs, cur.String())
		}
		cur.Reset()
	}
	for _, r := range id {
		switch {
		case r == '"':
			quoted = !quoted
		case r == '.' && !quoted:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return segs
}

func setPath(m map[string]any, segs []string, v any) {
	for _, s := range segs[:len(segs)-1] {
		next, ok := m[s].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[s] = next
		}
		m = next
	}
	m[segs[len(segs)-1]] = v
}

// m2kLocked are questions Samuori answers itself or never offers: transformer selection, containers, replicas (an
// option), the cluster type Samuori validates against, registry logins (credentials) and CI/CD.
var m2kLocked = []*regexp.Regexp{
	regexp.MustCompile(`^move2kube\.transformerselector$`),
	regexp.MustCompile(`^move2kube\.transformers\.types$`),
	regexp.MustCompile(`^move2kube\.spawncontainers$`),
	regexp.MustCompile(`^move2kube\.minreplicas$`),
	regexp.MustCompile(`^move2kube\.target\."?default"?\.clustertype$`),
	regexp.MustCompile(`^move2kube\.target\.imageregistry\.("[^"]*"|[^.]+)\.logintype$`),
	regexp.MustCompile(`\.cicd\.|argocd|\.tekton\.`),
}

var m2kIDRe = regexp.MustCompile(`^move2kube\.[A-Za-z0-9._"\[\]:/ -]{1,300}$`)

const maxQuestions = 200

type qaCache struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		Solutions []struct {
			ID          string   `yaml:"id"`
			Type        string   `yaml:"type"`
			Description string   `yaml:"description"`
			Hints       []string `yaml:"hints"`
			Options     []string `yaml:"options"`
			Default     any      `yaml:"default"`
			Answer      any      `yaml:"answer"`
		} `yaml:"solutions"`
	} `yaml:"spec"`
}

// m2kQuestions reads the questions Move2Kube asked from its QA cache, leaving out the ones Samuori answers itself and
// passwords, which are never offered.
func m2kQuestions(raw []byte) ([]Question, error) {
	var c qaCache
	if err := yaml.Unmarshal(raw, &c); err != nil || c.Kind != "QACache" {
		return nil, invalid("Move2Kube's question cache is not readable")
	}
	var out []Question
	for _, s := range c.Spec.Solutions {
		if !m2kIDRe.MatchString(s.ID) || s.Type == "Password" || slices.ContainsFunc(m2kLocked, func(r *regexp.Regexp) bool { return r.MatchString(s.ID) }) {
			continue
		}
		q := Question{ID: s.ID, Kind: s.Type, Text: clipString(s.Description, 500), Default: m2kValue(s.Default), Answer: m2kValue(s.Answer)}
		for _, h := range s.Hints {
			if len(q.Hints) < 3 {
				q.Hints = append(q.Hints, clipString(h, 300))
			}
		}
		for _, o := range s.Options {
			if len(q.Options) < 100 {
				q.Options = append(q.Options, clipString(o, 200))
			}
		}
		if i := slices.IndexFunc(out, func(x Question) bool { return x.ID == q.ID }); i >= 0 {
			out[i] = q
			continue
		}
		if len(out) == maxQuestions {
			break
		}
		out = append(out, q)
	}
	return out, nil
}

// m2kValue renders a default or answer as Samuori stores answers: lists as a JSON array, booleans as true or false.
func m2kValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return clipString(t, 1024)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		list := []string{}
		for _, e := range t {
			list = append(list, fmt.Sprint(e))
		}
		b, _ := json.Marshal(list)
		return string(b)
	}
	return clipString(fmt.Sprint(v), 1024)
}

var (
	urlPathRe    = regexp.MustCompile(`^/?[A-Za-z0-9._~/-]{0,200}$`)
	subdomainRe  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	registryRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)
	repoPathRe   = regexp.MustCompile(`^[a-z0-9]+([._/-][a-z0-9]+)*$`)
	plainInputRe = regexp.MustCompile(`^[A-Za-z0-9 ._:/@=+,()-]{0,256}$`)
)

// m2kInputRule checks a typed value for a question id; questions without a rule take plain printable text without
// template, quote or shell characters.
func m2kInputRule(id, v string) error {
	switch {
	case strings.HasSuffix(id, ".urlpath"):
		if !urlPathRe.MatchString(v) {
			return fmt.Errorf("a URL path such as /orders")
		}
	case strings.HasSuffix(id, ".ingress.host"):
		if v != "" && !hostRe.MatchString(v) {
			return fmt.Errorf("a DNS name such as shop.example.com, or empty")
		}
	case strings.HasSuffix(id, ".ingress.ingressclassname"):
		if v != "" && !dnsLabel.MatchString(v) {
			return fmt.Errorf("an ingress class name (a DNS label), or empty")
		}
	case strings.HasSuffix(id, ".ingress.tls"):
		if v != "" && (len(v) > 253 || !subdomainRe.MatchString(v)) {
			return fmt.Errorf("the name of a TLS Secret, or empty")
		}
	case id == "move2kube.target.imageregistry.url":
		if !registryRe.MatchString(v) {
			return fmt.Errorf("a registry host such as registry.example.com")
		}
	case id == "move2kube.target.imageregistry.namespace":
		if len(v) > 255 || !repoPathRe.MatchString(v) {
			return fmt.Errorf("a repository path such as team/shop")
		}
	case strings.HasSuffix(id, ".port"):
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("a port from 1 to 65535")
		}
	default:
		if !plainInputRe.MatchString(v) {
			return fmt.Errorf("letters, digits, spaces and . _ : / @ = + , ( ) - only, at most 256")
		}
	}
	return nil
}

// CheckAnswers validates answers server-side against the questions the last run returned: only asked questions,
// options from their list, typed values for inputs, and never a host path for storage (Samuori policy).
func (Move2Kube) CheckAnswers(questions []Question, answers map[string]string) error {
	if len(answers) > maxQuestions {
		return invalid("too many answers")
	}
	for id, v := range answers {
		q, ok := findQuestion(questions, id)
		if !ok {
			return invalid("%s was not asked by the last run; run again and answer its questions", clipString(id, 120))
		}
		if len(v) > 1024 || strings.ContainsAny(v, "\x00\r") {
			return invalid("the answer to %q is too long", q.Text)
		}
		switch q.Kind {
		case "Confirm":
			if v != "true" && v != "false" {
				return invalid("answer %q with true or false", q.Text)
			}
		case "MultiSelect":
			var picked []string
			if err := json.Unmarshal([]byte(v), &picked); err != nil {
				return invalid("the answer to %q must be a list of its options", q.Text)
			}
			seen := map[string]bool{}
			for _, p := range picked {
				if !slices.Contains(q.Options, p) || seen[p] || p == m2kOther {
					return invalid("%q is not an option of %q", clipString(p, 80), q.Text)
				}
				seen[p] = true
			}
		case "Select":
			if v == m2kOther {
				return invalid("for %q, type the value instead of choosing Other", q.Text)
			}
			if strings.HasPrefix(id, "move2kube.storage.") && strings.EqualFold(v, "HostPath") {
				return invalid("host path storage is not offered: it exposes the node's filesystem (Samuori policy)")
			}
			if slices.Contains(q.Options, v) {
				continue
			}
			if !slices.Contains(q.Options, m2kOther) {
				return invalid("%q is not an option of %q", clipString(v, 80), q.Text)
			}
			if err := m2kInputRule(id, v); err != nil {
				return invalid("the answer to %q must be %v", q.Text, err)
			}
		case "Input", "MultiLineInput":
			if err := m2kInputRule(id, v); err != nil {
				return invalid("the answer to %q must be %v", q.Text, err)
			}
		default:
			return invalid("Samuori cannot answer %q (a %s question)", q.Text, q.Kind)
		}
	}
	return nil
}

var m2kComposeKeys = []string{"version", "services", "volumes", "networks", "secrets", "configs"}

// CheckCompose refuses Compose files Move2Kube v0.3.15 silently ignores: it reads only files with a version of 1, 2
// or 3 and no top-level keys beyond those versions (not the Compose Specification with name: or without version:).
// It returns a review finding for every service setting Move2Kube does not carry over reliably: in recorded runs
// it dropped privileged: true from identical input in some runs and not others (docs/spikes/move2kube.md).
func (Move2Kube) CheckCompose(raw []byte) ([]Finding, error) {
	var top map[string]any
	if err := yaml.Unmarshal(raw, &top); err != nil || top == nil {
		return nil, invalid("the Compose file is not valid YAML")
	}
	const use = "; use Kompose for it, or add version: \"3.8\" and remove other top-level keys"
	v, versioned := top["version"]
	services, _ := top["services"].(map[string]any)
	switch {
	case !versioned && top["services"] != nil:
		return nil, invalid("Move2Kube reads only Compose files with a version of 1, 2 or 3; this file follows the Compose Specification (no version)" + use)
	case !versioned:
		services = map[string]any{}
		for k, s := range top {
			if m, ok := s.(map[string]any); ok {
				services[k] = m
			}
		}
	default:
		if s := strings.Trim(fmt.Sprint(v), `"' `); s == "" || !strings.ContainsAny(s[:1], "123") {
			return nil, invalid("Move2Kube reads only Compose files with a version of 1, 2 or 3, not %q"+use, s)
		}
		for k := range top {
			if !slices.Contains(m2kComposeKeys, k) && !strings.HasPrefix(k, "x-") {
				return nil, invalid("Move2Kube ignores a Compose file with the top-level key %q"+use, k)
			}
		}
	}
	var notes []Finding
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s, _ := services[n].(map[string]any)
		var set []string
		if s["privileged"] == true {
			set = append(set, "privileged")
		}
		for _, k := range []string{"cap_add", "security_opt", "devices"} {
			if s[k] != nil {
				set = append(set, k)
			}
		}
		for _, k := range []string{"network_mode", "pid", "ipc"} {
			if str(s[k]) == "host" {
				set = append(set, k+": host")
			}
		}
		if len(set) > 0 {
			notes = append(notes, Finding{Severity: SeverityReview, Code: "compose-security-setting", Source: SourceProvider, Object: n,
				Message: fmt.Sprintf("Compose service %s sets %s; Move2Kube does not carry such settings over reliably, so compare the generated workload with the Compose file and keep or drop each deliberately", n, strings.Join(set, ", "))})
		}
	}
	return notes, nil
}

var (
	m2kLogLine = regexp.MustCompile(`level=(\w+) msg="((?:[^"\\]|\\.)*)"`)
	m2kTemp    = regexp.MustCompile(`/work/tmp/[^\s"'\\]*`)
	m2kOption  = regexp.MustCompile(`^Using the transformation option '([^']+)' for the service '([^']+)'`)
	m2kIgnored = regexp.MustCompile(`^User has ignored data in path \[(.*)\]\. No storage type created`)
	m2kCount   = regexp.MustCompile(`^Planning done\. Number of services identified: (\d+)`)
)

// m2kClean makes a log message readable without sandbox paths.
func m2kClean(msg string) string {
	msg = strings.ReplaceAll(msg, convertjob.OutputRoot+"/"+m2kOut+"/", "")
	msg = m2kTemp.ReplaceAllString(msg, "(temporary file)")
	return clipString(cleanLog(msg), 500)
}

// m2kHarmless are errors Move2Kube v0.3.15 logs in Samuori's sandbox about its copy of the input and its build
// scripts, both of which Samuori neither generates nor keeps (docs/spikes/move2kube.md).
func m2kHarmless(msg string) bool {
	return strings.Contains(msg, "Type:Source") || strings.Contains(msg, "points to a unknown path") || strings.Contains(msg, "relative to the env output dir")
}

func (m Move2Kube) Interpret(in PlanInput, plan ToolPlan, out ToolOutput) (Interpretation, error) {
	var res Interpretation
	var fatal []string
	services := -1
	seen := map[string]bool{}
	add := func(f Finding) {
		f.Source = SourceProvider
		if !seen[f.Code+f.Message] {
			seen[f.Code+f.Message] = true
			res.Findings = append(res.Findings, f)
		}
	}
	for _, line := range strings.Split(out.Stderr, "\n") {
		g := m2kLogLine.FindStringSubmatch(line)
		if g == nil {
			continue
		}
		raw, err := strconv.Unquote(`"` + g[2] + `"`)
		if err != nil {
			raw = g[2]
		}
		msg := m2kClean(raw)
		switch g[1] {
		case "warning":
			if w := m2kIgnored.FindStringSubmatch(msg); w != nil {
				add(Finding{Severity: SeverityReview, Code: "storage-not-carried", Message: fmt.Sprintf("Move2Kube created no volume for %s, so its data is not carried over; answer its storage question or add a volume claim", w[1])})
				continue
			}
			add(Finding{Severity: SeverityWarning, Code: "move2kube-warning", Message: "Move2Kube: " + msg})
		case "error":
			if m2kHarmless(raw) {
				add(Finding{Severity: SeverityInfo, Code: "move2kube-internal", Message: "Move2Kube reported a path error about its copy of the input or its build scripts, which Samuori does not keep; the generated files are not affected"})
				continue
			}
			add(Finding{Severity: SeverityWarning, Code: "move2kube-error", Message: "Move2Kube: " + msg})
		case "fatal", "panic":
			fatal = append(fatal, msg)
		case "info":
			if o := m2kOption.FindStringSubmatch(msg); o != nil {
				f := Finding{Severity: SeverityInfo, Code: "transformation-option", Message: fmt.Sprintf("Move2Kube converted service %s with its %s transformer", o[2], o[1])}
				if strings.HasSuffix(o[1], "-Dockerfile") {
					f.Severity, f.Code = SeverityReview, "dockerfile-generated"
					f.Message = fmt.Sprintf("Move2Kube generated its own Dockerfile for service %s (%s) instead of using one from the upload; Samuori does not keep it, and the image and port in the manifests come from it, so check them or choose a Samuori build", o[2], o[1])
				}
				add(f)
			}
			if c := m2kCount.FindStringSubmatch(msg); c != nil {
				services, _ = strconv.Atoi(c[1])
			}
		}
	}
	if out.TimedOut {
		return res, invalid("Move2Kube did not finish within its time limit")
	}
	if out.ExitCode != 0 {
		detail := strings.Join(fatal, "; ")
		if detail == "" {
			detail = fmt.Sprintf("exit status %d", out.ExitCode)
		}
		return res, invalid("Move2Kube failed: %s", clipString(detail, 500))
	}
	cache, ok := out.Files[m2kCache]
	if !ok {
		return res, invalid("Move2Kube wrote no question cache")
	}
	qs, err := m2kQuestions(cache)
	if err != nil {
		return res, err
	}
	res.Questions = qs
	prefix := in.Name + "/" + m2kFormatDir(in.Options.Format, in.Name) + "/"
	names := make([]string, 0, len(out.Files))
	for name := range out.Files {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		rel := strings.TrimPrefix(name, prefix)
		bundle := path.Join("manifests", rel)
		switch in.Options.Format {
		case FormatHelm:
			bundle = path.Join(in.Name, rel)
		case FormatKustomize:
			bundle = path.Join("kustomize", rel)
		}
		res.Files = append(res.Files, newFile(bundle, out.Files[name]))
	}
	if len(res.Files) == 0 {
		why := "it found nothing it could convert"
		if services == 0 {
			why = "it identified no service in the input"
		}
		return res, invalid("Move2Kube generated no %s: %s (see its findings)", in.Options.Format, why)
	}
	add(Finding{Severity: SeverityInfo, Code: "move2kube-output-kept", Message: fmt.Sprintf("Samuori keeps only Move2Kube's %s output; its copy of the input, build and push scripts, CI/CD, Knative and OpenShift files are not generated or kept", in.Options.Format)})
	if n := len(unanswered(qs, in.Answers)); n > 0 {
		add(Finding{Severity: SeverityReview, Code: "questions-defaulted", Message: fmt.Sprintf("Move2Kube answered %d questions with its defaults; review them and run again with your answers", n)})
	}
	return res, nil
}

// unanswered lists the questions without a user answer.
func unanswered(qs []Question, answers map[string]string) []Question {
	var out []Question
	for _, q := range qs {
		if _, ok := answers[q.ID]; !ok {
			out = append(out, q)
		}
	}
	return out
}
