package api

import (
	"fmt"
	"sync"
	"testing"
)

func TestRepositoryRequests(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, projB := twoTeams(t, l)
	admin := l.adminMember(t)

	st, _, req := l.json(t, alice, "POST", "/projects/"+projA+"/repository-requests", `{"repository":"payments/orders-api","justification":"the orders service needs its own repository"}`, nil)
	if st != 202 || req["state"] != "pending" {
		t.Fatalf("request = %d %v", st, req)
	}
	approval := req["approvalId"].(string)
	for name, body := range map[string]string{
		"upper case":       `{"repository":"Payments/API","justification":"long enough reason"}`,
		"short reason":     `{"repository":"payments/api","justification":"short"}`,
		"unknown registry": `{"repository":"payments/api","registryId":"01JAAAAAAAAAAAAAAAAAAAAAAA","justification":"long enough reason"}`,
	} {
		if st, _, _ := l.json(t, alice, "POST", "/projects/"+projA+"/repository-requests", body, nil); st != 422 && st != 400 {
			t.Errorf("%s = %d", name, st)
		}
	}
	if st, _, _ := l.json(t, bob, "POST", "/projects/"+projA+"/repository-requests", `{"repository":"x/y","justification":"long enough reason"}`, nil); st != 404 {
		t.Fatalf("another team requested for project A: %d", st)
	}
	if st, _, _ := l.json(t, bob, "GET", "/projects/"+projA+"/repository-requests", "", nil); st != 404 {
		t.Fatalf("another team listed project A's requests: %d", st)
	}

	_, _, reg := l.json(t, admin, "POST", "/registries", `{"name":"lab","kind":"oci","endpoint":"registry.example:5000"}`, nil)
	_, _, prof := l.json(t, admin, "POST", "/registry-credential-profiles", `{"name":"lab-push","registryId":"`+reg["id"].(string)+`","type":"anonymous","purpose":"push"}`, nil)
	dest := func(name, project string) string {
		st, _, d := l.json(t, admin, "POST", "/destinations", `{"name":"`+name+`","registryId":"`+reg["id"].(string)+`","repository":"payments/orders-api","pushProfileId":"`+prof["id"].(string)+`","allowedProjectIds":["`+project+`"]}`, nil)
		if st != 201 {
			t.Fatalf("destination = %d %v", st, d)
		}
		return d["id"].(string)
	}
	other, mine := dest("orders-b", projB), dest("orders-a", projA)

	if st, _, b := l.json(t, admin, "POST", "/approvals/"+approval+"/decision", `{"decision":"approve","reason":"approving without a destination"}`, map[string]string{"Idempotency-Key": "decide-0000000000001"}); st != 409 || b["code"] != "unfulfilled" {
		t.Fatalf("plain approval = %d %v", st, b)
	}
	if st, _, _ := l.json(t, alice, "POST", "/approvals/"+approval+"/fulfillment", `{"destinationId":"`+mine+`","reason":"fulfilling my own request"}`, nil); st != 403 {
		t.Fatalf("a developer fulfilled the request: %d", st)
	}
	if st, _, _ := l.json(t, admin, "POST", "/approvals/"+approval+"/fulfillment", `{"destinationId":"`+other+`","reason":"wrong project destination"}`, nil); st != 422 {
		t.Fatalf("a destination closed to the project = %d", st)
	}
	st, _, ap := l.json(t, admin, "POST", "/approvals/"+approval+"/fulfillment", `{"destinationId":"`+mine+`","reason":"created payments/orders-api for the team"}`, nil)
	if st != 200 || ap["state"] != "approved" {
		t.Fatalf("fulfill = %d %v", st, ap)
	}
	_, _, list := l.json(t, alice, "GET", "/projects/"+projA+"/repository-requests", "", nil)
	got := list["items"].([]any)[0].(map[string]any)
	if got["state"] != "approved" || got["destinationName"] != "orders-a" {
		t.Fatalf("request after fulfillment = %v", got)
	}
	if st, _, _ := l.json(t, admin, "POST", "/approvals/"+approval+"/fulfillment", `{"destinationId":"`+mine+`","reason":"a second fulfillment"}`, nil); st != 409 {
		t.Fatalf("second fulfillment = %d", st)
	}

	t.Run("concurrent fulfillments keep the winner's destination", func(t *testing.T) {
		alt := dest("orders-a2", projA)
		names := map[string]string{mine: "orders-a", alt: "orders-a2"}
		for round := range 5 {
			_, _, req := l.json(t, alice, "POST", "/projects/"+projA+"/repository-requests", fmt.Sprintf(`{"repository":"payments/race-%d","justification":"a request two administrators fulfil"}`, round), nil)
			approval := req["approvalId"].(string)
			var wg sync.WaitGroup
			codes := map[string]int{}
			var mu sync.Mutex
			for _, d := range []string{mine, alt} {
				wg.Go(func() {
					st, _, _ := l.json(t, admin, "POST", "/approvals/"+approval+"/fulfillment", `{"destinationId":"`+d+`","reason":"fulfilling at the same time"}`, nil)
					mu.Lock()
					codes[d] = st
					mu.Unlock()
				})
			}
			wg.Wait()
			winner := ""
			for d, st := range codes {
				if st == 200 {
					if winner != "" {
						t.Fatalf("both fulfillments succeeded: %v", codes)
					}
					winner = d
				}
			}
			if winner == "" {
				t.Fatalf("no fulfillment succeeded: %v", codes)
			}
			_, _, list := l.json(t, alice, "GET", "/projects/"+projA+"/repository-requests", "", nil)
			for _, it := range list["items"].([]any) {
				if m := it.(map[string]any); m["approvalId"] == approval && m["destinationName"] != names[winner] {
					t.Fatalf("the request links %v, but %s was approved", m["destinationName"], names[winner])
				}
			}
		}
	})
}
