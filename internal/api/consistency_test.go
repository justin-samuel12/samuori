package api

import (
	"io"
	"net/http"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"samuori.invalid/samuori/api/openapi"
	"samuori.invalid/samuori/internal/api/apigen"
)

// Lists that are not paginated, each bounded by what it lists.
var unpagedLists = map[string]string{
	"listSignInProviders":       "public sign-in choices, a handful per installation",
	"listBuildAttempts":         "bounded by the build's retry limit",
	"getBuildBuildpacks":        "one build's buildpack lineage",
	"getBuildRailpack":          "one build's Railpack lineage",
	"listHelpTopics":            "built-in help shipped with the release",
	"getKubeConversionFindings": "one revision's findings, bounded by its manifests",
	"listKubeProviders":         "built-in Kubernetes provider catalogue",
	"listProviders":             "built-in secret provider catalogue",
	"listEcrRepositories":       "live view of a registry, read whole from it",
	"listSpecSuggestions":       "computed per request from one discovery, at most 20",
}

// Creates without an Idempotency-Key parameter; none today.
var createsWithoutIdempotency = map[string]string{}

func TestContractConsistency(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(openapi.Document)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			params := map[string]bool{}
			for _, ps := range []openapi3.Parameters{item.Parameters, op.Parameters} {
				for _, p := range ps {
					params[p.Value.Name] = true
				}
			}
			name := method + " " + path + " (" + op.OperationID + ")"
			for code, resp := range op.Responses.Map() {
				if code[0] == '2' || code[0] == '3' {
					continue
				}
				if !refersToProblem(resp) {
					t.Errorf("%s: %s does not answer with Problem", name, code)
				}
				if code == "429" && resp.Value.Headers["Retry-After"] == nil {
					t.Errorf("%s: 429 does not declare Retry-After", name)
				}
			}
			if method == http.MethodGet && isList(op) {
				if reason, ok := unpagedLists[op.OperationID]; ok {
					seen[op.OperationID] = reason != ""
				} else if !params["limit"] || !params["cursor"] || !hasNextCursor(op) {
					t.Errorf("%s: a list needs limit, cursor and nextCursor (or an entry in unpagedLists)", name)
				}
			}
			if method == http.MethodPost && op.Responses.Value("201") != nil {
				if reason, ok := createsWithoutIdempotency[op.OperationID]; ok {
					seen[op.OperationID] = reason != ""
				} else if !params["Idempotency-Key"] {
					t.Errorf("%s: a create needs Idempotency-Key (or an entry in createsWithoutIdempotency)", name)
				}
			}
		}
	}
	for _, allow := range []map[string]string{unpagedLists, createsWithoutIdempotency} {
		for id := range allow {
			if !seen[id] {
				t.Errorf("allowlist entry %s is stale or has no reason", id)
			}
		}
	}
}

func refersToProblem(r *openapi3.ResponseRef) bool {
	if r.Ref == "#/components/responses/Problem" || r.Ref == "#/components/responses/RateLimited" {
		return true
	}
	mt := r.Value.Content.Get("application/json")
	return mt != nil && mt.Schema != nil && mt.Schema.Ref == "#/components/schemas/Problem"
}

func okSchema(op *openapi3.Operation) *openapi3.Schema {
	r := op.Responses.Value("200")
	if r == nil || r.Value == nil {
		return nil
	}
	mt := r.Value.Content.Get("application/json")
	if mt == nil || mt.Schema == nil {
		return nil
	}
	return mt.Schema.Value
}

func isList(op *openapi3.Operation) bool {
	s := okSchema(op)
	if s == nil {
		return false
	}
	if s.Type.Is("array") {
		return true
	}
	items, ok := s.Properties["items"]
	return ok && items.Value.Type.Is("array")
}

func hasNextCursor(op *openapi3.Operation) bool {
	_, ok := okSchema(op).Properties["nextCursor"]
	return ok
}

func TestCreateIdempotencyKeyReplays(t *testing.T) {
	l := newLive(t, false)
	l.setup(t)
	admin := l.signIn(t, "admin", "correct horse battery")
	post := func(key, body string) (int, []byte) {
		t.Helper()
		resp := l.call(t, admin.c, "POST", "/teams", body, map[string]string{"X-CSRF-Token": admin.csrf, "Idempotency-Key": key})
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}

	code, body := post("team-key-0000000001", `{"name":"alpha"}`)
	if code != http.StatusCreated {
		t.Fatalf("first = %d %s", code, body)
	}
	team := decode[apigen.Team](t, body)
	code, body = post("team-key-0000000001", `{"name":"alpha"}`)
	p := decode[apigen.Problem](t, body)
	if code != http.StatusConflict || p.Code != "idempotent_replay" || p.Details == nil || *(*p.Details)[0].Reason != team.Id {
		t.Fatalf("replay = %d %s, want 409 idempotent_replay naming %s", code, body, team.Id)
	}
	var teams int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM teams WHERE name = 'alpha'`).Scan(&teams)
	if teams != 1 {
		t.Fatalf("teams named alpha = %d, want 1", teams)
	}

	if code, body = post("team-key-0000000002", `{"name":"alpha"}`); code != http.StatusConflict || decode[apigen.Problem](t, body).Code == "idempotent_replay" {
		t.Fatalf("duplicate name = %d %s, want the handler's 409", code, body)
	}
	if code, body = post("team-key-0000000002", `{"name":"beta"}`); code != http.StatusCreated {
		t.Fatalf("key reused after a rejected request = %d %s, want 201", code, body)
	}
	if code, _ = post("team-key-0000000001", `{"name":"gamma"}`); code != http.StatusConflict {
		t.Fatalf("other body, same key = %d, want 409", code)
	}
	admin.want(t, 201, "POST", "/teams", `{"name":"delta"}`)
}

func TestPaginationOfWholeLists(t *testing.T) {
	l := newLive(t, false)
	l.setup(t)
	admin := l.signIn(t, "admin", "correct horse battery")
	for _, n := range []string{"t-one", "t-two", "t-three"} {
		admin.want(t, 201, "POST", "/teams", `{"name":"`+n+`"}`)
	}
	all := decode[apigen.TeamList](t, admin.want(t, 200, "GET", "/teams", ""))
	if all.NextCursor != nil || len(all.Items) < 3 {
		t.Fatalf("without limit = %d items, cursor %v; want every item", len(all.Items), all.NextCursor)
	}
	var got []string
	cursor := ""
	for range len(all.Items) + 1 {
		q := "/teams?limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		page := decode[apigen.TeamList](t, admin.want(t, 200, "GET", q, ""))
		for _, it := range page.Items {
			got = append(got, it.Id)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(got) != len(all.Items) {
		t.Fatalf("paged %d items, whole list has %d", len(got), len(all.Items))
	}
	for i := range got {
		if got[i] != all.Items[i].Id {
			t.Fatalf("page order differs at %d", i)
		}
	}
	admin.want(t, 400, "GET", "/teams?limit=2&cursor=unknown", "")
}
