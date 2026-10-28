package buildspec

import (
	"strings"
	"testing"
)

// TestFrontendRules: a frontend is served by the launcher on the image's port, so the spec needs an http-service
// with a port, a start command for the application and a different port behind the launcher (ADR 0055).
func TestFrontendRules(t *testing.T) {
	spec := func(app, recipe, extra string) string {
		return "apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\nmetadata: {name: audit}\nspec:\n" +
			"  application: " + app + "\n" +
			"  build: {method: recipe, recipe: {id: python-service, version: 1.0.0, path: backend, " + recipe + "}}\n" + extra
	}
	const app = "{type: http-service, port: 8080, runtime: {language: python}}"
	const start = "start: uvicorn app.main:app --port 8000"
	if got := rules(t, spec(app, start+", frontend: {path: frontend, apiPrefixes: [/api]}", "")); got != "" {
		t.Errorf("valid frontend refused: %s", got)
	}
	for name, s := range map[string]string{
		"worker":        spec("{type: worker, port: 8080, runtime: {language: python}}", start+", frontend: {path: frontend}", ""),
		"no port":       spec("{type: http-service, runtime: {language: python}}", start+", frontend: {path: frontend}", ""),
		"same port":     spec(app, start+", frontend: {path: frontend, backendPort: 8080}", ""),
		"no start":      spec(app, "frontend: {path: frontend}", ""),
		"same folder":   spec(app, start+", frontend: {path: backend}", ""),
		"two platforms": spec(app, start+", frontend: {path: frontend}", "  target: {platforms: [linux/amd64, linux/arm64]}\n"),
		"prefix text":   spec(app, start+`, frontend: {path: frontend, apiPrefixes: ["/api\nRUN id"]}`, ""),
		"escaping path": spec(app, start+", frontend: {path: ../x}", ""),
		"no path":       spec(app, start+", frontend: {output: dist}", ""),
	} {
		if got := rules(t, s); got == "" {
			t.Errorf("%s was accepted", name)
		} else if strings.Contains(got, "yaml") {
			t.Errorf("%s: fixture is not valid YAML: %s", name, got)
		}
	}
}
