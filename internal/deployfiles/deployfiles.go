// Package deployfiles writes starting Compose files and Helm charts that reference built images, one image or every
// image of a project (ADRs 0059, 0061). The files carry references and port, probe and placeholder settings from the
// BuildSpec, never secret values.
package deployfiles

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/buildspec"
)

// Image is the reference the files use: a publication's registry location pinned by digest, or the local name
// docker load gives the downloaded archive.
type Image struct {
	Repository string
	Tag        string
	// Digest is the manifest digest a publication recorded; empty for a local image.
	Digest    string
	Published bool
}

func (i Image) Reference() string {
	ref := i.Repository
	if i.Tag != "" {
		ref += ":" + i.Tag
	}
	if i.Digest != "" {
		ref += "@" + i.Digest
	}
	return ref
}

// Input is one built image.
type Input struct {
	Name, Version string
	App           *buildspec.Application
	Image         Image
	// ModTime stamps archive entries, so the same build always yields the same bytes.
	ModTime time.Time
}

// Component is one image of a project. Name is a slug unique within the project.
type Component struct {
	Name     string
	App      *buildspec.Application
	Image    Image
	Replicas int
	Links    []Link
	Volumes  []Volume
}

// Link sets Env to the in-network URL of the component named Target.
type Link struct{ Env, Target string }

// Volume is persistent storage mounted at MountPath.
type Volume struct{ Name, MountPath string }

// Project is every image of a project that has a successful build.
type Project struct {
	Name, Version string
	Components    []Component
	// Skipped explains each image left out, such as one with no successful build.
	Skipped []string
	ModTime time.Time
}

type File struct {
	Path    string
	Content []byte
}

const (
	MaxReplicas = 20
	maxLinks    = 20
	maxVolumes  = 10
)

