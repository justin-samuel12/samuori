package kubeconvert

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// m2kGolden is a recorded run of the real Move2Kube v0.3.15 release binary (internal/toolrun TestRealMove2Kube,
// docs/spikes/move2kube.md).
type m2kGolden struct {
	Command  []string          `json:"command"`
	SHA256   string            `json:"sha256"`
	Name     string            `json:"name"`
	Input    Input             `json:"input"`
	Format   string            `json:"format"`
	Answers  map[string]string `json:"answers"`
	Asked    []Question        `json:"asked"`
	Config   string            `json:"config"`
	ExitCode int               `json:"exitCode"`
	Stderr   string            `json:"stderr"`
	Files    map[string]string `json:"files"`
}

func loadM2K(t *testing.T, name string) m2kGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/move2kube-v0.3.15-" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var g m2kGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g m2kGolden) planInput(t *testing.T) PlanInput {
	t.Helper()
	opts := Options{Format: g.Format, Replicas: 1}
	if err := opts.normalize(Move2Kube{}); err != nil {
		t.Fatal(err)
	}
	return PlanInput{Name: g.Name, Input: g.Input, Options: opts, Answers: g.Answers, Questions: g.Asked, ComposeFile: g.Input.ComposePath}
}

func (g m2kGolden) output() ToolOutput {
	files := map[string][]byte{}
	for p, c := range g.Files {
		files[p] = []byte(c)
	}
	return ToolOutput{ExitCode: g.ExitCode, Stderr: g.Stderr, Files: files}
}

func interpretM2K(t *testing.T, g m2kGolden) (PlanInput, Interpretation) {
	t.Helper()
	in := g.planInput(t)
	plan, err := Move2Kube{}.Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(append([]string{"move2kube"}, plan.Args...), " ") != strings.Join(g.Command, " ") {
		t.Fatalf("the plan changed since the recording:\n%v\n%v", plan.Args, g.Command)
	}
	interp, err := Move2Kube{}.Interpret(in, plan, g.output())
	if err != nil {
		t.Fatal(err)
	}
	return in, interp
}

func question(t *testing.T, qs []Question, id string) Question {
	t.Helper()
	q, ok := findQuestion(qs, id)
	if !ok {
		t.Fatalf("no question %s", id)
	}
	return q
}

// TestMove2KubeQuestionsFromRealCache: the questions come from the QA cache of a real run with their kinds, options
// and defaults; those Samuori answers itself (transformers, containers, replicas, cluster type, registry logins) are
// never offered.
func TestMove2KubeQuestionsFromRealCache(t *testing.T) {
	g := loadM2K(t, "shop-defaults")
	qs, err := m2kQuestions([]byte(g.Files[m2kCache]))
	if err != nil {
		t.Fatal(err)
	}
	port := question(t, qs, `move2kube.services."db".port`)
	if port.Kind != "Select" || port.Default != "8080" || !strings.Contains(strings.Join(port.Options, ","), m2kOther) || port.Text == "" {
		t.Errorf("db port question = %+v", port)
	}
	enable := question(t, qs, `move2kube.services.[].enable`)
	var def []string
	if enable.Kind != "MultiSelect" || json.Unmarshal([]byte(enable.Default), &def) != nil || len(def) != 3 {
		t.Errorf("services question = %+v", enable)
	}
	if st := question(t, qs, `move2kube.storage.type."db"..options`); !strings.Contains(strings.Join(st.Options, ","), "HostPath") || st.Default != "Ignore the data source" {
		t.Errorf("storage question = %+v", st)
	}
	for _, q := range qs {
		for _, locked := range []string{"transformers", "spawncontainers", "minreplicas", "clustertype", "logintype", "transformerselector"} {
			if strings.Contains(q.ID, locked) {
				t.Errorf("Samuori's own setting %s is offered as a question", q.ID)
			}
		}
	}
	if len(qs) != 17 {
		t.Errorf("%d questions", len(qs))
	}
	_, interp := interpretM2K(t, g)
	if len(interp.Questions) != 17 || !has(interp.Findings, SeverityReview, "questions-defaulted", "") {
		t.Errorf("interpretation: %d questions, findings %v", len(interp.Questions), interp.Findings)
	}
}

