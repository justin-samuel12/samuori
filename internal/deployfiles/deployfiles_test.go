package deployfiles

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"testing"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/kubeconvert"
)

const digest = "sha256:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"

var (
	published = Image{Repository: "registry.example.com/payments/orders-api", Tag: "2026-10-27.1", Digest: digest, Published: true}
	local     = Image{Repository: "payments/orders-api", Tag: "2026-10-27.1"}
	web       = &buildspec.Application{Type: "http-service", Port: 8080, HealthPath: "/healthz"}
)

func input(img Image, app *buildspec.Application) Input {
	return Input{Name: "orders-api", Version: "2026-10-27.1", App: app, Image: img, ModTime: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
}

// render executes the chart's templates the way helm template does, with the functions the templates use.
func render(t *testing.T, files []File, values map[string]any) map[string]map[string]any {
	t.Helper()
	var meta struct {
		Name       string `yaml:"name"`
		AppVersion string `yaml:"appVersion"`
	}
	root := template.New("chart").Option("missingkey=error")
	root.Funcs(template.FuncMap{
		"include": func(name string, data any) (string, error) {
			var b strings.Builder
			err := root.ExecuteTemplate(&b, name, data)
			return b.String(), err
		},
		"quote":      func(v any) string { return fmt.Sprintf("%q", fmt.Sprint(v)) },
		"trunc":      func(n int, s string) string { return s[:min(n, len(s))] },
		"trimSuffix": func(suf, s string) string { return strings.TrimSuffix(s, suf) },
		"nindent": func(n int, s string) string {
			pad := strings.Repeat(" ", n)
			return "\n" + pad + strings.ReplaceAll(s, "\n", "\n"+pad)
		},
		"toYaml": func(v any) (string, error) {
			b, err := yaml.Marshal(v)
			return strings.TrimSuffix(string(b), "\n"), err
		},
	})
	var manifests []File
	for _, f := range files {
		switch {
		case strings.HasSuffix(f.Path, "/Chart.yaml"):
			if err := yaml.Unmarshal(f.Content, &meta); err != nil {
				t.Fatal(err)
			}
		case strings.Contains(f.Path, "/templates/"):
			if _, err := root.New(f.Path).Parse(string(f.Content)); err != nil {
				t.Fatalf("%s: %v", f.Path, err)
			}
			if strings.HasSuffix(f.Path, ".yml") {
				manifests = append(manifests, f)
			}
		}
	}
	data := map[string]any{
		"Values":  values,
		"Release": map[string]any{"Name": "orders", "Namespace": "default", "Service": "Helm"},
		"Chart":   map[string]any{"Name": meta.Name, "AppVersion": meta.AppVersion},
	}
	out := map[string]map[string]any{}
	for _, f := range manifests {
		var b bytes.Buffer
		if err := root.ExecuteTemplate(&b, f.Path, data); err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		var obj map[string]any
		if err := yaml.Unmarshal(b.Bytes(), &obj); err != nil {
			t.Fatalf("%s does not parse: %v\n%s", f.Path, err, b.String())
		}
		problems, err := kubeconvert.ValidateObject(obj)
		if err != nil || len(problems) > 0 {
			t.Fatalf("%s: %v %v\n%s", f.Path, err, problems, b.String())
		}
		out[obj["kind"].(string)] = obj
	}
	return out
}

func valuesOf(t *testing.T, files []File) map[string]any {
	t.Helper()
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/values.yaml") {
			var v map[string]any
			if err := yaml.Unmarshal(f.Content, &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatal("no values.yaml")
	return nil
}

func dig(obj map[string]any, path ...any) any {
	var cur any = obj
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := cur.(map[string]any)
			cur = m[k]
		case int:
			s, _ := cur.([]any)
			if k >= len(s) {
				return nil
			}
			cur = s[k]
		}
	}
	return cur
}

func composeService(t *testing.T, in Input) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(Compose(in), &doc); err != nil {
		t.Fatalf("compose.yml does not parse: %v", err)
	}
	svc, ok := dig(doc, "services", Slug(in.Name)).(map[string]any)
	if !ok {
		t.Fatalf("compose.yml has no service: %v", doc)
	}
	return svc
}

