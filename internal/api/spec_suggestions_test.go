package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestSpecSuggestions: a Node.js web service gets a valid built-in recipe spec that can be saved as it is, a
// Dockerfile upload gets the dockerfile method recommended, and only the project's developers and maintainers can ask
// (another team gets 404, a publisher 403).
func TestSpecSuggestions(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, _ := twoTeams(t, l)
	node := map[string]string{
		"package.json":      `{"name":"shop","scripts":{"start":"node server.js"},"engines":{"node":">=20"},"dependencies":{"express":"^4.21.0"}}`,
		"package-lock.json": `{"lockfileVersion":3,"packages":{"":{},"node_modules/express":{"version":"4.21.2"}}}`,
		"server.js":         "require('express')().listen(3000)\n",
	}
	snap := l.snapshotOf(t, alice, projA, node)
	path := "/snapshots/" + snap + "/spec-suggestions"
	if st, _, b := l.json(t, alice, "GET", path, "", nil); st != 409 || b["code"] != "discovery_required" {
		t.Fatalf("before discovery = %d %v", st, b)
	}
	discover := func(snap, key string) {
		t.Helper()
		_, _, op := l.json(t, alice, "POST", "/snapshots/"+snap+"/discoveries", "", map[string]string{"Idempotency-Key": key})
		if done := l.await(t, alice, fmt.Sprint(op["id"])); done.ResourceId == nil {
			t.Fatalf("discovery = %+v", done)
		}
	}
	discover(snap, "suggest-disc-00000001")

	type suggestion struct {
		Method, Title, CanonicalYAML string
		Recommended, Valid           bool
		Reasons                      []string
		Warnings                     []struct{ Code string }
		Document                     map[string]any
		SemanticErrors               []any
	}
	suggest := func(m member, path string) (int, []suggestion) {
		t.Helper()
		resp := l.call(t, m.c, "GET", path, "", nil)
		defer resp.Body.Close()
		var out struct{ Items []suggestion }
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Items
	}
	st, items := suggest(alice, path)
	if st != 200 || len(items) == 0 {
		t.Fatalf("suggestions = %d %+v", st, items)
	}
	s := items[0]
	if s.Method != "recipe" || !s.Recommended || !s.Valid || !strings.Contains(s.CanonicalYAML, "id: node-service") || len(s.Reasons) == 0 {
		t.Fatalf("first suggestion = %+v", s)
	}
	if app := s.Document["spec"].(map[string]any)["application"].(map[string]any); app["port"] != float64(3000) || app["type"] != "http-service" {
		t.Errorf("application = %v", app)
	}
	if !strings.Contains(fmt.Sprint(s.Warnings), "port-assumed") {
		t.Errorf("warnings = %+v", s.Warnings)
	}
	body, _ := json.Marshal(map[string]any{"document": s.Document})
	if st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/specs", string(body), nil); st != 201 {
		t.Fatalf("saving the suggestion = %d %v", st, b)
	}

	if st, items := suggest(alice, path+"?name=Bad_Name"); st != 400 || items != nil {
		t.Errorf("invalid name = %d", st)
	}
	if st, _ := suggest(bob, path); st != 404 {
		t.Errorf("another team = %d, want 404", st)
	}
	admin := l.adminMember(t)
	var team string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), projA).Scan(&team)
	if st, _ := suggest(l.memberWith(t, admin, "pat", "publisher", team), path); st != 403 {
		t.Errorf("publisher = %d, want 403", st)
	}

	node["Dockerfile"] = "FROM node:22-bookworm-slim\nWORKDIR /app\nCOPY . .\nRUN npm ci\nEXPOSE 8080\nCMD [\"node\", \"server.js\"]\n"
	withDockerfile := l.snapshotOf(t, alice, projA, node)
	discover(withDockerfile, "suggest-disc-00000002")
	_, items = suggest(alice, "/snapshots/"+withDockerfile+"/spec-suggestions?name=web")
	if len(items) < 2 || items[0].Method != "dockerfile" || !items[0].Recommended || !items[0].Valid || items[1].Method != "recipe" {
		t.Fatalf("Dockerfile upload suggestions = %+v", items)
	}
	if !strings.Contains(items[0].CanonicalYAML, "name: web") || !strings.Contains(items[0].CanonicalYAML, "port: 8080") {
		t.Errorf("dockerfile suggestion =\n%s", items[0].CanonicalYAML)
	}
}
