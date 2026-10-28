package kubeconvert

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ImageEvidence is what Samuori's records show about a Samuori image: its digest, where it is published, and the runtime
// check of its build. Probes are added only from this evidence.
type ImageEvidence struct {
	Service    string
	Reference  string
	Digest     string
	BuildRunID string
	Profile    string
	Port       int
	Path       string
}

// PostContext is what post-processing may use besides the files: the user's options and Samuori's image records.
type PostContext struct {
	Name    string
	Options Options
	Image   *ImageEvidence
	// Review re-checks edited files only: options and image records were applied when the revision was generated.
	Review bool
}

type object struct {
	file    int
	obj     map[string]any
	changed bool
}

func (o object) ref() string {
	return fmt.Sprintf("%s/%s", str(o.obj["kind"]), str(dig(o.obj, "metadata", "name")))
}

type processor struct {
	ctx      PostContext
	files    []File
	parsed   [][]*object
	findings []Finding
	source   string
}

// Finding sources: provider findings and option changes belong to the generated revision; policy and schema
// findings are recomputed whenever the files change.
const (
	SourceProvider = "provider"
	SourceOptions  = "options"
	SourcePolicy   = "policy"
	SourceSchema   = "schema"
)

func (p *processor) add(sev, code, file, obj, field, format string, args ...any) {
	p.findings = append(p.findings, Finding{Severity: sev, Code: code, File: file, Object: obj, Field: field, Source: p.source, Message: fmt.Sprintf(format, args...)})
}

// Process parses every document, applies the user's options and Samuori's image records, replaces literal secret values
// with Secret references, reports what Samuori policy forbids or needs reviewed, and validates every object against the
// targeted Kubernetes schemas. It never executes anything and never drops a security-relevant setting silently.
func Process(ctx PostContext, in []File) ([]File, []Finding) {
	p := &processor{ctx: ctx, files: append([]File(nil), in...), source: SourceSchema}
	p.parse()
	if !ctx.Review {
		p.source = SourceOptions
		p.apply()
	}
	p.source = SourcePolicy
	for _, objs := range p.parsed {
		for _, o := range objs {
			p.inspect(o)
		}
	}
	if !ctx.Review {
		p.source = SourceOptions
		p.extras()
	}
	p.source = SourceSchema
	for _, objs := range p.parsed {
		for _, o := range objs {
			p.validate(o)
		}
	}
	p.serialize()
	p.rename()
	sort.SliceStable(p.findings, func(i, j int) bool {
		return severityRank(p.findings[i].Severity) < severityRank(p.findings[j].Severity)
	})
	return p.files, p.findings
}

func severityRank(s string) int {
	return map[string]int{SeverityError: 0, SeverityReview: 1, SeverityWarning: 2, SeverityInfo: 3}[s]
}

// helmMeta names Helm's own files, which keep the names Helm requires: Chart.yaml and values files, including the
// per-environment values-<env>.yaml Move2Kube writes.
func helmMeta(name string) bool {
	b := path.Base(name)
	return b == "Chart.yaml" || b == "values.yaml" || (strings.HasPrefix(b, "values-") && strings.HasSuffix(b, ".yaml"))
}

func kustomization(name string) bool {
	b := path.Base(name)
	return b == "kustomization.yaml" || b == "kustomization.yml" || b == "Kustomization"
}

func manifestFile(name string) bool {
	ext := path.Ext(name)
	return (ext == ".yaml" || ext == ".yml" || ext == ".json") && !helmMeta(name)
}

// patches lists the files kustomizations name as patches; they are partial objects or JSON patches, not objects.
func (p *processor) patches() map[string]bool {
	out := map[string]bool{}
	for _, f := range p.files {
		if !kustomization(f.Path) {
			continue
		}
		var k struct {
			Patches []struct {
				Path string `yaml:"path"`
			} `yaml:"patches"`
			PatchesStrategicMerge []string `yaml:"patchesStrategicMerge"`
		}
		if yaml.Unmarshal([]byte(f.Content), &k) != nil {
			continue
		}
		names := k.PatchesStrategicMerge
		for _, pt := range k.Patches {
			names = append(names, pt.Path)
		}
		for _, n := range names {
			if n != "" {
				out[path.Clean(path.Join(path.Dir(f.Path), n))] = true
			}
		}
	}
	return out
}

