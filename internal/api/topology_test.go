package api

import (
	"strings"
	"testing"
)

// TestTopologyPlan stores a plan and reports the planned and measured tiers (ADR 0046).
func TestTopologyPlan(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	l.setup(t)
	admin := l.adminMember(t)

	st, _, b := l.json(t, admin, "GET", "/system/topology", "", nil)
	if st != 200 || b["plan"] != nil {
		t.Fatalf("before a plan = %d %v", st, b)
	}
	if tier := b["live"].(map[string]any)["tier"]; tier != "standalone" {
		t.Errorf("a single SQLite server with local storage measures %v, want standalone", tier)
	}

	if st, _, b := l.json(t, admin, "PUT", "/system/topology", `{"deployment":"single-server","controllers":3,"workers":1,"databaseReplicated":false,"storageReplicated":false}`, nil); st != 422 {
		t.Errorf("single server with three controllers = %d %v, want 422", st, b)
	}

	st, _, b = l.json(t, admin, "PUT", "/system/topology", `{"deployment":"cluster","controllers":3,"workers":2,"databaseReplicated":true,"storageReplicated":false}`, nil)
	if st != 200 {
		t.Fatalf("set plan = %d %v", st, b)
	}
	planned := b["planned"].(map[string]any)
	var gaps []string
	for _, c := range planned["components"].([]any) {
		if m := c.(map[string]any); m["tolerates"].(float64) == 0 {
			gaps = append(gaps, m["name"].(string))
		}
	}
	if planned["tier"] != "partial-ha" || strings.Join(gaps, ",") != "entry point" {
		t.Errorf("a three-controller plan reached from a loopback portal URL: tier %v, gaps %v; want partial-ha, entry point", planned["tier"], gaps)
	}
	if b["live"].(map[string]any)["tier"] != "standalone" {
		t.Error("the measured tier must not follow the plan")
	}

	_, _, b = l.json(t, admin, "PUT", "/system/topology", `{"deployment":"cluster","controllers":4,"workers":2,"databaseReplicated":true,"storageReplicated":false}`, nil)
	if w, _ := b["planned"].(map[string]any)["warnings"].([]any); len(w) != 1 {
		t.Errorf("four controllers: warnings %v", w)
	}
	var n int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'topology.plan'`).Scan(&n)
	if n != 2 {
		t.Errorf("audited plan changes = %d, want 2", n)
	}
}
