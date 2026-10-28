// Command kubeschema extracts the schemas of the Kubernetes kinds Samuori validates from a release's OpenAPI v2 document
// (kubernetes/kubernetes api/openapi-spec/swagger.json, Apache-2.0) into one compact, strict JSON Schema file
// (docs/architecture/kubernetes-conversion.md#validation).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

var kinds = []string{
	"v1/ConfigMap", "v1/Namespace", "v1/PersistentVolumeClaim", "v1/Pod", "v1/ReplicationController", "v1/Secret", "v1/Service", "v1/ServiceAccount",
	"apps/v1/DaemonSet", "apps/v1/Deployment", "apps/v1/ReplicaSet", "apps/v1/StatefulSet",
	"batch/v1/CronJob", "batch/v1/Job",
	"networking.k8s.io/v1/Ingress", "networking.k8s.io/v1/NetworkPolicy",
	"autoscaling/v2/HorizontalPodAutoscaler", "policy/v1/PodDisruptionBudget",
}

func main() {
	in := flag.String("swagger", "", "swagger.json of a Kubernetes release")
	version := flag.String("version", "", "the release, such as v1.36.5")
	out := flag.String("out", "", "output file")
	flag.Parse()
	if err := run(*in, *version, *out); err != nil {
		fmt.Fprintln(os.Stderr, "kubeschema:", err)
		os.Exit(1)
	}
}

func run(in, version, out string) error {
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var doc struct {
		Definitions map[string]map[string]any `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	byGVK := map[string]string{}
	for name, def := range doc.Definitions {
		gvks, _ := def["x-kubernetes-group-version-kind"].([]any)
		for _, g := range gvks {
			m := g.(map[string]any)
			gv := m["version"].(string)
			if grp := m["group"].(string); grp != "" {
				gv = grp + "/" + gv
			}
			key := gv + "/" + m["kind"].(string)
			if len(gvks) == 1 {
				byGVK[key] = name
			}
		}
	}
	keep := map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		if keep[name] {
			return nil
		}
		def, ok := doc.Definitions[name]
		if !ok {
			return fmt.Errorf("missing definition %s", name)
		}
		keep[name] = true
		for _, ref := range refs(def) {
			if err := visit(ref); err != nil {
				return err
			}
		}
		return nil
	}
	index := map[string]string{}
	for _, k := range kinds {
		name, ok := byGVK[k]
		if !ok {
			return fmt.Errorf("%s is not in %s", k, in)
		}
		index[k] = name
		if err := visit(name); err != nil {
			return err
		}
	}
	defs := map[string]any{}
	names := make([]string, 0, len(keep))
	for n := range keep {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		defs[n] = strict(clean(doc.Definitions[n]))
	}
	if q, ok := defs["io.k8s.apimachinery.pkg.api.resource.Quantity"].(map[string]any); ok {
		q["type"] = []any{"string", "number", "null"}
	}
	result := map[string]any{"kubernetesVersion": version, "source": "kubernetes/kubernetes api/openapi-spec/swagger.json at " + version + " (Apache-2.0)",
		"kinds": index, "definitions": defs}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(b, '\n'), 0o644)
}

func refs(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok && k == "$ref" {
				out = append(out, strings.TrimPrefix(s, "#/definitions/"))
				continue
			}
			out = append(out, refs(x)...)
		}
	case []any:
		for _, x := range t {
			out = append(out, refs(x)...)
		}
	}
	return out
}

var dropped = map[string]bool{"description": true, "x-kubernetes-group-version-kind": true, "x-kubernetes-list-map-keys": true, "x-kubernetes-list-type": true,
	"x-kubernetes-patch-merge-key": true, "x-kubernetes-patch-strategy": true, "x-kubernetes-map-type": true, "x-kubernetes-unions": true}

// clean drops documentation, widens int-or-string and quantities as the API server accepts them, and allows null
// wherever a value is typed, as the API server drops nulls.
func clean(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			if !dropped[k] {
				out[k] = clean(x)
			}
		}
		if out["format"] == "int-or-string" {
			delete(out, "format")
			out["type"] = []any{"string", "integer", "null"}
		} else if ty, ok := out["type"].(string); ok {
			out["type"] = []any{ty, "null"}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = clean(x)
		}
		return out
	}
	return v
}

// strict rejects unknown fields in every object schema that lists its properties, as kubectl's strict validation does.
func strict(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if k != "properties" {
				t[k] = strict(x)
			}
		}
		if props, ok := t["properties"].(map[string]any); ok {
			for k, x := range props {
				props[k] = strict(x)
			}
			if _, has := t["additionalProperties"]; !has {
				t["additionalProperties"] = false
			}
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = strict(x)
		}
	}
	return v
}
