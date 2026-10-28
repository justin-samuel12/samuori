package api

import (
	"fmt"
	"slices"
	"testing"

	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/workerjoin"
)

// TestWorkerJoinCodeIssuing checks that only administrators get a worker join code, that it carries the same
// single-use token and CA pin as the separate fields, and that none is offered when remote workers cannot connect.
func TestWorkerJoinCodeIssuing(t *testing.T) {
	t.Run("SEC-WRK-11", func(t *testing.T) {
		l := newLiveWith(t, false, sources.DefaultConfig(), func(d *Deps) {
			d.WorkerJoin = WorkerJoin{Listen: "0.0.0.0:8444", Hostnames: []string{"localhost", "127.0.0.1", "samuori.example"}}
		})
		l.setup(t)
		admin := l.adminMember(t)
		idem := func(k string) map[string]string {
			return map[string]string{"Idempotency-Key": "worker-join-" + k + "-00000001"}
		}

		st, h, b := l.json(t, admin, "POST", "/enrollment-tokens", `{"ttlSeconds":3600}`, idem("a"))
		if st != 201 || h.Get("Cache-Control") != "no-store" {
			t.Fatalf("issue = %d %v %v", st, h, b)
		}
		raw, _ := b["code"].(string)
		c, err := workerjoin.Decode(raw)
		if err != nil {
			t.Fatalf("code %q: %v", raw, err)
		}
		if c.Token != b["token"] || c.Pin != b["caPin"] || !slices.Contains(c.Controllers, "samuori.example:8444") || slices.Contains(c.Controllers, "localhost:8444") {
			t.Fatalf("code = %+v, issued %v", c, b)
		}

		var org string
		_ = l.db.SQL.QueryRow(`SELECT id FROM organizations`).Scan(&org)
		if st, _, b := l.json(t, admin, "POST", "/users", `{"username":"ops","password":"a long secret phrase"}`, nil); st != 201 {
			t.Fatalf("user = %d %v", st, b)
		} else if st, _, _ := l.json(t, admin, "POST", "/role-bindings", fmt.Sprintf(`{"principalId":%q,"role":"operator","scopeType":"organization","scopeId":%q}`, b["id"], org), nil); st != 201 {
			t.Fatalf("bind = %d", st)
		}
		c2 := l.client()
		resp := l.call(t, c2, "POST", "/auth/login", `{"username":"ops","password":"a long secret phrase"}`, nil)
		ops := member{c2, map[string]string{}}
		for _, ck := range resp.Cookies() {
			if ck.Name == auth.CSRFCookie {
				ops.csrf[auth.CSRFHeader] = ck.Value
			}
		}
		if st, _, b := l.json(t, ops, "POST", "/enrollment-tokens", `{"ttlSeconds":3600}`, idem("b")); st != 403 || b["code"] != "forbidden" || b["token"] != nil {
			t.Fatalf("operator issue = %d %v, want 403 and nothing issued", st, b)
		}
	})

	t.Run("no code for a loopback-only worker endpoint", func(t *testing.T) {
		l := newLiveWith(t, false, sources.DefaultConfig(), func(d *Deps) {
			d.WorkerJoin = WorkerJoin{Listen: "127.0.0.1:8444", Hostnames: []string{"localhost"}}
		})
		l.setup(t)
		st, _, b := l.json(t, l.adminMember(t), "POST", "/enrollment-tokens", `{"ttlSeconds":3600}`, map[string]string{"Idempotency-Key": "worker-join-c-00000001"})
		if st != 201 || b["code"] != nil || b["token"] == nil {
			t.Fatalf("issue = %d %v, want a token without a code", st, b)
		}
	})
}
