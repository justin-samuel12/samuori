package buildspec

import (
	"strings"
	"testing"
)

// TestBuildpacksBuildSpec: the buildpacks method validates on its own, keeps configuration non-secret, reaches
// secrets only through bindings, and leaves the other methods' specs valid (ADR 0047, SEC-CNB-06).
func TestBuildpacksBuildSpec(t *testing.T) {
	head := "apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\nmetadata: {name: api}\nspec:\n"
	for name, doc := range map[string]string{
		"minimal":      "  build: {method: buildpacks}\n",
		"with options": "  build: {method: buildpacks, buildpacks: {context: services/api, env: {BP_NODE_VERSION: '22.*', BPE_DEFAULT_TZ: UTC}, bindings: [{name: npmrc, type: npmrc, secret: npm-token, key: .npmrc}]}}\n  secrets: [{id: npm-token}, {id: smoke, usage: test}]\n",
		"railpack":     "  build: {method: railpack}\n",
		"dockerfile":   "  build: {method: dockerfile, dockerfile: {path: Dockerfile}}\n",
	} {
		if got := rules(t, head+doc); got != "" {
			t.Errorf("%s: %s", name, got)
		}
	}
	for name, c := range map[string]struct{ doc, want string }{
		"with a recipe":       {"  build: {method: buildpacks, recipe: {id: node-service, version: 1.0.0}}\n", "schema@/spec/build"},
		"with railpack":       {"  build: {method: buildpacks, railpack: {}}\n", "schema@/spec/build"},
		"options elsewhere":   {"  build: {method: railpack, buildpacks: {}}\n", "schema@/spec/build"},
		"a context outside":   {"  build: {method: buildpacks, buildpacks: {context: ../x}}\n", "schema@/spec/build/buildpacks/context"},
		"a non-BP variable":   {"  build: {method: buildpacks, buildpacks: {env: {HTTPS_PROXY: x}}}\n", "schema@"},
		"a CNB variable":      {"  build: {method: buildpacks, buildpacks: {env: {CNB_USER_ID: '0'}}}\n", "schema@"},
		"a multi-line value":  {"  build: {method: buildpacks, buildpacks: {env: {BP_X: \"a\\nb\"}}}\n", "schema@/spec/build/buildpacks/env/BP_X"},
		"a key outside":       {"  build: {method: buildpacks, buildpacks: {bindings: [{name: b, type: t, secret: s, key: ../x}]}}\n  secrets: [{id: s}]\n", "schema@/spec/build/buildpacks/bindings/0/key"},
		"build arguments":     {"  build: {method: buildpacks, args: [{name: MODE, value: x}]}\n", "buildpacks-build-args@/spec/build/args"},
		"two platforms":       {"  build: {method: buildpacks}\n  target: {platforms: [linux/amd64, linux/arm64]}\n", "buildpacks-one-platform@/spec/target/platforms"},
		"an unbound secret":   {"  build: {method: buildpacks}\n  secrets: [{id: npm-token}]\n", "buildpacks-secret-unbound@/spec/secrets/0/id"},
		"an unknown secret":   {"  build: {method: buildpacks, buildpacks: {bindings: [{name: b, type: t, secret: nope}]}}\n", "buildpacks-binding-secret-unknown@/spec/build/buildpacks/bindings/0/secret"},
		"a test secret bound": {"  build: {method: buildpacks, buildpacks: {bindings: [{name: b, type: t, secret: s}]}}\n  secrets: [{id: s, usage: test}]\n", "buildpacks-binding-secret-unknown@/spec/build/buildpacks/bindings/0/secret"},
		"a mounted secret":    {"  build: {method: buildpacks, buildpacks: {bindings: [{name: b, type: t, secret: s}]}}\n  secrets: [{id: s, mount: {target: /root/.npmrc}}]\n", "buildpacks-secret-mount@/spec/secrets/0/mount"},
		"the type file":       {"  build: {method: buildpacks, buildpacks: {bindings: [{name: b, type: t, secret: s, key: type}]}}\n  secrets: [{id: s}]\n", "buildpacks-binding-key-reserved@/spec/build/buildpacks/bindings/0/key"},
		"one file twice":      {"  build: {method: buildpacks, buildpacks: {bindings: [{name: b, type: t, secret: s}, {name: b, type: t, secret: u, key: s}]}}\n  secrets: [{id: s}, {id: u}]\n", "buildpacks-binding-duplicate@/spec/build/buildpacks/bindings/1/key"},
		"two types":           {"  build: {method: buildpacks, buildpacks: {bindings: [{name: b, type: t, secret: s}, {name: b, type: v, secret: u}]}}\n  secrets: [{id: s}, {id: u}]\n", "buildpacks-binding-type-clash@/spec/build/buildpacks/bindings/1/type"},
	} {
		if got := rules(t, head+c.doc); !strings.Contains(got, c.want) {
			t.Errorf("%s: issues %q, want %s", name, got, c.want)
		}
	}
}