// TestMove2KubeAnswersValidation (SEC-M2K-02): answers are checked server-side against the questions the last run
// returned: no unasked or locked question, options from the list, typed custom values, no host path storage and no
// template or shell characters in free text.
func TestMove2KubeAnswersValidation(t *testing.T) {
	g := loadM2K(t, "shop-defaults")
	qs, _ := m2kQuestions([]byte(g.Files[m2kCache]))
	m := Move2Kube{}
	ok := map[string]string{
		`move2kube.services."db".port`:                "5432",
		`move2kube.services.db.deployment`:            "StatefulSet",
		`move2kube.services.[].enable`:                `["api","db"]`,
		`move2kube.storage.type."db"..options`:        "PVC",
		`move2kube.target."default".ingress.host`:     "shop.example.com",
		`move2kube.target.imageregistry.url`:          "registry.example.invalid:5000",
		`move2kube.target.imageregistry.namespace`:    "team/shop",
		`move2kube.services."api"."3000".urlpath`:     "/api",
		`move2kube.target."default".ingress.tls`:      "",
		`move2kube.services."web"."8080".servicetype`: "ClusterIP",
	}
	if err := m.CheckAnswers(qs, ok); err != nil {
		t.Fatalf("valid answers refused: %v", err)
	}
	for name, bad := range map[string]map[string]string{
		"unasked":         {"move2kube.services.evil.enable": "true"},
		"locked replicas": {"move2kube.minreplicas": "5"},
		"locked login":    {`move2kube.target.imageregistry."quay.io".logintype`: "username and password"},
		"not an option":   {`move2kube.services.db.deployment`: "DaemonSet"},
		"other literal":   {`move2kube.services."db".port`: m2kOther},
		"bad port":        {`move2kube.services."db".port`: "99999"},
		"host path":       {`move2kube.storage.type."db"..options`: "HostPath"},
		"multi not list":  {`move2kube.services.[].enable`: "api"},
		"multi unknown":   {`move2kube.services.[].enable`: `["api","rogue"]`},
		"template":        {`move2kube.services."api"."3000".urlpath`: "/{{ .Values.x }}"},
		"bad host":        {`move2kube.target."default".ingress.host`: "not a host"},
		"bad registry":    {`move2kube.target.imageregistry.url`: "https://quay.io"},
		"shell":           {`move2kube.target."default".ingress.ingressclassname`: "$(id)"},
	} {
		if err := m.CheckAnswers(qs, bad); err == nil {
			t.Errorf("%s: accepted %v", name, bad)
		}
	}
}

