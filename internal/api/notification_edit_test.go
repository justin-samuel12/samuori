package api

import (
	"fmt"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/ids"
)

// TestNotificationEdits: administrators edit channels and rules with the checks of creation; a Slack channel's new
// webhook URL becomes a new controller secret and is never returned; the kind is fixed; developers are refused.
func TestNotificationEdits(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, _, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	code, _, ch := l.json(t, admin, "POST", "/notification-channels", `{"name":"cicd","kind":"slack","secret":"https://hooks.slack.com/services/T0/B0/old"}`, nil)
	if code != 201 {
		t.Fatalf("create channel = %d %v", code, ch)
	}
	id := ch["id"].(string)
	secretOf := func() string {
		var s string
		_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT secret_id FROM notification_channels WHERE id = ?`), id).Scan(&s)
		return s
	}
	before := secretOf()
	if code, _, _ := l.json(t, alice, "PUT", "/notification-channels/"+id, `{"name":"x"}`, nil); code != 403 {
		t.Errorf("developer edited a channel: %d", code)
	}
	code, _, upd := l.json(t, admin, "PUT", "/notification-channels/"+id, `{"name":"cicd-builds"}`, nil)
	if code != 200 || upd["name"] != "cicd-builds" || secretOf() != before {
		t.Errorf("rename = %d %v, secret changed %v", code, upd, secretOf() != before)
	}
	code, _, upd = l.json(t, admin, "PUT", "/notification-channels/"+id, `{"name":"cicd-builds","secret":"https://hooks.slack.com/services/T0/B0/new"}`, nil)
	if code != 200 || secretOf() == before || strings.Contains(fmt.Sprint(upd), "hooks.slack.com") {
		t.Errorf("new webhook URL = %d %v", code, upd)
	}
	for name, body := range map[string]string{
		"url on slack":  `{"name":"cicd-builds","url":"https://example.com/hook"}`,
		"plain http":    `{"name":"cicd-builds","secret":"http://hooks.example.com/x"}`,
		"bad name":      `{"name":"Not Valid"}`,
		"unknown field": `{"name":"cicd-builds","kind":"webhook"}`,
	} {
		if code, _, b := l.json(t, admin, "PUT", "/notification-channels/"+id, body, nil); code != 422 && code != 400 {
			t.Errorf("%s = %d %v", name, code, b)
		}
	}
	code, _, rule := l.json(t, admin, "POST", "/notification-rules", fmt.Sprintf(`{"channelId":%q,"events":["build.succeeded"]}`, id), nil)
	if code != 201 {
		t.Fatalf("create rule = %d %v", code, rule)
	}
	rid := rule["id"].(string)
	code, _, r2 := l.json(t, admin, "PUT", "/notification-rules/"+rid, fmt.Sprintf(`{"channelId":%q,"events":["build.failed","validation.failed"]}`, id), nil)
	if code != 200 || fmt.Sprint(r2["events"]) != "[build.failed validation.failed]" {
		t.Errorf("edit rule = %d %v", code, r2)
	}
	if code, _, _ := l.json(t, admin, "PUT", "/notification-rules/"+rid, fmt.Sprintf(`{"channelId":%q,"events":["build.exploded"]}`, id), nil); code != 422 && code != 400 {
		t.Errorf("unknown event = %d", code)
	}
	if code, _, _ := l.json(t, admin, "PUT", "/notification-rules/"+ids.New(), fmt.Sprintf(`{"channelId":%q,"events":["build.failed"]}`, id), nil); code != 404 {
		t.Errorf("missing rule = %d", code)
	}
	if code, _, _ := l.json(t, alice, "PUT", "/notification-rules/"+rid, fmt.Sprintf(`{"channelId":%q,"events":["build.failed"]}`, id), nil); code != 403 {
		t.Errorf("developer edited a rule: %d", code)
	}
	var audited int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('notification-channel.update', 'notification-rule.update')`).Scan(&audited)
	if audited != 3 {
		t.Errorf("edit audit events = %d", audited)
	}
}