func (p *processor) parse() {
	p.parsed = make([][]*object, len(p.files))
	patches := p.patches()
	for i, f := range p.files {
		if !manifestFile(f.Path) || kustomization(f.Path) {
			continue
		}
		if patches[f.Path] {
			p.add(SeverityInfo, "kustomize-patch", f.Path, "", "", "a Kustomize patch; Samuori validates the objects it patches, not the patch")
			continue
		}
		if strings.Contains(f.Content, "{{") {
			p.add(SeverityReview, "templated", f.Path, "", "", "the file is a template; Samuori cannot validate it until it is rendered")
			continue
		}
		dec := yaml.NewDecoder(strings.NewReader(f.Content))
		for {
			var v any
			err := dec.Decode(&v)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				p.add(SeverityError, "unparsable", f.Path, "", "", "the file is not valid YAML: %v", err)
				p.parsed[i] = nil
				break
			}
			if v == nil {
				continue
			}
			m, ok := v.(map[string]any)
			if !ok {
				p.add(SeverityError, "not-an-object", f.Path, "", "", "a document is not a Kubernetes object")
				continue
			}
			p.parsed[i] = append(p.parsed[i], &object{file: i, obj: m})
		}
	}
}

func (p *processor) objects(kind string) []*object {
	var out []*object
	for _, objs := range p.parsed {
		for _, o := range objs {
			if str(o.obj["kind"]) == kind {
				out = append(out, o)
			}
		}
	}
	return out
}

// podSpec returns the pod template spec of a workload, or nil for other kinds.
func podSpec(obj map[string]any) map[string]any {
	switch str(obj["kind"]) {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController", "Job":
		m, _ := dig(obj, "spec", "template", "spec").(map[string]any)
		return m
	case "CronJob":
		m, _ := dig(obj, "spec", "jobTemplate", "spec", "template", "spec").(map[string]any)
		return m
	case "Pod":
		m, _ := obj["spec"].(map[string]any)
		return m
	}
	return nil
}

