package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/ids"
)

// TestEgressPromotion: an organization administrator moves the allowances several projects hold for one destination
// into the organization egress policy; they are revoked in the same transaction, preset allowances are only listed,
// and a destination the policy lists with other settings is refused (ADR 0052, 2026-10-27 amendment).
func TestEgressPromotion(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, projB := twoTeams(t, l)
	admin := l.adminMember(t)
	var org, user string
	if err := l.db.SQL.QueryRow(l.db.Rebind(`SELECT p.org_id, u.id FROM projects p JOIN users u ON u.org_id = p.org_id WHERE p.id = ? AND u.username = 'alice'`), projA).Scan(&org, &user); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	allow := func(project, kind, preset, host string, port int, methods string, internal bool) {
		req := ids.New()
		var p, h, pt any
		if kind == "preset" {
			p = preset
		} else {
			h, pt = host, port
		}
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO egress_access_requests (id, org_id, project_id, method, entries, justification, evidence, requested_by, created_at, closed_at) VALUES (?, ?, ?, 'recipe', '{}', 'needs it for the build', '{}', ?, ?, ?)`, []any{req, org, project, user, now, now}},
			{`INSERT INTO project_egress_allowances (id, org_id, project_id, kind, preset, host, port, methods, reason, request_id, status, requested_by, approved_by, created_at, internal) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'needs it for the build', ?, 'active', ?, ?, ?, ?)`,
				[]any{ids.New(), org, project, kind, p, h, pt, methods, req, user, user, now, internal}},
		} {
			if _, err := l.db.SQL.Exec(l.db.Rebind(q.sql), q.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	allow(projA, "destination", "", "objects.githubusercontent.com", 443, `["recipe"]`, false)
	allow(projB, "destination", "", "objects.githubusercontent.com", 443, `["dockerfile"]`, false)
	allow(projA, "destination", "", "nexus.corp", 8081, `["recipe"]`, true)
	allow(projA, "preset", "pypi", "", 0, `["recipe"]`, false)

	if code, _, _ := l.json(t, alice, "GET", "/egress-allowances/by-destination", "", nil); code != 403 {
		t.Errorf("developer listed every project's allowances: %d", code)
	}
	_, _, list := l.json(t, admin, "GET", "/egress-allowances/by-destination", "", nil)
	dests := list["destinations"].([]any)
	first := dests[0].(map[string]any)
	if len(dests) != 2 || first["destination"] != "objects.githubusercontent.com:443" || len(first["allowances"].([]any)) != 2 || len(list["presets"].([]any)) != 1 {
		t.Fatalf("granted = %v", list)
	}

	const why = `"reason":"sharp downloads its binaries from GitHub releases"`
	promote := func(m member, body string) (int, body) {
		code, _, b := l.json(t, m, "POST", "/egress-allowances/promotion", body, nil)
		return code, b
	}
	if code, _ := promote(alice, `{"host":"objects.githubusercontent.com","port":443,"network":"public",`+why+`}`); code != 403 {
		t.Errorf("developer promoted = %d", code)
	}
	if code, _ := promote(admin, `{"host":"objects.githubusercontent.com","port":443,"network":"public","reason":"short"}`); code != 422 && code != 400 {
		t.Errorf("short reason = %d", code)
	}
	code, out := promote(admin, `{"host":"objects.githubusercontent.com","port":443,"network":"public","noDockerfile":true,`+why+`}`)
	if code != 200 || len(out["revoked"].([]any)) != 2 {
		t.Fatalf("promote = %d %v", code, out)
	}
	version := int(out["policyVersion"].(float64))
	if !strings.Contains(fmt.Sprint(out["revoked"]), fmt.Sprintf("Moved into the organization egress policy, version %d: sharp", version)) {
		t.Errorf("revoke reason = %v", out["revoked"])
	}
	_, _, eg := l.json(t, admin, "GET", "/system/egress", "", nil)
	pol := fmt.Sprint(eg["policy"])
	if !strings.Contains(pol, "host:objects.githubusercontent.com") || !strings.Contains(pol, "noDockerfile:true") || !strings.Contains(pol, "network:public") {
		t.Errorf("policy = %s", pol)
	}
	if code, _ := promote(admin, `{"host":"objects.githubusercontent.com","port":443,"network":"public",`+why+`}`); code != 404 {
		t.Errorf("second promote = %d", code)
	}
	if _, _, after := l.json(t, admin, "GET", "/egress-allowances/by-destination", "", nil); len(after["destinations"].([]any)) != 1 {
		t.Errorf("after promotion = %v", after)
	}

	allow(projB, "destination", "", "objects.githubusercontent.com", 443, `["recipe"]`, true)
	if code, b := promote(admin, `{"host":"objects.githubusercontent.com","port":443,"network":"internal",`+why+`}`); code != 409 {
		t.Errorf("internal over a public policy entry = %d %v", code, b)
	}
	var audited int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'egress.allowance.promote'`).Scan(&audited)
	if audited != 1 {
		t.Errorf("promotion audit events = %d", audited)
	}
}
