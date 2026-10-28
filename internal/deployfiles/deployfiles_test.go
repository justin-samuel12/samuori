package deployfiles

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
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

// execChart executes the chart's templates the way helm template does, with the functions the templates use, and
// checks each manifest against the Kubernetes schemas.
func execChart(files []File, values map[string]any) ([]map[string]any, error) {
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
		"required": func(msg string, v any) (any, error) {
			if v == nil || v == "" {
				return nil, errors.New(msg)
			}
			return v, nil
		},
	})
	var manifests []File
	for _, f := range files {
		switch {
		case strings.HasSuffix(f.Path, "/Chart.yaml"):
			if err := yaml.Unmarshal(f.Content, &meta); err != nil {
				return nil, err
			}
		case strings.Contains(f.Path, "/templates/"):
			if _, err := root.New(f.Path).Parse(string(f.Content)); err != nil {
				return nil, fmt.Errorf("%s: %w", f.Path, err)
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
	var out []map[string]any
	for _, f := range manifests {
		var b bytes.Buffer
		if err := root.ExecuteTemplate(&b, f.Path, data); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		var obj map[string]any
		if err := yaml.Unmarshal(b.Bytes(), &obj); err != nil {
			return nil, fmt.Errorf("%s does not parse: %w\n%s", f.Path, err, b.String())
		}
		problems, err := kubeconvert.ValidateObject(obj)
		if err != nil || len(problems) > 0 {
			return nil, fmt.Errorf("%s: %v %v\n%s", f.Path, err, problems, b.String())
		}
		out = append(out, obj)
	}
	return out, nil
}

// render executes the chart and returns its objects by kind.
func render(t *testing.T, files []File, values map[string]any) map[string]map[string]any {
	t.Helper()
	objs, err := execChart(files, values)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, o := range objs {
		out[o["kind"].(string)] = o
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

func project() Project {
	return Project{Name: "shop", Version: "2026-10-28.2", ModTime: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC),
		Components: []Component{
			{Name: "web", App: web, Image: published, Replicas: 3, Links: []Link{{Env: "API_URL", Target: "api"}}},
			{Name: "api", App: &buildspec.Application{Type: "http-service", Port: 8080}, Image: local, Replicas: 1},
			{Name: "worker", App: &buildspec.Application{Type: "worker"}, Image: local, Replicas: 2,
				Links: []Link{{Env: "API_URL", Target: "api"}}, Volumes: []Volume{{Name: "data", MountPath: "/var/lib/worker"}}},
		},
		Skipped: []string{"report has no successful build."}}
}

// TestProjectCompose: one service per component, links set the target's service URL, replicas and volumes as asked,
// and two services on one port do not both take the host port.
func TestProjectCompose(t *testing.T) {
	p := project()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	out := ProjectCompose(p)
	if !bytes.Equal(out, ProjectCompose(p)) {
		t.Error("the Compose file is not deterministic")
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("compose.yml does not parse: %v\n%s", err, out)
	}
	w, a, k := dig(doc, "services", "web"), dig(doc, "services", "api"), dig(doc, "services", "worker")
	if dig(doc, "services", "web", "image") != published.Reference() || dig(doc, "services", "web", "deploy", "replicas") != 3 ||
		dig(doc, "services", "web", "environment", "API_URL") != "http://api:8080" || fmt.Sprint(dig(doc, "services", "web", "ports")) != "[8080]" {
		t.Errorf("web = %v", w)
	}
	if fmt.Sprint(dig(doc, "services", "api", "ports")) != "[8080:8080]" || dig(doc, "services", "api", "pull_policy") != "never" || dig(doc, "services", "api", "deploy") != nil {
		t.Errorf("api = %v", a)
	}
	if dig(doc, "services", "worker", "ports") != nil || fmt.Sprint(dig(doc, "services", "worker", "volumes")) != "[worker-data:/var/lib/worker]" ||
		dig(doc, "services", "worker", "deploy", "replicas") != 2 || dig(doc, "services", "worker", "environment", "API_URL") != "http://api:8080" {
		t.Errorf("worker = %v", k)
	}
	if _, ok := dig(doc, "volumes", "worker-data").(map[string]any); !ok || !strings.Contains(string(out), "# Left out: report has no successful build.") {
		t.Errorf("compose = %s", out)
	}
}

// TestProjectChart: the chart renders a Deployment and Service for each HTTP image, a Deployment without a Service
// for the worker, a claim for its volume that needs a size, and link URLs that name the target's Service.
func TestProjectChart(t *testing.T) {
	p := project()
	files := ProjectChart(p)
	values := valuesOf(t, files)
	if _, err := execChart(files, values); err == nil || !strings.Contains(err.Error(), "set worker.persistence.data.size") {
		t.Errorf("a claim without a size rendered: %v", err)
	}
	dig(values, "worker", "persistence", "data").(map[string]any)["size"] = "1Gi"
	list, err := execChart(files, values)
	if err != nil {
		t.Fatal(err)
	}
	objs := map[string]map[string]any{}
	var names []string
	for _, o := range list {
		k := o["kind"].(string) + "/" + dig(o, "metadata", "name").(string)
		objs[k] = o
		names = append(names, k)
	}
	want := "Deployment/orders-web Service/orders-web Deployment/orders-api Service/orders-api Deployment/orders-worker PersistentVolumeClaim/orders-worker-data"
	if strings.Join(names, " ") != want {
		t.Fatalf("objects = %v", names)
	}
	webC := dig(objs["Deployment/orders-web"], "spec", "template", "spec", "containers", 0).(map[string]any)
	if dig(objs["Deployment/orders-web"], "spec", "replicas") != 3 || dig(webC, "env", 0, "name") != "API_URL" || dig(webC, "env", 0, "value") != "http://orders-api:8080" ||
		webC["image"] != published.Reference() || dig(webC, "livenessProbe", "httpGet", "path") != "/healthz" {
		t.Errorf("web = %v", webC)
	}
	if dig(objs["Service/orders-api"], "spec", "selector", "app.kubernetes.io/component") != "api" ||
		dig(objs["Deployment/orders-api"], "spec", "selector", "matchLabels", "app.kubernetes.io/component") != "api" {
		t.Errorf("api = %v", objs["Service/orders-api"])
	}
	worker := dig(objs["Deployment/orders-worker"], "spec", "template", "spec").(map[string]any)
	if dig(worker, "containers", 0, "volumeMounts", 0, "mountPath") != "/var/lib/worker" || dig(worker, "volumes", 0, "persistentVolumeClaim", "claimName") != "orders-worker-data" ||
		dig(worker, "containers", 0, "imagePullPolicy") != "Never" || dig(worker, "containers", 0, "ports") != nil ||
		dig(objs["PersistentVolumeClaim/orders-worker-data"], "spec", "resources", "requests", "storage") != "1Gi" {
		t.Errorf("worker = %v", worker)
	}
	dig(values, "web").(map[string]any)["env"] = []any{map[string]any{"name": "LOG_LEVEL", "value": "info"}}
	list, err = execChart(files, values)
	if err != nil || dig(list[0], "spec", "template", "spec", "containers", 0, "env", 1, "name") != "LOG_LEVEL" {
		t.Errorf("extra env with links = %v %v", err, list)
	}
	var a, b bytes.Buffer
	if err := WriteProjectChart(&a, p); err != nil {
		t.Fatal(err)
	}
	_ = WriteProjectChart(&b, p)
	if !bytes.Equal(a.Bytes(), b.Bytes()) || ProjectChartFilename(p) != "shop-2026.1028.2.tgz" {
		t.Errorf("archive not deterministic or misnamed: %s", ProjectChartFilename(p))
	}
}

// TestProjectValidate: links need a valid name and an HTTP target with a port; volumes a clean absolute path.
func TestProjectValidate(t *testing.T) {
	for name, change := range map[string]func(*Project){
		"lower-case env":     func(p *Project) { p.Components[0].Links[0].Env = "api_url" },
		"env starting digit": func(p *Project) { p.Components[0].Links[0].Env = "1URL" },
		"duplicate env":      func(p *Project) { p.Components[0].Links = append(p.Components[0].Links, p.Components[0].Links[0]) },
		"worker target":      func(p *Project) { p.Components[0].Links[0].Target = "worker" },
		"unknown target":     func(p *Project) { p.Components[0].Links[0].Target = "report" },
		"relative mount":     func(p *Project) { p.Components[2].Volumes[0].MountPath = "var/lib" },
		"root mount":         func(p *Project) { p.Components[2].Volumes[0].MountPath = "/" },
		"dot-dot mount":      func(p *Project) { p.Components[2].Volumes[0].MountPath = "/var/../etc" },
		"colon mount":        func(p *Project) { p.Components[2].Volumes[0].MountPath = "/data:rw" },
		"volume name":        func(p *Project) { p.Components[2].Volumes[0].Name = "Data" },
		"too many replicas":  func(p *Project) { p.Components[1].Replicas = MaxReplicas + 1 },
		"negative replicas":  func(p *Project) { p.Components[1].Replicas = -1 },
	} {
		p := project()
		change(&p)
		if p.Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
