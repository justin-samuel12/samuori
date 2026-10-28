package toolrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/convertjob"
	"samuori.invalid/samuori/internal/kubeconvert"
)

// m2kRecording is one recorded run of the real Move2Kube binary, read by the golden tests in kubeconvert.
type m2kRecording struct {
	Note     string                 `json:"note"`
	Command  []string               `json:"command"`
	SHA256   string                 `json:"sha256"`
	Version  string                 `json:"version"`
	Name     string                 `json:"name"`
	Input    kubeconvert.Input      `json:"input"`
	Format   string                 `json:"format"`
	Answers  map[string]string      `json:"answers,omitempty"`
	Asked    []kubeconvert.Question `json:"asked,omitempty"`
	Config   string                 `json:"config"`
	ExitCode int                    `json:"exitCode"`
	Stderr   string                 `json:"stderr"`
	Files    map[string]string      `json:"files"`
}

const m2kOrdersDockerfile = "FROM node:22-alpine\nWORKDIR /app\nCOPY package.json package-lock.json ./\nRUN npm ci --omit=dev\nCOPY server.js ./\nUSER node\nEXPOSE 3000\nCMD [\"node\", \"server.js\"]\n"

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// runM2K plans a run as the controller does, runs it through the runner with the sandbox paths moved below dir,
// and reads the kept output back as the worker reports it.
func runM2K(t *testing.T, bin, dir string, in kubeconvert.PlanInput) (m2kRecording, kubeconvert.Interpretation) {
	t.Helper()
	m := kubeconvert.Move2Kube{}
	plan, err := m.Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	local := func(a string) string {
		for _, root := range []string{convertjob.FilesRoot, convertjob.InputRoot, convertjob.OutputRoot} {
			if a == root || strings.HasPrefix(a, root+"/") {
				return filepath.Join(dir, strings.TrimPrefix(root, "/"), strings.TrimPrefix(a, root))
			}
		}
		return a
	}
	args := []string{bin}
	for _, a := range plan.Args {
		args = append(args, local(a))
	}
	out := filepath.Join(dir, "out")
	_ = os.RemoveAll(out)
	_ = os.RemoveAll(filepath.Join(dir, "work"))
	res := execute(Spec{Args: args, Mkdirs: plan.Mkdirs, Files: plan.Files, Keep: plan.Keep, TimeoutSeconds: 300}, out, filepath.Join(dir, "work"), false)
	_, _, stderr, _ := ReadMeta(out)
	if res.Error != "" {
		t.Fatalf("runner: %+v", res)
	}
	sandbox := strings.NewReplacer(filepath.Join(dir, "in"), convertjob.InputRoot, filepath.Join(dir, "out"), convertjob.OutputRoot, filepath.Join(dir, "work"), "/work")
	rec := m2kRecording{Command: append([]string{"move2kube"}, plan.Args...), Name: in.Name, Input: in.Input, Format: in.Options.Format, Answers: in.Answers, Asked: in.Questions,
		Config: plan.Files["samuori-move2kube.yml"], ExitCode: res.ExitCode, Stderr: sandbox.Replace(string(stderr)), Files: map[string]string{}}
	files := map[string][]byte{}
	root := filepath.Join(out, plan.OutputDir)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b, _ := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		rec.Files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	interp, err := m.Interpret(in, plan, kubeconvert.ToolOutput{ExitCode: res.ExitCode, TimedOut: res.TimedOut, Stderr: rec.Stderr, Files: files})
	if err != nil {
		t.Fatalf("interpret: %v\n%s", err, rec.Stderr)
	}
	return rec, interp
}

