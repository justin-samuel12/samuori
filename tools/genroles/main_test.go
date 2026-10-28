package main

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestRender(t *testing.T) {
	spec, err := openapi3.NewLoader().LoadFromData([]byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /health:
    get: {operationId: getHealth, x-samuori-public: true, responses: {"200": {description: ok}}}
  /teams:
    get: {operationId: listTeams, x-samuori-roles: [any], responses: {"200": {description: ok}}}
    post: {operationId: createTeam, x-samuori-roles: [administrator], responses: {"200": {description: ok}}}
  /builds:
    post: {operationId: createBuild, x-samuori-roles: [developer, maintainer], responses: {"200": {description: ok}}}
  /nobody:
    get: {operationId: undeclared, responses: {"200": {description: ok}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := render(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := `export const operationAccess = {
  createBuild: ["developer", "maintainer"],
  createTeam: ["administrator"],
  getHealth: "public",
  listTeams: "any",
  undeclared: [],
} as const satisfies Record<string, Access>;`
	if !strings.Contains(string(out), want) {
		t.Fatalf("render =\n%s", out)
	}
}

// TestDeclaredSpec: the real document renders, and every operation declares who may call it.
func TestDeclaredSpec(t *testing.T) {
	spec, err := openapi3.NewLoader().LoadFromFile("../../api/openapi/samuori.v1.yml")
	if err != nil {
		t.Fatal(err)
	}
	out, err := render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), ": [],") {
		t.Errorf("an operation declares neither x-samuori-roles nor x-samuori-public:\n%s", out)
	}
	if !strings.Contains(string(out), `  listProjects: ["developer", "maintainer", "publisher", "security-reviewer", "administrator"],`) {
		t.Error("listProjects is not rendered from the document")
	}
}
