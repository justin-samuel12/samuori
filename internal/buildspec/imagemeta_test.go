package buildspec

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestImageMetadataValidation: label and annotation keys are reverse-DNS keys outside Samuori's reserved prefix, values
// are bounded single-line text, and each map holds at most 64 entries.
func TestImageMetadataValidation(t *testing.T) {
	spec := func(field string, m map[string]string) string {
		raw, _ := json.Marshal(m)
		return "apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\nmetadata: {name: api}\nspec:\n" +
			"  build: {method: dockerfile, dockerfile: {path: Dockerfile}}\n" +
			"  outputs: {image: {" + field + ": " + string(raw) + "}}\n"
	}
	good := map[string]string{
		"org.opencontainers.image.source": "https://example.com/repo",
		"com.example.team":                "payments",
		"maintainer":                      "ops@example.com",
		"com.example.app/Version_2":       "1.2.3",
		"org.example.description":         strings.Repeat("é", 2048),
	}
	for _, field := range []string{"labels", "annotations"} {
		if got := rules(t, spec(field, good)); got != "" {
			t.Errorf("%s: valid entries refused: %s", field, got)
		}
		many := map[string]string{}
		for i := range 65 {
			many[fmt.Sprintf("com.example.k%d", i)] = "v"
		}
		for name, m := range map[string]map[string]string{
			"reserved prefix":   {"io.samuori.build": "x"},
			"reserved root":     {"io.samuori": "x"},
			"upper-case head":   {"Maintainer": "x"},
			"space in key":      {"com.example.a b": "x"},
			"leading dot":       {".com.example": "x"},
			"newline in value":  {"com.example.a": "line1\nline2"},
			"tab in value":      {"com.example.a": "a\tb"},
			"NUL in value":      {"com.example.a": "a\x00b"},
			"value over 4 KiB":  {"com.example.a": strings.Repeat("é", 2049)},
			"key over 255":      {"com.example." + strings.Repeat("a", 250): "x"},
			"too many":          many,
			"bracket in key":    {"com.example.a[linux/amd64]": "x"},
			"secret value form": {"com.example.a": "AKIAABCDEFGHIJKLMNOP"},
		} {
			if got := rules(t, spec(field, m)); got == "" {
				t.Errorf("%s: %s was accepted", field, name)
			}
		}
	}
}

func TestImageMetadataApplied(t *testing.T) {
	for method, want := range map[string][2]bool{
		"dockerfile": {true, true}, "recipe": {true, true}, "railpack": {false, true}, "buildpacks": {false, false},
	} {
		if l, a := ImageMetadataApplied(method); l != want[0] || a != want[1] {
			t.Errorf("%s: labels %v annotations %v, want %v", method, l, a, want)
		}
	}
}