// TestRealMove2Kube runs the official Move2Kube v0.3.15 release binary named by SAMUORI_MOVE2KUBE_BINARY through the
// runner with the argv, configuration and prune list Samuori plans, on the examples, including the review → answer →
// resume loop; SAMUORI_MOVE2KUBE_RECORD=1 writes the golden files (docs/spikes/move2kube.md). It checks the provider
// contract, not the isolation: confinement and --network=none apply only inside a Linux conversion step.
func TestRealMove2Kube(t *testing.T) {
	bin := os.Getenv("SAMUORI_MOVE2KUBE_BINARY")
	if bin == "" {
		t.Skip("set SAMUORI_MOVE2KUBE_BINARY to a move2kube v0.3.15 binary")
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	record := func(name string, rec m2kRecording) {
		rec.SHA256, rec.Version = digest, "v0.3.15"
		rec.Note = "Recorded by TestRealMove2Kube with the official release binary and no network; sandbox paths restored in stderr. Regenerate: docs/spikes/move2kube.md."
		if os.Getenv("SAMUORI_MOVE2KUBE_RECORD") != "1" {
			return
		}
		b, _ := json.MarshalIndent(rec, "", "  ")
		if err := os.WriteFile("../kubeconvert/testdata/move2kube-v0.3.15-"+name+".json", append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opts := func(format string) kubeconvert.Options {
		return kubeconvert.Options{Format: format, Replicas: 1}
	}

	t.Run("compose with answers", func(t *testing.T) {
		dir := t.TempDir()
		copyTree(t, "../../examples/compose-shop", filepath.Join(dir, "in"))
		compose, _ := os.ReadFile("../../examples/compose-shop/compose.yml")
		v3 := strings.Replace(string(compose), "name: shop\n", "version: \"3.8\"\n", 1)
		if err := os.WriteFile(filepath.Join(dir, "in", "compose.yml"), []byte(v3), 0o644); err != nil {
			t.Fatal(err)
		}
		in := kubeconvert.PlanInput{Name: "shop", Input: kubeconvert.Input{Kind: kubeconvert.InputCompose, SnapshotID: "s", ComposePath: "compose.yml"}, Options: opts("manifests"), ComposeFile: "compose.yml"}
		rec, first := runM2K(t, bin, dir, in)
		record("shop-defaults", rec)
		t.Logf("run 1: %d files, %d questions, %d findings", len(first.Files), len(first.Questions), len(first.Findings))
		answers := map[string]string{
			`move2kube.services."db".port`:                "5432",
			`move2kube.services."db"."8080".servicetype`:  "ClusterIP",
			`move2kube.services."web"."8080".servicetype`: "ClusterIP",
			`move2kube.services."api"."3000".servicetype`: "Ingress",
			`move2kube.services."api"."3000".urlpath`:     "/api",
			`move2kube.storage.type."db"..options`:        "PVC",
			`move2kube.target."default".ingress.host`:     "shop.example.com",
			`move2kube.target.imageregistry.url`:          "registry.example.invalid",
			`move2kube.target.imageregistry.namespace`:    "shop",
			`move2kube.services.[].enable`:                `["api","db","web"]`,
		}
		if err := (kubeconvert.Move2Kube{}).CheckAnswers(first.Questions, answers); err != nil {
			t.Fatal(err)
		}
		in.Answers, in.Questions = answers, first.Questions
		rec, second := runM2K(t, bin, dir, in)
		record("shop-answered", rec)
		next := map[string]string{}
		var fresh []string
		for _, q := range second.Questions {
			if v, ok := answers[q.ID]; ok {
				next[q.ID] = v
				continue
			}
			fresh = append(fresh, q.ID)
			next[q.ID] = q.Answer
		}
		t.Logf("run 2: %d questions, new: %v", len(second.Questions), fresh)
		next[`move2kube.services."db"."5432".servicetype`] = "ClusterIP"
		if err := (kubeconvert.Move2Kube{}).CheckAnswers(second.Questions, next); err != nil {
			t.Fatal(err)
		}
		in.Answers, in.Questions = next, second.Questions
		rec, third := runM2K(t, bin, dir, in)
		record("shop-resumed", rec)
		for _, q := range third.Questions {
			if _, ok := next[q.ID]; !ok {
				t.Errorf("run 3 still asks %s", q.ID)
			}
		}
		if !strings.Contains(rec.Files["shop/deploy/yamls/db-service.yaml"], "5432") {
			t.Errorf("the answered port did not reach the db Service:\n%s", rec.Files["shop/deploy/yamls/db-service.yaml"])
		}
	})

	t.Run("source", func(t *testing.T) {
		dir := t.TempDir()
		copyTree(t, "../../examples/orders-api", filepath.Join(dir, "in"))
		in := kubeconvert.PlanInput{Name: "orders", Input: kubeconvert.Input{Kind: kubeconvert.InputSource, SnapshotID: "s"}, Options: opts("helm")}
		rec, interp := runM2K(t, bin, dir, in)
		record("orders-source-helm", rec)
		t.Logf("source: %d files, %d questions", len(interp.Files), len(interp.Questions))
	})

	t.Run("source with Dockerfile", func(t *testing.T) {
		dir := t.TempDir()
		copyTree(t, "../../examples/orders-api", filepath.Join(dir, "in"))
		if err := os.WriteFile(filepath.Join(dir, "in", "Dockerfile"), []byte(m2kOrdersDockerfile), 0o644); err != nil {
			t.Fatal(err)
		}
		in := kubeconvert.PlanInput{Name: "orders", Input: kubeconvert.Input{Kind: kubeconvert.InputSource, SnapshotID: "s"}, Options: opts("kustomize")}
		rec, interp := runM2K(t, bin, dir, in)
		record("orders-dockerfile-kustomize", rec)
		t.Logf("dockerfile: %d files, %d questions", len(interp.Files), len(interp.Questions))
	})
}