func containers(spec map[string]any) []map[string]any {
	var out []map[string]any
	for _, key := range []string{"initContainers", "containers"} {
		list, _ := spec[key].([]any)
		for _, c := range list {
			if m, ok := c.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// apply carries out the user's choices and Samuori's image records; each change is reported.
func (p *processor) apply() {
	o := p.ctx.Options
	if img := p.ctx.Image; img != nil {
		p.substitute(img)
	}
	if o.Resources != (Resources{}) {
		for _, objs := range p.parsed {
			for _, ob := range objs {
				spec := podSpec(ob.obj)
				for _, c := range containersOf(spec, "containers") {
					if _, has := c["resources"]; has {
						continue
					}
					c["resources"] = resourcesFrom(o.Resources)
					ob.changed = true
					p.add(SeverityInfo, "resources-from-options", p.files[ob.file].Path, ob.ref(), "resources", "container %s takes the requests and limits you chose", str(c["name"]))
				}
			}
		}
	}
	if o.Namespace != "" {
		set := 0
		for _, objs := range p.parsed {
			for _, ob := range objs {
				meta, _ := ob.obj["metadata"].(map[string]any)
				if meta == nil || meta["namespace"] != nil || clusterScoped[str(ob.obj["kind"])] {
					continue
				}
				meta["namespace"] = o.Namespace
				ob.changed = true
				set++
			}
		}
		if set > 0 {
			p.add(SeverityInfo, "namespace-from-options", "", "", "metadata.namespace", "Samuori set the namespace %s on %d objects that named none", o.Namespace, set)
		}
	}
	if o.Storage.StorageClass != "" {
		for _, ob := range p.objects("PersistentVolumeClaim") {
			spec, _ := ob.obj["spec"].(map[string]any)
			if spec != nil && spec["storageClassName"] == nil {
				spec["storageClassName"] = o.Storage.StorageClass
				ob.changed = true
			}
		}
	}
	e := o.Exposure
	if e.Type == ExposeClusterIP || e.Service == "" {
		return
	}
	var svc *object
	for _, ob := range p.objects("Service") {
		if str(dig(ob.obj, "metadata", "name")) == e.Service {
			svc = ob
		}
	}
	if svc == nil {
		p.add(SeverityError, "exposed-service-missing", "", "Service/"+e.Service, "", "there is no Service %s to expose; Compose services get one only when they publish ports", e.Service)
		return
	}
	spec, _ := svc.obj["spec"].(map[string]any)
	switch e.Type {
	case ExposeNodePort, ExposeLoadBalancer:
		spec["type"] = map[string]string{ExposeNodePort: "NodePort", ExposeLoadBalancer: "LoadBalancer"}[e.Type]
		svc.changed = true
		p.add(SeverityInfo, "exposure-from-options", p.files[svc.file].Path, svc.ref(), "spec.type", "the service type is %s, as you chose", spec["type"])
	case ExposeIngress:
		if t := str(spec["type"]); t != "" && t != "ClusterIP" {
			p.add(SeverityReview, "exposure-conflict", p.files[svc.file].Path, svc.ref(), "spec.type", "the Compose file makes this service %s; with an Ingress, ClusterIP is usually enough", t)
		}
		ports, _ := spec["ports"].([]any)
		if len(ports) == 0 {
			p.add(SeverityError, "exposed-service-missing", p.files[svc.file].Path, svc.ref(), "spec.ports", "the service has no port to route to")
			return
		}
		port := dig(ports[0].(map[string]any), "port")
		ing := map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
			"metadata": map[string]any{"name": e.Service, "labels": map[string]any{"app.kubernetes.io/name": e.Service}},
			"spec": map[string]any{"rules": []any{map[string]any{"host": e.Host, "http": map[string]any{"paths": []any{map[string]any{
				"path": e.Path, "pathType": "Prefix", "backend": map[string]any{"service": map[string]any{"name": e.Service, "port": map[string]any{"number": port}}}}}}}}}}
		if o.Namespace != "" {
			ing["metadata"].(map[string]any)["namespace"] = o.Namespace
		}
		name := path.Join(path.Dir(p.files[svc.file].Path), e.Service+"-ingress.yml")
		p.files = append(p.files, File{Path: name})
		p.parsed = append(p.parsed, []*object{{file: len(p.files) - 1, obj: ing, changed: true}})
		p.add(SeverityInfo, "ingress-from-options", name, "Ingress/"+e.Service, "", "Samuori added an Ingress for %s%s, as you chose; it needs an ingress controller and, for TLS, a certificate", e.Host, e.Path)
	}
}

var clusterScoped = map[string]bool{"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true, "PersistentVolume": true, "StorageClass": true,
	"CustomResourceDefinition": true, "IngressClass": true, "PriorityClass": true, "ValidatingWebhookConfiguration": true, "MutatingWebhookConfiguration": true}

func containersOf(spec map[string]any, key string) []map[string]any {
	if spec == nil {
		return nil
	}
	var out []map[string]any
	list, _ := spec[key].([]any)
	for _, c := range list {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func resourcesFrom(r Resources) map[string]any {
	out := map[string]any{}
	req, lim := map[string]any{}, map[string]any{}
	if r.CPURequest != "" {
		req["cpu"] = r.CPURequest
	}
	if r.MemoryRequest != "" {
		req["memory"] = r.MemoryRequest
	}
	if r.CPULimit != "" {
		lim["cpu"] = r.CPULimit
	}
	if r.MemoryLimit != "" {
		lim["memory"] = r.MemoryLimit
	}
	if len(req) > 0 {
		out["requests"] = req
	}
	if len(lim) > 0 {
		out["limits"] = lim
	}
	return out
}

// substitute pins the Samuori image by digest in the workload of its service and adds a readiness probe only when the
// build's runtime check names a port.
func (p *processor) substitute(img *ImageEvidence) {
	found := false
	for _, objs := range p.parsed {
		for _, ob := range objs {
			spec := podSpec(ob.obj)
			for _, c := range containersOf(spec, "containers") {
				if str(c["name"]) != img.Service {
					continue
				}
				found = true
				file := p.files[ob.file].Path
				ref := img.Reference
				if ref == "" {
					ref = "samuori-unpublished/" + img.Service + "@" + img.Digest
					p.add(SeverityError, "image-not-published", file, ob.ref(), "image", "the Samuori image %s is not published; publish it and generate again, or replace %s with its registry reference", img.Digest, ref)
				} else {
					p.add(SeverityInfo, "image-pinned", file, ob.ref(), "image", "container %s uses the Samuori image %s", img.Service, ref)
				}
				c["image"] = ref
				ob.changed = true
				for _, k := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
					probe, _ := c[k].(map[string]any)
					exec, _ := probe["exec"].(map[string]any)
					if exec == nil {
						continue
					}
					var argv []string
					cmd, _ := exec["command"].([]any)
					for _, a := range cmd {
						argv = append(argv, str(a))
					}
					p.add(SeverityReview, "probe-from-replaced-image", file, ob.ref(), k, "container %s keeps the %s from the Compose file, which runs %q in the image Samuori replaced; Samuori has no evidence that the Samuori image has that command or that it succeeds there, so check it before deploying", img.Service, k, strings.Join(argv, " "))
				}
				if c["readinessProbe"] != nil {
					continue
				}
				switch {
				case img.Port > 0 && (img.Profile == "http-startup" || img.Profile == "static-serve"):
					c["readinessProbe"] = map[string]any{"httpGet": map[string]any{"path": or(img.Path, "/"), "port": img.Port}, "periodSeconds": 10}
					p.add(SeverityInfo, "probe-from-evidence", file, ob.ref(), "readinessProbe", "an HTTP readiness probe on port %d, from the %s check of build %s", img.Port, img.Profile, img.BuildRunID)
				case img.Port > 0:
					c["readinessProbe"] = map[string]any{"tcpSocket": map[string]any{"port": img.Port}, "periodSeconds": 10}
					p.add(SeverityInfo, "probe-from-evidence", file, ob.ref(), "readinessProbe", "a TCP readiness probe on port %d, from build %s", img.Port, img.BuildRunID)
				}
			}
		}
	}
	if !found {
		p.add(SeverityError, "image-service-missing", "", "", "", "no workload runs the Compose service %s, so the Samuori image was not placed", img.Service)
	}
}

var (
	secretName  = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|secret|token|api[_-]?key|private[_-]?key|credential|access[_-]?key|signing[_-]?key|session[_-]?key|client[_-]?secret|connection[_-]?string|dsn)$|(?i)^(.*_)?(pass(word|wd)?|secret|token|api[_-]?key)(_|$)`)
	notSecret   = regexp.MustCompile(`(?i)_(file|path|dir|url|host|port|user|username|name|endpoint|length|ttl|expiry|enabled)$`)
	credInValue = regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`)
)

// secretLike reports whether an environment variable's literal value looks like a credential.
func secretLike(name, value string) bool {
	if value == "" || strings.HasPrefix(value, "$") {
		return false
	}
	if credInValue.MatchString(value) {
		return true
	}
	return secretName.MatchString(name) && !notSecret.MatchString(name)
}

// inspect reports Samuori policy findings and replaces literal secrets.
func (p *processor) inspect(ob *object) {
	file := p.files[ob.file].Path
	kind := str(ob.obj["kind"])
	switch kind {
	case "Secret":
		p.blankSecret(ob)
	case "ConfigMap":
		data, _ := ob.obj["data"].(map[string]any)
		for k, v := range data {
			if secretLike(k, str(v)) {
				data[k] = ""
				ob.changed = true
				p.add(SeverityReview, "secret-in-configmap", file, ob.ref(), "data."+k, "%s looked like a credential; Samuori removed its value. Keep it in a Secret instead", k)
			}
		}
	case "Service":
		if t := str(dig(ob.obj, "spec", "type")); t == "NodePort" || t == "LoadBalancer" {
			p.add(SeverityWarning, "exposed-outside-cluster", file, ob.ref(), "spec.type", "a %s service is reachable from outside the cluster", t)
		}
	}
	spec := podSpec(ob.obj)
	if spec == nil {
		return
	}
	workload := str(dig(ob.obj, "metadata", "name"))
	for _, key := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if spec[key] == true {
			p.add(SeverityError, "host-namespace", file, ob.ref(), key, "%s shares the node's namespace, which Samuori policy forbids", key)
		}
	}
	vols, _ := spec["volumes"].([]any)
	for _, v := range vols {
		if m, ok := v.(map[string]any); ok && m["hostPath"] != nil {
			p.add(SeverityError, "host-path", file, ob.ref(), "volumes."+str(m["name"]), "volume %s mounts a path of the node (hostPath), which Samuori policy forbids", str(m["name"]))
		}
	}
	podSC, _ := spec["securityContext"].(map[string]any)
	podNonRoot := podSC != nil && podSC["runAsNonRoot"] == true
	if podSC != nil && isZero(podSC["runAsUser"]) {
		p.add(SeverityError, "runs-as-root", file, ob.ref(), "securityContext.runAsUser", "the pod runs as root (runAsUser 0)")
	}
	for _, c := range containers(spec) {
		name := str(c["name"])
		field := func(s string) string { return "containers." + name + "." + s }
		sc, _ := c["securityContext"].(map[string]any)
		if sc != nil {
			if sc["privileged"] == true {
				p.add(SeverityError, "privileged", file, ob.ref(), field("securityContext.privileged"), "container %s is privileged, which Samuori policy forbids", name)
			}
			if add, _ := dig(sc, "capabilities", "add").([]any); len(add) > 0 {
				p.add(SeverityError, "added-capabilities", file, ob.ref(), field("securityContext.capabilities.add"), "container %s adds Linux capabilities %v", name, add)
			}
			if sc["allowPrivilegeEscalation"] == true {
				p.add(SeverityError, "privilege-escalation", file, ob.ref(), field("securityContext.allowPrivilegeEscalation"), "container %s allows privilege escalation", name)
			}
			if isZero(sc["runAsUser"]) {
				p.add(SeverityError, "runs-as-root", file, ob.ref(), field("securityContext.runAsUser"), "container %s runs as root (runAsUser 0)", name)
			}
			if str(sc["procMount"]) == "Unmasked" || str(dig(sc, "seccompProfile", "type")) == "Unconfined" {
				p.add(SeverityError, "unconfined", file, ob.ref(), field("securityContext"), "container %s turns off kernel confinement (procMount or seccomp)", name)
			}
		}
		if !podNonRoot && (sc == nil || sc["runAsNonRoot"] != true) {
			p.add(SeverityReview, "may-run-as-root", file, ob.ref(), field("securityContext.runAsNonRoot"), "container %s runs as the image's user, which may be root; set runAsNonRoot once the image supports it", name)
		}
		ports, _ := c["ports"].([]any)
		for _, pt := range ports {
			if m, ok := pt.(map[string]any); ok && m["hostPort"] != nil {
				p.add(SeverityWarning, "host-port", file, ob.ref(), field("ports"), "container %s binds port %v on the node", name, m["hostPort"])
			}
		}
		image := str(c["image"])
		if image != "" && !strings.Contains(image, "@sha256:") {
			p.add(SeverityReview, "image-not-pinned", file, ob.ref(), field("image"), "%s is referenced by tag; pin it by digest so every node runs the same image", image)
		}
		if c["resources"] == nil {
			p.add(SeverityReview, "no-resources", file, ob.ref(), field("resources"), "container %s has no requests or limits", name)
		}
		if c["readinessProbe"] == nil {
			p.add(SeverityReview, "no-readiness-probe", file, ob.ref(), field("readinessProbe"), "container %s has no readiness probe and Samuori has no evidence to add one", name)
		}
		p.envSecrets(ob, c, workload, file, field)
	}
}

