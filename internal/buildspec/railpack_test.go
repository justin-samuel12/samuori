package buildspec

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func rules(t *testing.T, doc string) string {
	t.Helper()
	res, err := Validate([]byte(doc), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range append(res.SchemaErrors, res.SemanticErrors...) {
		out = append(out, i.Rule+"@"+i.Path)
	}
	return strings.Join(out, " ")
}

// TestRailpackBuildSpec: the railpack method validates on its own, refuses fields it would ignore, and leaves
// existing recipe and Dockerfile specs valid (ADR 0047).
func TestRailpackBuildSpec(t *testing.T) {
	raw, err := os.ReadFile("../../examples/orders-api.spec.json")
	if err != nil {
		t.Fatal(err)
	}
	var wrapped struct{ Document json.RawMessage }
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		t.Fatal(err)
	}
	if got := rules(t, string(wrapped.Document)); got != "" {
		t.Errorf("the example recipe spec is no longer valid: %s", got)
	}
	head := "apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\nmetadata: {name: api}\nspec:\n"
	if got := rules(t, head+"  build: {method: dockerfile, dockerfile: {path: Dockerfile}}\n"); got != "" {
		t.Errorf("a Dockerfile spec is no longer valid: %s", got)
	}
	if got := rules(t, head+"  build: {method: railpack}\n"); got != "" {
		t.Errorf("a minimal railpack spec is invalid: %s", got)
	}
	if got := rules(t, head+"  build: {method: railpack, railpack: {context: services/api, startCommand: node server.js}}\n  secrets: [{id: npm-token}]\n"); got != "" {
		t.Errorf("a railpack spec with options is invalid: %s", got)
	}
	for name, c := range map[string]struct{ doc, want string }{
		"with a recipe":         {"  build: {method: railpack, recipe: {id: node-service, version: 1.0.0}}\n", "schema@/spec/build"},
		"railpack options":      {"  build: {method: dockerfile, dockerfile: {}, railpack: {}}\n", "schema@/spec/build"},
		"a context outside":     {"  build: {method: railpack, railpack: {context: ../x}}\n", "schema@/spec/build/railpack/context"},
		"a multi-line command":  {"  build: {method: railpack, railpack: {buildCommand: \"a\\nb\"}}\n", "schema@/spec/build/railpack/buildCommand"},
		"build arguments":       {"  build: {method: railpack, args: [{name: MODE, value: x}]}\n", "railpack-build-args@/spec/build/args"},
		"two platforms":         {"  build: {method: railpack}\n  target: {platforms: [linux/amd64, linux/arm64]}\n", "railpack-one-platform@/spec/target/platforms"},
		"clashing secrets":      {"  build: {method: railpack}\n  secrets: [{id: npm-token}, {id: npm.token}]\n", "railpack-secret-env-clash@/spec/secrets/1/id"},
		"a proxy-named secret":  {"  build: {method: railpack}\n  secrets: [{id: https-proxy}]\n", "railpack-secret-env-reserved@/spec/secrets/0/id"},
		"a file-mounted secret": {"  build: {method: railpack}\n  secrets: [{id: npmrc, mount: {target: /root/.npmrc}}]\n", "railpack-secret-mount@/spec/secrets/0/mount"},
	} {
		if got := rules(t, head+c.doc); !strings.Contains(got, c.want) {
			t.Errorf("%s: issues %q, want %s", name, got, c.want)
		}
	}
	if RailpackSecretEnv("npm-token.v2") != "NPM_TOKEN_V2" {
		t.Errorf("RailpackSecretEnv = %s", RailpackSecretEnv("npm-token.v2"))
	}
}