// TestPublishedImage: the files pin the publication's tag and digest, expose the one port, probe the health path
// and render to manifests that pass the Kubernetes schemas, including with optional values set.
func TestPublishedImage(t *testing.T) {
	in := input(published, web)
	ref := "registry.example.com/payments/orders-api:2026-10-27.1@" + digest
	svc := composeService(t, in)
	if svc["image"] != ref || svc["pull_policy"] != nil || fmt.Sprint(svc["ports"]) != "[8080:8080]" || svc["healthcheck"] != nil {
		t.Errorf("compose service = %v", svc)
	}
	files := Chart(in)
	values := valuesOf(t, files)
	objs := render(t, files, values)
	c := dig(objs["Deployment"], "spec", "template", "spec", "containers", 0).(map[string]any)
	if c["image"] != ref || c["imagePullPolicy"] != "IfNotPresent" || dig(c, "livenessProbe", "httpGet", "path") != "/healthz" ||
		dig(c, "readinessProbe", "httpGet", "port") != "app" || dig(c, "ports", 0, "containerPort") != 8080 {
		t.Errorf("container = %v", c)
	}
	if dig(objs["Service"], "spec", "ports", 0, "port") != 8080 {
		t.Errorf("service = %v", objs["Service"])
	}
	values["env"] = []any{map[string]any{"name": "LOG_LEVEL", "value": "info"}}
	values["existingSecret"] = "orders-api-env"
	values["imagePullSecrets"] = []any{"registry-credentials"}
	values["resources"] = map[string]any{"limits": map[string]any{"memory": "256Mi"}}
	objs = render(t, files, values)
	c = dig(objs["Deployment"], "spec", "template", "spec", "containers", 0).(map[string]any)
	if dig(c, "envFrom", 0, "secretRef", "name") != "orders-api-env" || dig(c, "resources", "limits", "memory") != "256Mi" {
		t.Errorf("container with values = %v", c)
	}
}

// TestLocalImage: an unpublished image is named as docker load names it, never pulled, and the files say to load it.
func TestLocalImage(t *testing.T) {
	in := input(local, web)
	compose := string(Compose(in))
	svc := composeService(t, in)
	if svc["image"] != "payments/orders-api:2026-10-27.1" || svc["pull_policy"] != "never" || !strings.Contains(compose, "docker load") {
		t.Errorf("compose = %s", compose)
	}
	files := Chart(in)
	objs := render(t, files, valuesOf(t, files))
	c := dig(objs["Deployment"], "spec", "template", "spec", "containers", 0).(map[string]any)
	if c["image"] != "payments/orders-api:2026-10-27.1" || c["imagePullPolicy"] != "Never" {
		t.Errorf("container = %v", c)
	}
	if strings.Contains(compose+fmt.Sprint(files), "registry.example.com") {
		t.Error("an unpublished image named a registry")
	}
}

// TestWorkersAndTasks: a worker has no ports, Service or probes; a command-line task runs as a Job.
func TestWorkersAndTasks(t *testing.T) {
	worker := input(published, &buildspec.Application{Type: "worker", Port: 9000, HealthPath: "/healthz"})
	if svc := composeService(t, worker); svc["ports"] != nil {
		t.Errorf("worker compose = %v", svc)
	}
	files := Chart(worker)
	objs := render(t, files, valuesOf(t, files))
	c := dig(objs["Deployment"], "spec", "template", "spec", "containers", 0).(map[string]any)
	if objs["Service"] != nil || c["ports"] != nil || c["livenessProbe"] != nil {
		t.Errorf("worker chart = %v", objs)
	}
	task := input(local, &buildspec.Application{Type: "cli-task"})
	if svc := composeService(t, task); svc["restart"] != "no" {
		t.Errorf("task compose = %v", svc)
	}
	files = Chart(task)
	objs = render(t, files, valuesOf(t, files))
	if objs["Job"] == nil || objs["Deployment"] != nil || dig(objs["Job"], "spec", "template", "spec", "restartPolicy") != "Never" {
		t.Errorf("task chart = %v", objs)
	}
	noSpec := input(local, nil)
	files = Chart(noSpec)
	if objs := render(t, files, valuesOf(t, files)); objs["Deployment"] == nil || objs["Service"] != nil {
		t.Errorf("chart without an application = %v", objs)
	}
}

// TestChartArchive: the .tgz is deterministic and holds the files helm expects under the chart's folder.
func TestChartArchive(t *testing.T) {
	in := input(published, web)
	var a, b bytes.Buffer
	if err := WriteChart(&a, in); err != nil {
		t.Fatal(err)
	}
	if err := WriteChart(&b, in); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("the archive is not deterministic")
	}
	zr, err := gzip.NewReader(&a)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var names []string
	var chart []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Name == "orders-api/Chart.yaml" {
			chart, _ = io.ReadAll(tr)
		}
	}
	want := "orders-api/Chart.yaml orders-api/values.yaml orders-api/templates/_helpers.tpl orders-api/templates/deployment.yml orders-api/templates/service.yml orders-api/templates/NOTES.txt"
	if strings.Join(names, " ") != want {
		t.Errorf("archive = %v", names)
	}
	var meta map[string]any
	if err := yaml.Unmarshal(chart, &meta); err != nil || meta["apiVersion"] != "v2" || meta["name"] != "orders-api" || meta["version"] != "2026.1027.1" || meta["appVersion"] != "2026-10-27.1" {
		t.Errorf("Chart.yaml = %v %v", meta, err)
	}
	if ChartFilename(in) != "orders-api-2026.1027.1.tgz" {
		t.Errorf("filename = %s", ChartFilename(in))
	}
	for v, want := range map[string]string{"2026-01-05.12": "2026.105.12", "samuori-4f53cda18c2b": "0.1.0+samuori-4f53cda18c2b", "": "0.1.0", "a b..c": "0.1.0+a-b.c"} {
		if got := ChartVersion(v); got != want {
			t.Errorf("ChartVersion(%q) = %s, want %s", v, got, want)
		}
	}
}
