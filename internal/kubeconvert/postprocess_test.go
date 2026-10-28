package kubeconvert

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// golden is a recorded run of the real Kompose v1.38.0 release binary on examples/compose-shop (see the file's
// command, sha256 and note fields).
type golden struct {
	Command  string            `json:"command"`
	SHA256   string            `json:"sha256"`
	ExitCode int               `json:"exitCode"`
	Stderr   string            `json:"stderr"`
	Files    map[string]string `json:"files"`
}

func loadGolden(t *testing.T, name string) golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/kompose-v1.38.0-shop-" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

const examplePassword = "example-not-a-real-password"

func convertGolden(t *testing.T, g golden, opts Options, img *ImageEvidence) ([]File, []Finding) {
	t.Helper()
	k := Kompose{}
	if err := opts.normalize(k); err != nil {
		t.Fatal(err)
	}
	in := PlanInput{Name: "shop", Input: Input{Kind: InputCompose, SnapshotID: "s", ComposePath: "compose.yml"}, Options: opts, ComposeFile: "compose.yml"}
	plan, err := k.Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for p, c := range g.Files {
		files[p] = []byte(c)
	}
	interp, err := k.Interpret(in, plan, ToolOutput{ExitCode: g.ExitCode, Stderr: g.Stderr, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	out, findings := Process(PostContext{Name: "shop", Options: opts, Image: img}, interp.Files)
	return out, append(interp.Findings, findings...)
}

func has(findings []Finding, sev, code, field string) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool {
		return f.Severity == sev && f.Code == code && (field == "" || strings.HasSuffix(f.Field, field))
	})
}

func content(files []File, name string) string {
	for _, f := range files {
		if f.Path == name {
			return f.Content
		}
	}
	return ""
}