func (p *processor) envSecrets(ob *object, c map[string]any, workload, file string, field func(string) string) {
	env, _ := c["env"].([]any)
	for _, e := range env {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		name, value := str(m["name"]), str(m["value"])
		if _, literal := m["value"]; !literal || !secretLike(name, value) {
			continue
		}
		secret := secretObjectName(workload)
		delete(m, "value")
		m["valueFrom"] = map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": name}}
		ob.changed = true
		p.add(SeverityReview, "secret-reference", file, ob.ref(), field("env."+name),
			"%s held a literal value that looks like a credential; Samuori replaced it with a reference to key %s of Secret %s, which you create in the cluster (the value is not in the bundle)", name, name, secret)
	}
}

func secretObjectName(workload string) string {
	n := strings.Trim(workload, "-")
	if len(n) > 52 {
		n = n[:52]
	}
	return n + "-secrets"
}

func (p *processor) blankSecret(ob *object) {
	file := p.files[ob.file].Path
	var keys []string
	for _, field := range []string{"data", "stringData"} {
		m, _ := ob.obj[field].(map[string]any)
		for k := range m {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	blank := map[string]any{}
	for _, k := range keys {
		blank[k] = ""
	}
	delete(ob.obj, "data")
	ob.obj["stringData"] = blank
	ob.changed = true
	p.add(SeverityReview, "secret-values-removed", file, ob.ref(), "data", "the Secret carried values from the input (%s); Samuori removed them. Set them in the cluster, never in this bundle", strings.Join(keys, ", "))
}

func (p *processor) extras() {
	if p.ctx.Options.Format != FormatKustomize || slices.ContainsFunc(p.files, func(f File) bool { return kustomization(f.Path) }) {
		return
	}
	var resources []string
	for i, f := range p.files {
		if manifestFile(f.Path) && strings.HasPrefix(f.Path, "manifests/") {
			resources = append(resources, ymlName(strings.TrimPrefix(p.files[i].Path, "manifests/")))
		}
	}
	sort.Strings(resources)
	k := map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": resources}
	if ns := p.ctx.Options.Namespace; ns != "" {
		k["namespace"] = ns
	}
	b, _ := marshalYAML(k)
	p.files = append(p.files, newFile("manifests/kustomization.yml", b))
	p.parsed = append(p.parsed, nil)
	p.add(SeverityInfo, "kustomization", "manifests/kustomization.yml", "", "", "Samuori listed the generated manifests in a kustomization; Kompose does not write one")
}

func (p *processor) validate(ob *object) {
	file := p.files[ob.file].Path
	errs, err := validateObject(ob.obj)
	switch {
	case errors.Is(err, errUnknownKind):
		p.add(SeverityReview, "kind-not-validated", file, ob.ref(), "", "%s %s is not in Samuori's Kubernetes %s schema set and was not validated", str(ob.obj["apiVersion"]), str(ob.obj["kind"]), KubernetesVersion)
	case err != nil:
		p.add(SeverityError, "schema-unavailable", file, ob.ref(), "", "the object could not be validated: %v", err)
	}
	for _, e := range errs {
		field, msg, _ := strings.Cut(e, ": ")
		p.add(SeverityError, "schema", file, ob.ref(), field, "not valid for Kubernetes %s: %s", KubernetesVersion, msg)
	}
}

func (p *processor) serialize() {
	for i, objs := range p.parsed {
		changed := false
		for _, o := range objs {
			changed = changed || o.changed
		}
		if !changed {
			continue
		}
		var buf bytes.Buffer
		for j, o := range objs {
			if j > 0 {
				buf.WriteString("---\n")
			}
			b, err := marshalYAML(o.obj)
			if err != nil {
				p.add(SeverityError, "unserializable", p.files[i].Path, o.ref(), "", "the object could not be written: %v", err)
				continue
			}
			buf.Write(b)
		}
		p.files[i] = newFile(p.files[i].Path, buf.Bytes())
	}
}

// rename gives manifests the .yml extension Samuori uses. Helm requires its own names for Chart.yaml and values files,
// and a folder with a provider's kustomization.yaml keeps its names, which that kustomization refers to.
func (p *processor) rename() {
	keep := map[string]bool{}
	for _, f := range p.files {
		if path.Base(f.Path) == "kustomization.yaml" {
			keep[path.Dir(f.Path)] = true
		}
	}
	for i := range p.files {
		if keep[path.Dir(p.files[i].Path)] {
			continue
		}
		if name := ymlName(p.files[i].Path); name != p.files[i].Path {
			for j := range p.findings {
				if p.findings[j].File == p.files[i].Path {
					p.findings[j].File = name
				}
			}
			p.files[i].Path = name
		}
	}
}

func ymlName(name string) string {
	if strings.HasSuffix(name, ".yaml") && !helmMeta(name) {
		return strings.TrimSuffix(name, ".yaml") + ".yml"
	}
	return name
}

func marshalYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

func newFile(name string, b []byte) File {
	sum := sha256.Sum256(b)
	return File{Path: name, Content: string(b), Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: len(b)}
}

func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func isZero(v any) bool {
	switch n := v.(type) {
	case int:
		return n == 0
	case int64:
		return n == 0
	case uint64:
		return n == 0
	case float64:
		return n == 0
	}
	return false
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
