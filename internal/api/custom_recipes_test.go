package api

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/auth"
)

func TestCustomRecipeAPI(t *testing.T) {
	t.Run("SEC-SPC-07", func(t *testing.T) {
		l := newLive(t, false)
		l.setup(t)
		admin, csrf := l.login(t)
		h := map[string]string{auth.CSRFHeader: csrf}
		def := `id: anything-base
version: 1.0.0
displayName: Anything
platforms: [linux/arm64]
stages:
  - name: final
    image: docker.io/library/node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392
    steps:
      - user: "10001"
`
		body, _ := json.Marshal(map[string]string{"yaml": def})
		if r := l.call(t, admin, "POST", "/custom-recipes", string(body), h); r.StatusCode != 201 {
			raw, _ := io.ReadAll(r.Body)
			t.Fatalf("create = %d: %s", r.StatusCode, raw)
		}
		var preview struct {
			Valid      bool   `json:"valid"`
			Dockerfile string `json:"dockerfile"`
		}
		_ = json.NewDecoder(l.call(t, admin, "POST", "/custom-recipes/preview", string(body), h).Body).Decode(&preview)
		if !preview.Valid || !strings.Contains(preview.Dockerfile, "USER 10001") {
			t.Fatalf("preview = %+v", preview)
		}
		if r := l.call(t, admin, "POST", "/custom-recipes/anything-base/versions/1.0.0/actions", `{"action":"approve","reason":"reviewed"}`, h); r.StatusCode != 200 {
			t.Fatalf("approve = %d", r.StatusCode)
		}
		if r := l.call(t, admin, "DELETE", "/custom-recipes/anything-base/versions/1.0.0", "", h); r.StatusCode != 409 {
			t.Errorf("discarding an approved version = %d, want 409", r.StatusCode)
		}
		var list struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		_ = json.NewDecoder(l.call(t, admin, "GET", "/recipes", "", nil).Body).Decode(&list)
		found := false
		for _, it := range list.Items {
			for _, k := range []string{"languages", "applicationTypes", "platforms", "runtimes"} {
				if string(it[k]) == "null" {
					t.Errorf("recipe %s has %s: null; the contract requires a list", it["id"], k)
				}
			}
			if string(it["id"]) == `"anything-base"` {
				found = string(it["custom"]) == "true"
			}
		}
		if !found {
			t.Error("the approved custom recipe is not listed as custom")
		}
	})
}