// TestKomposeGoldenFindings (SEC-K8S-03, SEC-K8S-04): post-processing of real Kompose output keeps every
// security-sensitive setting and reports it, and no literal credential reaches the bundle.
func TestKomposeGoldenFindings(t *testing.T) {
	g := loadGolden(t, "manifests")
	files, findings := convertGolden(t, g, Options{Namespace: "shop"}, nil)
	for _, want := range []struct{ sev, code, field string }{
		{SeverityError, "privileged", "securityContext.privileged"},
		{SeverityError, "added-capabilities", "capabilities.add"},
		{SeverityReview, "secret-reference", "env.DATABASE_PASSWORD"},
		{SeverityReview, "secret-reference", "env.POSTGRES_PASSWORD"},
		{SeverityReview, "host-volume-not-carried", ""},
		{SeverityReview, "no-service-created", ""},
		{SeverityReview, "image-not-pinned", "image"},
		{SeverityWarning, "exposed-outside-cluster", "spec.type"},
	} {
		if !has(findings, want.sev, want.code, want.field) {
			t.Errorf("missing %s finding %s (%s)", want.sev, want.code, want.field)
		}
	}
	if has(findings, SeverityReview, "secret-reference", "env.DATABASE_USER") || has(findings, SeverityReview, "secret-reference", "env.LOG_LEVEL") {
		t.Error("a non-secret variable was turned into a Secret reference")
	}
	for _, f := range findings {
		if f.Code == "schema" {
			t.Errorf("real Kompose output failed validation: %+v", f)
		}
		if strings.Contains(f.Message, "/private/") || strings.Contains(f.Message, "/in/") {
			t.Errorf("a finding leaks a sandbox or host path: %s", f.Message)
		}
	}
	db := content(files, "manifests/db-deployment.yml")
	if !strings.Contains(db, "privileged: true") || !strings.Contains(db, "SYS_NICE") {
		t.Errorf("security-sensitive settings were dropped:\n%s", db)
	}
	if !strings.Contains(db, "secretKeyRef") || !strings.Contains(db, "name: db-secrets") {
		t.Errorf("POSTGRES_PASSWORD is not a Secret reference:\n%s", db)
	}
	for _, f := range files {
		if strings.Contains(f.Content, examplePassword) {
			t.Errorf("%s still holds the literal password", f.Path)
		}
		if strings.HasSuffix(f.Path, ".yaml") {
			t.Errorf("%s keeps Kompose's .yaml name", f.Path)
		}
	}

	var zipped bytes.Buffer
	if err := Bundle(&zipped, "shop", Revision{Number: 1, Files: files, Findings: findings, Lineage: Lineage{Provider: "kompose", ProviderVersion: "v1.38.0"}}); err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(zipped.Bytes()), int64(zipped.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range z.File {
		names = append(names, f.Name)
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if bytes.Contains(b, []byte(examplePassword)) {
			t.Errorf("SEC-K8S-05: %s in the ZIP holds the literal password", f.Name)
		}
	}
	if !slices.Contains(names, "shop/manifests/api-deployment.yml") || !slices.Contains(names, "shop/samuori-review.json") {
		t.Errorf("bundle entries = %v", names)
	}
}

// TestDigestSubstitutionAndProbes: the Samuori image replaces its service's image by digest, and a readiness probe is
// added only when the build's runtime check names a port.
func TestDigestSubstitutionAndProbes(t *testing.T) {
	g := loadGolden(t, "manifests")
	ref := "registry.example.invalid/shop/api@sha256:" + strings.Repeat("a", 64)
	files, findings := convertGolden(t, g, Options{Namespace: "shop"}, &ImageEvidence{Service: "api", Reference: ref, Digest: "sha256:" + strings.Repeat("a", 64),
		BuildRunID: "b1", Profile: "http-startup", Port: 3000, Path: "/healthz"})
	api := content(files, "manifests/api-deployment.yml")
	if !strings.Contains(api, "image: "+ref) || !strings.Contains(api, "readinessProbe") || !strings.Contains(api, "path: /healthz") {
		t.Errorf("api deployment:\n%s", api)
	}
	if !has(findings, SeverityInfo, "image-pinned", "image") || !has(findings, SeverityInfo, "probe-from-evidence", "readinessProbe") {
		t.Error("substitution and probe are not reported")
	}
	if !strings.Contains(content(files, "manifests/web-deployment.yml"), "image: registry.example.invalid/shop/web:1.4.0") {
		t.Error("another service's image changed")
	}
	// The Compose healthcheck became an exec liveness probe written for the replaced image; it is kept and reported.
	if !has(findings, SeverityReview, "probe-from-replaced-image", "livenessProbe") || !strings.Contains(api, "livenessProbe") {
		t.Error("the Compose healthcheck's probe is not reported for the Samuori image")
	}
	if _, plain := convertGolden(t, g, Options{Namespace: "shop"}, nil); has(plain, SeverityReview, "probe-from-replaced-image", "") {
		t.Error("a probe is reported as replaced although no image was substituted")
	}

	_, findings = convertGolden(t, g, Options{Namespace: "shop"}, &ImageEvidence{Service: "api", Digest: "sha256:" + strings.Repeat("b", 64), BuildRunID: "b2", Profile: "process-start"})
	if has(findings, SeverityInfo, "probe-from-evidence", "") || !has(findings, SeverityReview, "no-readiness-probe", "api.readinessProbe") {
		t.Error("a probe was added without port evidence")
	}
	if !has(findings, SeverityError, "image-not-published", "image") {
		t.Error("an unpublished image is not flagged")
	}
}

// TestSchemaAndPolicyFindings (SEC-K8S-04): an invalid object fails validation against Kubernetes 1.36, and host
// access added by an edit is reported.
func TestSchemaAndPolicyFindings(t *testing.T) {
	bad := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replica: 2
  selector:
    matchLabels: {app: api}
  template:
    metadata:
      labels: {app: api}
    spec:
      hostNetwork: true
      containers:
        - name: api
          image: example.invalid/api@sha256:` + strings.Repeat("c", 64) + `
          ports:
            - containerPort: "http"
          securityContext:
            runAsUser: 0
            allowPrivilegeEscalation: true
      volumes:
        - name: root
          hostPath: {path: /}
---
apiVersion: v1
kind: Secret
metadata:
  name: api
data:
  token: c2VjcmV0
`
	files, findings := Process(PostContext{Name: "shop", Options: Options{Format: FormatManifests}}, []File{newFile("manifests/api.yaml", []byte(bad))})
	unknownField := slices.ContainsFunc(findings, func(f Finding) bool { return f.Code == "schema" && strings.Contains(f.Message, "'replica'") })
	if !unknownField || !has(findings, SeverityError, "schema", "containerPort") {
		t.Errorf("schema errors not surfaced: %+v", findings)
	}
	for _, code := range []string{"host-namespace", "host-path", "runs-as-root", "privilege-escalation"} {
		if !has(findings, SeverityError, code, "") {
			t.Errorf("missing %s", code)
		}
	}
	if !has(findings, SeverityReview, "secret-values-removed", "data") || strings.Contains(files[0].Content, "c2VjcmV0") {
		t.Errorf("the Secret's value was kept:\n%s", files[0].Content)
	}
	if !strings.Contains(files[0].Content, "hostNetwork: true") {
		t.Error("hostNetwork was dropped silently")
	}
}

func TestHelmAndKustomizeLayouts(t *testing.T) {
	files, findings := convertGolden(t, loadGolden(t, "chart"), Options{Format: FormatHelm, Namespace: "shop"}, nil)
	if content(files, "shop/Chart.yaml") == "" || content(files, "shop/templates/api-deployment.yml") == "" {
		var names []string
		for _, f := range files {
			names = append(names, f.Path)
		}
		t.Errorf("chart files = %v", names)
	}
	if has(findings, SeverityError, "schema", "") {
		t.Error("chart templates failed validation")
	}
	files, _ = convertGolden(t, loadGolden(t, "manifests"), Options{Format: FormatKustomize, Namespace: "shop"}, nil)
	k := content(files, "manifests/kustomization.yml")
	if !strings.Contains(k, "- api-deployment.yml") || !strings.Contains(k, "namespace: shop") {
		t.Errorf("kustomization:\n%s", k)
	}
}

// TestKomposePlan (SEC-K8S-02): the tool never builds or pushes, takes paths inside the sandbox only, and hostPath
// volumes are not offered.
func TestKomposePlan(t *testing.T) {
	o := Options{Format: FormatHelm}
	if err := o.normalize(Kompose{}); err != nil {
		t.Fatal(err)
	}
	plan, err := Kompose{}.Plan(PlanInput{Name: "shop", Options: o, ComposeFile: "deploy/compose.yml"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(plan.Args, " ")
	for _, want := range []string{"-f /in/deploy/compose.yml", "-o /out/shop", "--build none", "--push-image=false", "--with-kompose-annotation=false", "--chart"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if _, err := (Kompose{}).Plan(PlanInput{Name: "shop", Options: o, ComposeFile: "../etc/passwd"}); err == nil {
		t.Error("a Compose path outside the input was accepted")
	}
	bad := Options{Storage: Storage{Volumes: "hostPath"}}
	if err := bad.normalize(Kompose{}); err == nil {
		t.Error("hostPath volumes were accepted")
	}
}
