package buildspec

import (
	"fmt"
	"strings"
	"testing"
)

// TestSystemPackagesSchema: package names are plain distribution package names, so none can carry shell text into
// the rendered install command, and each list is bounded.
func TestSystemPackagesSchema(t *testing.T) {
	spec := func(pkgs string) string {
		return "apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\nmetadata: {name: api}\nspec:\n" +
			"  application: {type: worker, runtime: {language: nodejs}}\n" +
			"  build: {method: recipe, recipe: {id: node-service, version: 1.0.0, start: node worker.js, systemPackages: " + pkgs + "}}\n"
	}
	if got := rules(t, spec("{build: [build-essential, python3, libstdc++6, g++], runtime: [libvips42, ca-certificates]}")); got != "" {
		t.Errorf("valid packages refused: %s", got)
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = fmt.Sprintf("pkg%d", i)
	}
	for name, pkgs := range map[string]string{
		"shell text":     `{build: ["curl; rm -rf /"]}`,
		"option":         `{runtime: ["-y"]}`,
		"upper case":     `{runtime: [Curl]}`,
		"space":          `{build: ["a b"]}`,
		"duplicate":      `{build: [curl, curl]}`,
		"unknown field":  `{test: [curl]}`,
		"too many":       "{build: [" + strings.Join(many, ", ") + "]}",
		"version suffix": `{build: ["curl=8.5.0-2"]}`,
	} {
		if got := rules(t, spec(pkgs)); got == "" {
			t.Errorf("%s was accepted", name)
		}
	}
}
