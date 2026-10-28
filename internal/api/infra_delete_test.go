package api

import (
	"fmt"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/ids"
)

// TestInfrastructureDeletes: administrators delete notification channels, federation routes and unused enrollment
// tokens, each audited; a channel that rules use and a route location a project uses are refused with the uses
// named; developers are refused every one.
func TestInfrastructureDeletes(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	audited := func(action, id string) int {
		var n int
		_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT COUNT(*) FROM audit_events WHERE action = ? AND resource_id = ?`), action, id).Scan(&n)
		return n
	}

	t.Run("notification channel", func(t *testing.T) {
		code, _, ch := l.json(t, admin, "POST", "/notification-channels", `{"name":"ops","kind":"slack","secret":"https://hooks.slack.com/services/T0/B0/x"}`, nil)
		if code != 201 {
			t.Fatalf("create channel = %d %v", code, ch)
		}
		id := ch["id"].(string)
		code, _, rule := l.json(t, admin, "POST", "/notification-rules", fmt.Sprintf(`{"channelId":%q,"events":["build.failed"]}`, id), nil)
		if code != 201 {
			t.Fatalf("create rule = %d %v", code, rule)
		}
		if code, _, _ := l.json(t, alice, "DELETE", "/notification-channels/"+id, "", nil); code != 403 {
			t.Errorf("developer deleted a channel: %d", code)
		}
		code, _, b := l.json(t, admin, "DELETE", "/notification-channels/"+id, "", nil)
		if code != 409 || b["code"] != "channel_in_use" || !strings.Contains(fmt.Sprint(b["message"]), "build.failed") {
			t.Fatalf("delete a used channel = %d %v", code, b)
		}
		if code, _, _ := l.json(t, admin, "DELETE", "/notification-rules/"+rule["id"].(string), "", nil); code != 204 {
			t.Fatalf("delete rule = %d", code)
		}
		if code, _, b := l.json(t, admin, "DELETE", "/notification-channels/"+id, "", nil); code != 204 {
			t.Fatalf("delete channel = %d %v", code, b)
		}
		if code, _, _ := l.json(t, admin, "DELETE", "/notification-channels/"+id, "", nil); code != 404 {
			t.Errorf("delete again = %d", code)
		}
		if audited("notification-channel.delete", id) != 1 {
			t.Error("channel deletion not audited")
		}
	})

	t.Run("federation route", func(t *testing.T) {
		pin := "sha256:" + strings.Repeat("ab", 32)
		code, _, peer := l.json(t, admin, "POST", "/federation/peers", fmt.Sprintf(`{"name":"site-b","endpoint":"https://b.example:8445","caPin":%q,"organizationId":%q}`, pin, ids.New()), nil)
		if code != 201 {
			t.Fatalf("create peer = %d %v", code, peer)
		}
		code, _, route := l.json(t, admin, "POST", "/federation/routes", fmt.Sprintf(`{"peerId":%q,"location":"site-b-east","remoteLocation":"east"}`, peer["id"]), nil)
		if code != 201 {
			t.Fatalf("create route = %d %v", code, route)
		}
		id := route["id"].(string)
		if code, _, _ := l.json(t, admin, "PUT", "/projects/"+projA+"/locations", `{"locations":["site-b-east"]}`, nil); code != 200 {
			t.Fatalf("restrict project = %d", code)
		}
		if code, _, _ := l.json(t, alice, "DELETE", "/federation/routes/"+id, "", nil); code != 403 {
			t.Errorf("developer deleted a route: %d", code)
		}
		if code, _, b := l.json(t, admin, "DELETE", "/locations/site-b-east", "", nil); code != 409 || !strings.Contains(fmt.Sprint(b["message"]), "federation route") {
			t.Errorf("delete a route's location directly = %d %v", code, b)
		}
		code, _, b := l.json(t, admin, "DELETE", "/federation/routes/"+id, "", nil)
		if code != 409 || !strings.Contains(fmt.Sprint(b["message"]), "project app") {
			t.Fatalf("delete a used route = %d %v", code, b)
		}
		if code, _, _ := l.json(t, admin, "PUT", "/projects/"+projA+"/locations", `{"locations":[]}`, nil); code != 200 {
			t.Fatalf("clear project locations = %d", code)
		}
		if code, _, b := l.json(t, admin, "DELETE", "/federation/routes/"+id, "", nil); code != 204 {
			t.Fatalf("delete route = %d %v", code, b)
		}
		_, _, locs := l.json(t, admin, "GET", "/locations", "", nil)
		if strings.Contains(fmt.Sprint(locs["items"]), "site-b-east") {
			t.Errorf("the route's location remains: %v", locs)
		}
		if audited("federation.route.delete", id) != 1 {
			t.Error("route deletion not audited")
		}
	})

	t.Run("enrollment token", func(t *testing.T) {
		code, _, tok := l.json(t, admin, "POST", "/enrollment-tokens", `{"ttlSeconds":3600}`, map[string]string{"Idempotency-Key": "infra-delete-token-0001"})
		if code != 201 {
			t.Fatalf("issue = %d %v", code, tok)
		}
		id := tok["id"].(string)
		if code, _, _ := l.json(t, alice, "GET", "/enrollment-tokens", "", nil); code != 403 {
			t.Errorf("developer listed tokens: %d", code)
		}
		if code, _, _ := l.json(t, alice, "DELETE", "/enrollment-tokens/"+id, "", nil); code != 403 {
			t.Errorf("developer revoked a token: %d", code)
		}
		if code, _, b := l.json(t, admin, "DELETE", "/enrollment-tokens/"+id, "", nil); code != 204 {
			t.Fatalf("revoke = %d %v", code, b)
		}
		if code, _, b := l.json(t, admin, "DELETE", "/enrollment-tokens/"+id, "", nil); code != 409 || b["code"] != "token_spent" {
			t.Errorf("revoke again = %d %v", code, b)
		}
		_, _, list := l.json(t, admin, "GET", "/enrollment-tokens", "", nil)
		if s := fmt.Sprint(list["items"]); !strings.Contains(s, id) || !strings.Contains(s, "revokedAt") || strings.Contains(s, tok["token"].(string)) {
			t.Errorf("list = %v", list)
		}
		if audited("enrollment_token.revoke", id) != 1 {
			t.Error("revocation not audited")
		}
	})
}