// TestMove2KubeConfigFromAnswers: the configuration file Samuori writes for a re-run reproduces the recorded one
// byte for byte, expands per-service selections, and keeps Samuori's fixed settings whatever the answers say.
func TestMove2KubeConfigFromAnswers(t *testing.T) {
	g := loadM2K(t, "shop-resumed")
	cfg, err := m2kConfig(g.planInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg != g.Config {
		t.Fatalf("configuration differs from the recorded run:\n%s", cfg)
	}
	in := g.planInput(t)
	in.Questions = append(in.Questions, Question{ID: "move2kube.spawncontainers", Kind: "Confirm"})
	in.Answers = map[string]string{"move2kube.spawncontainers": "true", `move2kube.services.[].enable`: `["api"]`}
	cfg, _ = m2kConfig(in)
	var parsed struct {
		M struct {
			Spawn    bool `yaml:"spawncontainers"`
			Services map[string]struct {
				Enable bool `yaml:"enable"`
			} `yaml:"services"`
		} `yaml:"move2kube"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.M.Spawn || !parsed.M.Services["api"].Enable || parsed.M.Services["db"].Enable {
		t.Errorf("configuration = %s", cfg)
	}
}

// TestMove2KubeGoldenPostProcessing (SEC-M2K-03): real Move2Kube output in each layout goes through the same
// validation as Kompose's: literal credentials become Secret references, added capabilities are policy errors, the
// Samuori image is pinned by digest, Helm and Kustomize keep the names they refer to, patches are not mistaken for
// objects, and nothing outside the chosen format is kept.
func TestMove2KubeGoldenPostProcessing(t *testing.T) {
	g := loadM2K(t, "shop-resumed")
	in, interp := interpretM2K(t, g)
	img := &ImageEvidence{Service: "api", Reference: "registry.example.invalid/shop/api@sha256:" + strings.Repeat("ab", 32), Digest: "sha256:" + strings.Repeat("ab", 32), BuildRunID: "b1", Profile: "http-startup", Port: 3000, Path: "/healthz"}
	in.Options.Namespace = "shop"
	out, findings := Process(PostContext{Name: g.Name, Options: in.Options, Image: img}, interp.Files)
	findings = append(interp.Findings, findings...)
	for _, f := range out {
		if strings.Contains(f.Content, examplePassword) {
			t.Errorf("%s holds the literal password", f.Path)
		}
		if !strings.HasPrefix(f.Path, "manifests/") || !strings.HasSuffix(f.Path, ".yml") {
			t.Errorf("unexpected bundle path %s", f.Path)
		}
	}
	api := content(out, "manifests/api-deployment.yml")
	if !strings.Contains(api, img.Reference) || !strings.Contains(api, "readinessProbe") || !strings.Contains(api, "namespace: shop") {
		t.Errorf("api workload:\n%s", api)
	}
	for _, want := range []struct{ sev, code string }{
		{SeverityError, "added-capabilities"}, {SeverityReview, "secret-reference"}, {SeverityInfo, "image-pinned"}, {SeverityInfo, "namespace-from-options"}, {SeverityInfo, "move2kube-output-kept"},
	} {
		if !has(findings, want.sev, want.code, "") {
			t.Errorf("missing %s %s", want.sev, want.code)
		}
	}
	if has(findings, SeverityError, "schema", "") {
		t.Errorf("schema errors on real output: %v", findings)
	}

	g = loadM2K(t, "orders-source-helm")
	in, interp = interpretM2K(t, g)
	out, findings = Process(PostContext{Name: g.Name, Options: in.Options}, interp.Files)
	if content(out, "orders/Chart.yaml") == "" || content(out, "orders/values-dev.yaml") == "" || !has(findings, SeverityReview, "templated", "") || !has(interp.Findings, SeverityReview, "dockerfile-generated", "") {
		t.Errorf("helm: files %v findings %v", paths(out), findings)
	}

	g = loadM2K(t, "orders-dockerfile-kustomize")
	in, interp = interpretM2K(t, g)
	out, findings = Process(PostContext{Name: g.Name, Options: in.Options}, interp.Files)
	if content(out, "kustomize/overlays/dev/kustomization.yaml") == "" || content(out, "kustomize/base/orders-api-deployment.yaml") == "" || content(out, "manifests/kustomization.yml") != "" {
		t.Errorf("kustomize layout changed: %v", paths(out))
	}
	if has(findings, SeverityError, "not-an-object", "") || !has(findings, SeverityInfo, "kustomize-patch", "") {
		t.Errorf("kustomize findings: %v", findings)
	}
	for _, f := range out {
		if strings.Contains(f.Path, "source/") || strings.Contains(f.Path, "scripts/") || strings.Contains(f.Path, "openshift") {
			t.Errorf("kept %s", f.Path)
		}
	}
}

// TestMove2KubeComposeCheck: Compose files Move2Kube v0.3.15 would silently ignore are refused before a run, and
// security-relevant settings it does not carry reliably are noted for review.
func TestMove2KubeComposeCheck(t *testing.T) {
	modern, err := os.ReadFile("../../examples/compose-shop/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Move2Kube{}).CheckCompose(modern); err == nil {
		t.Error("a Compose Specification file was accepted")
	}
	if _, err := (Move2Kube{}).CheckCompose([]byte("version: \"3.8\"\nname: shop\nservices: {}\n")); err == nil {
		t.Error("a top-level name was accepted")
	}
	notes, err := (Move2Kube{}).CheckCompose([]byte(strings.Replace(string(modern), "name: shop\n", "version: \"3.8\"\n", 1)))
	if err != nil || len(notes) != 1 || notes[0].Object != "db" || !strings.Contains(notes[0].Message, "privileged") || !strings.Contains(notes[0].Message, "cap_add") {
		t.Errorf("notes %v: %v", notes, err)
	}
}

func paths(fs []File) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Path)
	}
	return out
}
