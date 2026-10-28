package api

import (
	"fmt"
	"slices"
	"testing"
)

// TestProjectRoleShowsItsTeam: a person with a role on a project of another team sees that project and its team's
// name, but not the team's other projects.
func TestProjectRoleShowsItsTeam(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, projB := twoTeams(t, l)
	admin := l.adminMember(t)
	_, _, me := l.json(t, alice, "GET", "/me", "", nil)
	principal := me["principal"].(map[string]any)["id"].(string)
	if st, _, b := l.json(t, admin, "POST", "/role-bindings", fmt.Sprintf(`{"principalId":%q,"role":"developer","scopeType":"project","scopeId":%q}`, principal, projB), nil); st != 201 {
		t.Fatalf("bind = %d %v", st, b)
	}
	_, _, projects := l.json(t, alice, "GET", "/projects", "", nil)
	teamOf := map[string]string{}
	for _, p := range projects["items"].([]any) {
		m := p.(map[string]any)
		teamOf[m["id"].(string)] = m["teamId"].(string)
		if m["teamName"] != "team-a" && m["teamName"] != "team-b" {
			t.Errorf("project %v has no team name", m)
		}
	}
	if teamOf[projA] == "" || teamOf[projB] == "" {
		t.Fatalf("projects = %v", projects)
	}
	_, _, teams := l.json(t, alice, "GET", "/teams", "", nil)
	var visible []string
	for _, tm := range teams["items"].([]any) {
		visible = append(visible, tm.(map[string]any)["id"].(string))
	}
	for _, id := range []string{teamOf[projA], teamOf[projB]} {
		if !slices.Contains(visible, id) {
			t.Errorf("team %s of a visible project is missing from /teams: %v", id, visible)
		}
	}
}