var (
	slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)
	dailyVer   = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})\.(\d+)$`)
	metaUnsafe = regexp.MustCompile(`[^0-9A-Za-z.-]+`)
	metaDots   = regexp.MustCompile(`\.{2,}`)
	envName    = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	volumeName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	mountPath  = regexp.MustCompile(`^(/[A-Za-z0-9._@+-]+)+$`)
)

// Slug is a name Compose services, Helm charts and Kubernetes labels all accept.
func Slug(name string) string {
	s := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 53 {
		s = strings.TrimRight(s[:53], "-")
	}
	if s == "" {
		return "app"
	}
	return s
}

// ChartVersion turns a build version such as 2026-10-27.3 into the semver 2026.1027.3, which keeps builds in order;
// any other version becomes build metadata of 0.1.0.
func ChartVersion(version string) string {
	if m := dailyVer.FindStringSubmatch(version); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		n, _ := strconv.Atoi(m[4])
		return fmt.Sprintf("%d.%d.%d", y, mo*100+d, n)
	}
	meta := strings.Trim(metaDots.ReplaceAllString(metaUnsafe.ReplaceAllString(version, "-"), "."), ".")
	if meta == "" {
		return "0.1.0"
	}
	return "0.1.0+" + meta
}

func (c Component) appType() string {
	if c.App == nil {
		return ""
	}
	return c.App.Type
}

// serves reports whether the image listens on a port others reach; workers and command-line tasks do not.
func (c Component) serves() bool {
	t := c.appType()
	return c.App != nil && c.App.Port > 0 && t != "worker" && t != "cli-task"
}

func (c Component) httpProbe() bool {
	return c.serves() && c.App.HealthPath != "" && c.appType() != "database"
}

// Linkable reports whether other components can reach this one over HTTP.
func (c Component) Linkable() bool {
	t := c.appType()
	return c.serves() && (t == "http-service" || t == "static-site")
}

func (c Component) job() bool { return c.appType() == "cli-task" }

// ValidMountPath accepts a clean absolute path other than / made of characters Compose and Kubernetes take as is.
func ValidMountPath(p string) bool {
	if len(p) > 256 || !mountPath.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Validate checks the replicas, links and volumes people choose.
func (p Project) Validate() error {
	byName := map[string]Component{}
	for _, c := range p.Components {
		byName[c.Name] = c
	}
	for _, c := range p.Components {
		if c.Replicas < 0 || c.Replicas > MaxReplicas {
			return fmt.Errorf("%s: replicas must be between 0 and %d", c.Name, MaxReplicas)
		}
		if len(c.Links) > maxLinks || len(c.Volumes) > maxVolumes {
			return fmt.Errorf("%s: at most %d links and %d volumes", c.Name, maxLinks, maxVolumes)
		}
		envs := map[string]bool{}
		for _, l := range c.Links {
			if !envName.MatchString(l.Env) {
				return fmt.Errorf("%s: %q is not an environment variable name (upper-case letters, digits and underscores, not starting with a digit)", c.Name, l.Env)
			}
			if envs[l.Env] {
				return fmt.Errorf("%s: %s is set twice", c.Name, l.Env)
			}
			envs[l.Env] = true
			if t, ok := byName[l.Target]; !ok || !t.Linkable() {
				return fmt.Errorf("%s: %s must point at an HTTP image with a port in these files", c.Name, l.Env)
			}
		}
		names, paths := map[string]bool{}, map[string]bool{}
		for _, v := range c.Volumes {
			if !volumeName.MatchString(v.Name) {
				return fmt.Errorf("%s: volume name %q must be lower-case letters, digits and dashes, at most 32 characters", c.Name, v.Name)
			}
			if !ValidMountPath(v.MountPath) {
				return fmt.Errorf("%s: mount path %q must be a clean absolute path other than /", c.Name, v.MountPath)
			}
			if names[v.Name] || paths[v.MountPath] {
				return fmt.Errorf("%s: volume %s or its mount path is used twice", c.Name, v.Name)
			}
			names[v.Name], paths[v.MountPath] = true, true
		}
	}
	return nil
}

func q(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func (in Input) component() Component {
	return Component{Name: Slug(in.Name), App: in.App, Image: in.Image, Replicas: 1}
}

func imageNote(prefix string, img Image) string {
	if img.Published {
		return fmt.Sprintf("# %sThe image is published at %s with tag %s and pinned to digest %s.\n", prefix, img.Repository, img.Tag, img.Digest)
	}
	return fmt.Sprintf("# %sThe image is not published to a registry. Download it from Samuori and load it first (docker load -i <file>.tar);\n# it is then named %s.\n", prefix, img.Reference())
}

func (in Input) header(kind string) string {
	return fmt.Sprintf("# Starting %s for %s %s, generated by Samuori. Review it before use; Samuori does not deploy anything.\n", kind, oneLine(in.Name), oneLine(in.Version)) +
		imageNote("", in.Image)
}

func (p Project) header(kind string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Starting %s for project %s, generated by Samuori. Review it before use; Samuori does not deploy anything.\n", kind, oneLine(p.Name))
	for _, c := range p.Components {
		b.WriteString(imageNote(c.Name+": ", c.Image))
	}
	for _, s := range p.Skipped {
		fmt.Fprintf(&b, "# Left out: %s\n", oneLine(s))
	}
	return b.String()
}

// Compose is a compose.yml with one service running the image.
func Compose(in Input) []byte {
	var b strings.Builder
	b.WriteString(in.header("Compose file"))
	b.WriteString("services:\n")
	in.component().composeService(&b, nil, true)
	return []byte(b.String())
}

// ProjectCompose is a compose.yml with one service per component.
func ProjectCompose(p Project) []byte {
	var b strings.Builder
	b.WriteString(p.header("Compose file"))
	b.WriteString("services:\n")
	byName := map[string]Component{}
	for _, c := range p.Components {
		byName[c.Name] = c
	}
	usedPorts := map[int]bool{}
	var volumes []string
	for _, c := range p.Components {
		hostPort := c.serves() && c.Replicas <= 1 && !usedPorts[c.App.Port]
		if hostPort {
			usedPorts[c.App.Port] = true
		}
		c.composeService(&b, byName, hostPort)
		for _, v := range c.Volumes {
			volumes = append(volumes, c.Name+"-"+v.Name)
		}
	}
	if len(volumes) > 0 {
		b.WriteString("volumes:\n")
		for _, v := range volumes {
			fmt.Fprintf(&b, "  %s: {}\n", v)
		}
	}
	return []byte(b.String())
}

// composeService writes one service. byName is nil for a single image; hostPort publishes the container port on the
// same host port, otherwise Docker picks one so replicas and components do not collide.
func (c Component) composeService(b *strings.Builder, byName map[string]Component, hostPort bool) {
	fmt.Fprintf(b, "  %s:\n    image: %s\n", c.Name, q(c.Image.Reference()))
	if !c.Image.Published {
		b.WriteString("    pull_policy: never\n")
	}
	if c.job() {
		b.WriteString("    restart: \"no\"\n")
	} else {
		b.WriteString("    restart: unless-stopped\n")
	}
	if c.serves() {
		if hostPort {
			fmt.Fprintf(b, "    ports:\n      - %s\n", q(fmt.Sprintf("%d:%d", c.App.Port, c.App.Port)))
		} else {
			fmt.Fprintf(b, "    # Docker picks the host port (docker compose port %s %d shows it), so replicas and other services do not collide.\n", c.Name, c.App.Port)
			fmt.Fprintf(b, "    ports:\n      - %s\n", q(strconv.Itoa(c.App.Port)))
		}
		if c.httpProbe() {
			fmt.Fprintf(b, "    # No healthcheck: Compose runs it inside the container and Samuori cannot assume the image has curl or wget.\n    # The application answers %s on port %d.\n", c.App.HealthPath, c.App.Port)
		}
	}
	if byName != nil && c.Replicas != 1 && !c.job() {
		fmt.Fprintf(b, "    # docker compose up starts this many containers of the service.\n    deploy:\n      replicas: %d\n", c.Replicas)
	}
	if len(c.Links) > 0 {
		b.WriteString("    environment:\n")
		for _, l := range c.Links {
			t := byName[l.Target]
			fmt.Fprintf(b, "      %s: %s\n", l.Env, q(fmt.Sprintf("http://%s:%d", t.Name, t.App.Port)))
		}
		b.WriteString("    # Add the other variables the application reads. Keep secret values out of this file, for example in an env file\n" +
			"    # that is not committed.\n    # env_file: .env\n")
	} else {
		b.WriteString("    # The BuildSpec declares no runtime environment variables; add the ones the application reads.\n" +
			"    # environment:\n    #   NAME: \"\"\n" +
			"    # Keep secret values out of this file, for example in an env file that is not committed.\n" +
			"    # env_file: .env\n")
	}
	if len(c.Volumes) > 0 {
		b.WriteString("    volumes:\n")
		for _, v := range c.Volumes {
			fmt.Fprintf(b, "      - %s\n", q(c.Name+"-"+v.Name+":"+v.MountPath))
		}
	}
}

// chartScope is one component's place in a chart: the template expression for its values and resource name.
type chartScope struct {
	chart   string
	c       Component
	project bool
	byName  map[string]Component
}

func (s chartScope) values() string {
	if s.project {
		return fmt.Sprintf("(index .Values %q)", s.c.Name)
	}
	return ".Values"
}

func releaseName(suffix string) string {
	return fmt.Sprintf(`{{ printf "%%s-%%s" $.Release.Name %q | trunc 63 | trimSuffix "-" }}`, suffix)
}

func (s chartScope) name() string {
	if s.project {
		return releaseName(s.c.Name)
	}
	return fmt.Sprintf(`{{ include "%s.fullname" . }}`, s.chart)
}

func (s chartScope) container() string {
	if s.project {
		return s.c.Name
	}
	return "{{ .Chart.Name }}"
}

func (s chartScope) labels(indent int, selector bool) string {
	helper := "labels"
	if selector {
		helper = "selectorLabels"
	}
	out := fmt.Sprintf("{{- include \"%s.%s\" . | nindent %d }}", s.chart, helper, indent)
	if s.project {
		out += "\n" + strings.Repeat(" ", indent) + "app.kubernetes.io/component: " + s.c.Name
	}
	return out
}

// Chart is a Helm chart's files under <name>/, in a fixed order.
func Chart(in Input) []File {
	name := Slug(in.Name)
	s := chartScope{chart: name, c: in.component()}
	var values strings.Builder
	values.WriteString(in.header("Helm values"))
	values.WriteString("# Names of existing Secrets holding registry credentials, if the registry needs them.\nimagePullSecrets: []\n")
	s.c.writeValues(&values, "", false)
	files := []File{
		{name + "/Chart.yaml", chartMeta(name, "Starting chart for "+in.Name, in.Version)},
		{name + "/values.yaml", []byte(values.String())},
		{name + "/templates/_helpers.tpl", []byte(helpers(name))},
	}
	files = append(files, s.templates("")...)
	files = append(files, File{name + "/templates/NOTES.txt", []byte(s.notes())})
	return files
}

// ProjectChart is one Helm chart with a workload, and a Service where it serves, per component.
func ProjectChart(p Project) []File {
	name := Slug(p.Name)
	byName := map[string]Component{}
	for _, c := range p.Components {
		byName[c.Name] = c
	}
	var values, notes strings.Builder
	values.WriteString(p.header("Helm values"))
	values.WriteString("# Names of existing Secrets holding registry credentials, if a registry needs them.\nimagePullSecrets: []\n")
	files := []File{
		{name + "/Chart.yaml", chartMeta(name, "Starting chart for project "+p.Name, p.Version)},
		{name + "/values.yaml", nil},
		{name + "/templates/_helpers.tpl", []byte(helpers(name))},
	}
	for _, c := range p.Components {
		s := chartScope{chart: name, c: c, project: true, byName: byName}
		fmt.Fprintf(&values, "%s:\n", c.Name)
		c.writeValues(&values, "  ", true)
		files = append(files, s.templates(c.Name+"-")...)
		notes.WriteString(s.notes())
	}
	files[1].Content = []byte(values.String())
	files = append(files, File{name + "/templates/NOTES.txt", []byte(notes.String())})
	return files
}

func chartMeta(name, description, version string) []byte {
	return []byte(fmt.Sprintf("apiVersion: v2\nname: %s\ndescription: %s\ntype: application\nversion: %s\nappVersion: %s\n",
		name, q(description+", generated by Samuori. Review it before use."), ChartVersion(version), q(version)))
}

func (s chartScope) templates(prefix string) []File {
	dir := s.chart + "/templates/" + prefix
	var files []File
	if s.c.job() {
		files = append(files, File{dir + "job.yml", []byte(s.workload("Job"))})
	} else {
		files = append(files, File{dir + "deployment.yml", []byte(s.workload("Deployment"))})
	}
	if s.c.serves() {
		files = append(files, File{dir + "service.yml", []byte(s.service())})
	}
	for _, v := range s.c.Volumes {
		files = append(files, File{dir + v.Name + "-pvc.yml", []byte(s.claim(v))})
	}
	return files
}

func (c Component) writeValues(b *strings.Builder, ind string, project bool) {
	pull := "IfNotPresent"
	if !c.Image.Published {
		pull = "Never"
		b.WriteString(ind + "# Kubernetes nodes need the image loaded, for example with kind load image-archive or ctr -n k8s.io images import.\n" +
			ind + "# pullPolicy Never keeps the node from pulling a different image of the same name from a public registry.\n")
	}
	fmt.Fprintf(b, "%[1]simage:\n%[1]s  repository: %[2]s\n%[1]s  tag: %[3]s\n%[1]s  digest: %[4]s\n%[1]s  pullPolicy: %[5]s\n", ind, q(c.Image.Repository), q(c.Image.Tag), q(c.Image.Digest), pull)
	if !c.job() {
		fmt.Fprintf(b, "%sreplicaCount: %d\n", ind, c.Replicas)
	}
	if c.serves() {
		fmt.Fprintf(b, "%[1]scontainerPort: %[2]d\n%[1]sservice:\n%[1]s  type: ClusterIP\n%[1]s  port: %[2]d\n", ind, c.App.Port)
	}
	if c.httpProbe() {
		fmt.Fprintf(b, "%sprobes:\n%s  path: %s\n", ind, ind, q(c.App.HealthPath))
	}
	if len(c.Links) > 0 {
		b.WriteString(ind + "# The chart sets the linked URLs; add the other variables the application reads, such as:\n")
	} else {
		b.WriteString(ind + "# The BuildSpec declares no runtime environment variables; add the ones the application reads, such as:\n")
	}
	b.WriteString(ind + "# - name: NAME\n" + ind + "#   value: \"\"\n" + ind + "env: []\n" +
		ind + "# Name of an existing Secret whose keys become environment variables. Samuori never puts secret values here.\n" +
		ind + "existingSecret: \"\"\n" +
		ind + "# The BuildSpec sets no resource requests or limits; set them for your cluster.\n" + ind + "resources: {}\n")
	if len(c.Volumes) > 0 {
		b.WriteString(ind + "# Set each size, such as 1Gi, before installing; an empty storageClass uses the cluster's default.\n" + ind + "persistence:\n")
		for _, v := range c.Volumes {
			fmt.Fprintf(b, "%[1]s  %[2]s:\n%[1]s    mountPath: %[3]s\n%[1]s    storageClass: \"\"\n%[1]s    size: \"\"\n", ind, v.Name, q(v.MountPath))
		}
	}
}

func helpers(name string) string {
	return fmt.Sprintf(`{{- define "%[1]s.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "%[1]s.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "%[1]s.labels" -}}
{{ include "%[1]s.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "%[1]s.image" -}}
{{ .repository }}{{ with .tag }}:{{ . }}{{ end }}{{ with .digest }}@{{ . }}{{ end }}
{{- end -}}
`, name)
}

func (s chartScope) workload(kind string) string {
	var b strings.Builder
	api := "apps/v1"
	if kind == "Job" {
		api = "batch/v1"
	}
	fmt.Fprintf(&b, `{{- $v := %s -}}
apiVersion: %s
kind: %s
metadata:
  name: %s
  labels:
    %s
spec:
`, s.values(), api, kind, s.name(), s.labels(4, false))
	if kind == "Deployment" {
		fmt.Fprintf(&b, `  replicas: {{ $v.replicaCount }}
  selector:
    matchLabels:
      %s
`, s.labels(6, true))
	}
	fmt.Fprintf(&b, `  template:
    metadata:
      labels:
        %s
    spec:
`, s.labels(8, true))
	if kind == "Job" {
		b.WriteString("      restartPolicy: Never\n")
	}
	fmt.Fprintf(&b, `      {{- with .Values.imagePullSecrets }}
      imagePullSecrets:
        {{- range . }}
        - name: {{ . | quote }}
        {{- end }}
      {{- end }}
      containers:
        - name: %s
          image: {{ include "%s.image" $v.image | quote }}
          imagePullPolicy: {{ $v.image.pullPolicy }}
`, s.container(), s.chart)
	if s.c.serves() {
		b.WriteString(`          ports:
            - name: app
              containerPort: {{ $v.containerPort }}
              protocol: TCP
`)
	}
	if s.c.httpProbe() {
		for _, p := range []string{"livenessProbe", "readinessProbe"} {
			fmt.Fprintf(&b, `          %s:
            httpGet:
              path: {{ $v.probes.path | quote }}
              port: app
`, p)
		}
	}
	if len(s.c.Links) > 0 {
		b.WriteString("          env:\n")
		for _, l := range s.c.Links {
			fmt.Fprintf(&b, "            - name: %s\n              value: \"http://%s:{{ (index $.Values %q).service.port }}\"\n", l.Env, releaseName(l.Target), l.Target)
		}
		b.WriteString(`            {{- with $v.env }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
`)
	} else {
		b.WriteString(`          {{- with $v.env }}
          env:
            {{- toYaml . | nindent 12 }}
          {{- end }}
`)
	}
	b.WriteString(`          {{- with $v.existingSecret }}
          envFrom:
            - secretRef:
                name: {{ . | quote }}
          {{- end }}
          {{- with $v.resources }}
          resources:
            {{- toYaml . | nindent 12 }}
          {{- end }}
`)
	if len(s.c.Volumes) > 0 {
		b.WriteString("          volumeMounts:\n")
		for _, v := range s.c.Volumes {
			fmt.Fprintf(&b, "            - name: %s\n              mountPath: {{ (index $v.persistence %q).mountPath | quote }}\n", v.Name, v.Name)
		}
		b.WriteString("      volumes:\n")
		for _, v := range s.c.Volumes {
			fmt.Fprintf(&b, "        - name: %s\n          persistentVolumeClaim:\n            claimName: %s\n", v.Name, releaseName(s.c.Name+"-"+v.Name))
		}
	}
	return b.String()
}

func (s chartScope) service() string {
	return fmt.Sprintf(`{{- $v := %s -}}
apiVersion: v1
kind: Service
metadata:
  name: %s
  labels:
    %s
spec:
  type: {{ $v.service.type }}
  selector:
    %s
  ports:
    - name: app
      port: {{ $v.service.port }}
      targetPort: app
      protocol: TCP
`, s.values(), s.name(), s.labels(4, false), s.labels(4, true))
}

// claim is a PersistentVolumeClaim; helm install stops until its size is set.
func (s chartScope) claim(v Volume) string {
	return fmt.Sprintf(`{{- $p := index %s.persistence %q -}}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  labels:
    %s
spec:
  accessModes:
    - ReadWriteOnce
  {{- with $p.storageClass }}
  storageClassName: {{ . | quote }}
  {{- end }}
  resources:
    requests:
      storage: {{ required %q $p.size | quote }}
`, s.values(), v.Name, releaseName(s.c.Name+"-"+v.Name), s.labels(4, false), "set "+s.c.Name+".persistence."+v.Name+".size, such as 1Gi")
}

func (s chartScope) notes() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s runs image {{ include %q %s.image }}.\n", s.c.Name, s.chart+".image", s.values())
	if !s.c.Image.Published {
		b.WriteString("The image is not in a registry: load it into the cluster's nodes before installing.\n")
	}
	if s.c.serves() {
		fmt.Fprintf(&b, "To reach it from this machine:\n  kubectl port-forward --namespace {{ .Release.Namespace }} service/%s %d:{{ %s.service.port }}\n", s.name(), s.c.App.Port, s.values())
	}
	return b.String()
}

// WriteChart writes the chart as the .tgz archive helm package produces.
func WriteChart(w io.Writer, in Input) error { return writeArchive(w, Chart(in), in.ModTime) }

// WriteProjectChart writes the project's chart as a .tgz archive.
func WriteProjectChart(w io.Writer, p Project) error {
	return writeArchive(w, ProjectChart(p), p.ModTime)
}

func writeArchive(w io.Writer, files []File, modTime time.Time) error {
	zw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	at := modTime.UTC().Truncate(time.Second)
	zw.ModTime = at
	tw := tar.NewWriter(zw)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Path, Mode: 0o644, Size: int64(len(f.Content)), ModTime: at, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := io.Copy(tw, bytes.NewReader(f.Content)); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// ChartFilename is the archive name helm package would give the chart.
func ChartFilename(in Input) string {
	return Slug(in.Name) + "-" + ChartVersion(in.Version) + ".tgz"
}

// ProjectChartFilename is the archive name helm package would give the project's chart.
func ProjectChartFilename(p Project) string {
	return Slug(p.Name) + "-" + ChartVersion(p.Version) + ".tgz"
}
