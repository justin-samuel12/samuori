package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/cli"
)

const (
	configSecret   = "registry-password-value-7f3a"
	configSlackURL = "https://hooks.slack.com/services/T0/B0/slack-secret-9c1d"
	configRecipe   = `{"yaml":"id: shop-base\nversion: 1.0.0\ndisplayName: Shop base\nplatforms: [linux/arm64]\nstages:\n  - name: final\n    image: docker.io/library/node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392\n    steps:\n      - user: \"10001\"\n"}`
)

var secretLikeField = regexp.MustCompile(`(?i)(password|passwd|token|private.?key|signing.?key|api.?key|client.?secret|^secret$|secret.?(id|ref|value)|^value$|cert)`)

func emailReady(d *Deps) { d.Notify.EmailReady = true }

// seedConfiguration fills one organization with an item of every exported kind, plus secrets that must never leave it.
func seedConfiguration(t *testing.T, l *live, admin member, projA string) []string {
	t.Helper()
	ok := func(want int, method, path, payload string, extra map[string]string) body {
		t.Helper()
		st, _, b := l.json(t, admin, method, path, payload, extra)
		if st != want {
			t.Fatalf("%s %s = %d %v", method, path, st, b)
		}
		return b
	}
	ok(200, "PUT", "/identity-settings", `{"session":{"idleTimeoutMinutes":30,"maxSessionHours":8,"warningSeconds":120}}`, nil)
	ok(200, "PUT", "/audit-policy", `{"retentionDays":90,"exportEnabled":true,"requireExport":true}`, nil)
	for _, x := range ok(200, "GET", "/policies", "", nil)["items"].([]any) {
		if pol := x.(map[string]any); pol["kind"] == "validation" {
			ok(200, "PUT", "/policies/"+pol["id"].(string), `{"kind":"validation","document":{"requireSbom":true,"requireRuntimeTest":false}}`,
				map[string]string{"If-Match": `"` + jsonString(pol["version"]) + `"`})
		}
	}
	ok(200, "PUT", "/build-method-policy", `{"allowed":["dockerfile","railpack","recipe"],"default":"recipe"}`, nil)
	pool := ok(201, "POST", "/worker-pools", `{"name":"builders","isolationProfile":"rootless-dedicated-vm","trustDomain":"builders.example","location":"lab"}`, nil)
	ok(200, "PUT", "/providers/railpack", `{"enabled":false,"version":"","artifacts":{},"egressPresets":[],"pools":["`+pool["id"].(string)+`"],"projects":["`+projA+`"],"timeoutSeconds":600}`, nil)
	ok(204, "PUT", "/recipes/node-service/versions/1.0.0/status", `{"status":"deprecated","reason":"moving to 2.x"}`, nil)
	shipped := ok(200, "GET", "/recipes/node-service/versions/1.0.0/runtimes", "", nil)["shipped"].([]any)[0].(map[string]any)["version"].(string)
	ok(204, "PUT", "/recipes/node-service/versions/1.0.0/shipped-runtimes/"+shipped+"/status", `{"status":"withdrawn","reason":"end of life"}`, nil)
	node25 := "docker.io/library/node:25-bookworm-slim@sha256:" + strings.Repeat("b", 64)
	rt := ok(201, "POST", "/recipes/node-service/versions/1.0.0/runtimes", `{"label":"25","buildImage":"`+node25+`","runImage":"`+node25+`"}`, nil)
	ok(200, "POST", "/recipes/node-service/versions/1.0.0/runtimes/"+rt["id"].(string)+"/actions", `{"action":"approve","reason":"reviewed"}`, nil)
	ok(201, "POST", "/custom-recipes", configRecipe, nil)
	ok(200, "POST", "/custom-recipes/shop-base/versions/1.0.0/actions", `{"action":"approve","reason":"reviewed"}`, nil)

	var local string
	for _, x := range ok(200, "GET", "/secret-providers", "", nil)["items"].([]any) {
		if m := x.(map[string]any); m["type"] == "local" {
			local = m["id"].(string)
		}
	}
	sec := ok(201, "POST", "/secret-providers/"+local+"/local-secrets", `{"name":"lab-password","type":"text","value":"`+configSecret+`"}`, map[string]string{"Idempotency-Key": "config-seed-secret-1"})
	reg := ok(201, "POST", "/registries", `{"name":"lab","kind":"oci","endpoint":"registry.example:5000"}`, nil)["id"].(string)
	ok(201, "POST", "/registry-credential-profiles", `{"name":"lab-push","registryId":"`+reg+`","type":"oci-basic","purpose":"push","username":"ci","secretRef":"`+sec["id"].(string)+`"}`, nil)
	open := ok(201, "POST", "/registry-credential-profiles", `{"name":"lab-open","registryId":"`+reg+`","type":"anonymous","purpose":"push","repositoryScopes":["shop/*"]}`, nil)["id"].(string)
	ok(201, "POST", "/destinations", `{"name":"orders","registryId":"`+reg+`","repository":"shop/orders","pushProfileId":"`+open+`","allowedProjectIds":["`+projA+`"],"protectedTags":["v*"],"immutableTags":true}`, nil)

	mail := ok(201, "POST", "/notification-channels", `{"name":"ops-mail","kind":"email","recipients":["ops@example.com"]}`, nil)["id"].(string)
	ok(201, "POST", "/notification-channels", `{"name":"ops-slack","kind":"slack","secret":"`+configSlackURL+`"}`, nil)
	hook := ok(201, "POST", "/notification-channels", `{"name":"ops-hook","kind":"webhook","url":"https://hooks.example.com/samuori"}`, nil)
	var teamB string
	for _, x := range ok(200, "GET", "/teams", "", nil)["items"].([]any) {
		if m := x.(map[string]any); m["name"] == "team-b" {
			teamB = m["id"].(string)
		}
	}
	ok(201, "POST", "/notification-rules", `{"channelId":"`+mail+`","events":["build.failed"],"projectId":"`+projA+`"}`, nil)
	ok(201, "POST", "/notification-rules", `{"channelId":"`+mail+`","events":["validation.failed","build.failed"],"teamId":"`+teamB+`"}`, nil)

	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	tok := ok(201, "POST", "/me/api-tokens", `{"name":"seed","expiresAt":"`+expires+`","scopes":["read","write"]}`, nil)
	enrol := ok(201, "POST", "/enrollment-tokens", `{"ttlSeconds":3600}`, map[string]string{"Idempotency-Key": "config-seed-enrol-1"})
	values := []string{configSecret, configSlackURL, "correct horse battery", "a long secret phrase", hook["signingKey"].(string), tok["token"].(string)}
	for _, k := range []string{"token", "code"} {
		if s, isStr := enrol[k].(string); isStr && s != "" {
			values = append(values, s)
		}
	}
	return values
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }

func (l *live) raw(t *testing.T, m member, method, path, contentType string, payload []byte, extra map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, l.srv.URL+"/api/v1"+path, bytes.NewReader(payload))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set(auth.CSRFHeader, m.csrf[auth.CSRFHeader])
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := m.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out
}

func exportSpec(t *testing.T, l *live, admin member) (map[string]any, []byte) {
	t.Helper()
	st, h, out := l.raw(t, admin, "GET", "/configuration/export", "", nil, nil)
	if st != 200 || !strings.HasPrefix(h.Get("Content-Type"), "application/yaml") {
		t.Fatalf("export = %d %s %s", st, h.Get("Content-Type"), out)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("export is not YAML: %v\n%s", err, out)
	}
	return doc["spec"].(map[string]any), out
}

type importAnswer struct {
	DryRun   bool `json:"dryRun"`
	Complete bool `json:"complete"`
	Items    []struct {
		Section, Name, Action, Reason, Outcome, Error string
	} `json:"items"`
	NeedsAttention []struct{ Section, Name, Kind, Message, Link string } `json:"needsAttention"`
	Summary        struct{ Create, Update, Unchanged, Skip, Failed int } `json:"summary"`
}

func (a importAnswer) action(section, name string) string {
	for _, it := range a.Items {
		if it.Section == section && it.Name == name {
			return it.Action
		}
	}
	return "missing"
}

func (a importAnswer) attention(section, name string) string {
	for _, x := range a.NeedsAttention {
		if x.Section == section && x.Name == name {
			return x.Kind
		}
	}
	return ""
}

func count(t *testing.T, l *live, query string) int {
	t.Helper()
	var n int
	if err := l.db.SQL.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestConfigurationExportImport (SEC-CFG-01): an administrator's export carries every configuration section and no
// secret; importing it into a fresh organization previews, then reproduces it except for what holds a secret, which
// is listed instead; a second apply changes nothing; nothing is deleted and no user or role binding is created.
func TestConfigurationExportImport(t *testing.T) {
	src := newLiveWith(t, false, smallChunks(), emailReady)
	alice, _, projA, _ := twoTeams(t, src)
	srcAdmin := src.adminMember(t)
	secrets := seedConfiguration(t, src, srcAdmin, projA)

	spec, yml := exportSpec(t, src, srcAdmin)
	t.Run("export holds no secret", func(t *testing.T) {
		for _, v := range secrets {
			if strings.Contains(string(yml), v) {
				t.Errorf("export contains a seeded secret value %.6s…", v)
			}
		}
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, c := range x {
					// Outcome keys name rule classes, such as private-key, not fields.
					if secretLikeField.MatchString(k) && path != "/policies/sensitive-content/outcomes" {
						t.Errorf("export has a secret-like field %s/%s", path, k)
					}
					walk(path+"/"+k, c)
				}
			case []any:
				for _, c := range x {
					walk(path, c)
				}
			}
		}
		walk("", spec)
		sections := []string{"teams", "identity", "auditRetention", "policies", "buildMethods", "recipes", "workerPools", "registries", "credentialProfiles", "destinations", "notificationChannels", "notificationRules"}
		for _, section := range sections {
			if spec[section] == nil {
				t.Errorf("export has no %s", section)
			}
		}
		for section := range spec {
			if !slices.Contains(sections, section) {
				t.Errorf("export has an unexpected section %s", section)
			}
		}
		for _, absent := range []string{"lab-password", "alice", "bob", "username", "secretRef"} {
			if strings.Contains(string(yml), absent) {
				t.Errorf("export mentions %s", absent)
			}
		}
		_, again := exportSpec(t, src, srcAdmin)
		strip := regexp.MustCompile(`exportedAt: .*\n`)
		if !bytes.Equal(strip.ReplaceAll(yml, nil), strip.ReplaceAll(again, nil)) {
			t.Error("two exports differ")
		}
		st, h, js := src.raw(t, srcAdmin, "GET", "/configuration/export", "", nil, map[string]string{"Accept": "application/json"})
		var asJSON map[string]any
		if st != 200 || !strings.HasPrefix(h.Get("Content-Type"), "application/json") || json.Unmarshal(js, &asJSON) != nil || !reflect.DeepEqual(normalize(asJSON["spec"]), normalize(spec)) {
			t.Errorf("JSON export = %d %s", st, h.Get("Content-Type"))
		}
		if count(t, src, `SELECT COUNT(*) FROM audit_events WHERE action = 'configuration.export'`) != 3 {
			t.Error("exports are not audited")
		}
	})

	t.Run("administrators only", func(t *testing.T) {
		if st, _, _ := src.raw(t, alice, "GET", "/configuration/export", "", nil, nil); st != 403 {
			t.Errorf("developer export = %d", st)
		}
		if st, _, _ := src.raw(t, alice, "POST", "/configuration/import", "application/yaml", yml, nil); st != 403 {
			t.Errorf("developer import = %d", st)
		}
	})

	dst := newLiveWith(t, false, smallChunks(), emailReady)
	dst.setup(t)
	admin := dst.adminMember(t)
	_, _, team := dst.json(t, admin, "POST", "/teams", `{"name":"team-a"}`, nil)
	dst.json(t, admin, "POST", "/projects", `{"name":"app","teamId":"`+team["id"].(string)+`"}`, nil)
	dst.json(t, admin, "POST", "/registries", `{"name":"keep-me","kind":"oci","endpoint":"keep.example:5000"}`, nil)
	users, bindings := count(t, dst, `SELECT COUNT(*) FROM users`), count(t, dst, `SELECT COUNT(*) FROM role_bindings`)

	importFile := func(t *testing.T, query string, extra map[string]string) importAnswer {
		t.Helper()
		st, _, out := dst.raw(t, admin, "POST", "/configuration/import"+query, "application/yaml", yml, extra)
		if st != 200 {
			t.Fatalf("import%s = %d %s", query, st, out)
		}
		assertMatchesContract(t, "POST", "/configuration/import", st, http.Header{"Content-Type": {"application/json"}}, out)
		var a importAnswer
		_ = json.Unmarshal(out, &a)
		return a
	}

	t.Run("invalid files", func(t *testing.T) {
		for name, c := range map[string]struct {
			file, want string
			status     int
		}{
			"wrong kind":    {"apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\nmetadata: {}\nspec: {}\n", "/kind", 422},
			"unknown field": {"apiVersion: samuori.invalid/v1alpha1\nkind: Configuration\nmetadata: {}\nspec:\n  users: [alice]\n", "/spec", 422},
			"bad pool":      {"apiVersion: samuori.invalid/v1alpha1\nkind: Configuration\nmetadata: {}\nspec:\n  workerPools:\n    - {name: x, isolationProfile: privileged, trustDomain: x}\n", "/spec/workerPools/0/isolationProfile", 422},
			"not yaml":      {"{{{", "", 400},
		} {
			st, _, out := dst.raw(t, admin, "POST", "/configuration/import", "application/yaml", []byte(c.file), nil)
			if st != c.status || !strings.Contains(string(out), c.want) {
				t.Errorf("%s = %d %s", name, st, out)
			}
		}
	})

	t.Run("preview changes nothing", func(t *testing.T) {
		dst.json(t, admin, "GET", "/policies", "", nil)
		before := count(t, dst, `SELECT COUNT(*) FROM audit_events`)
		a := importFile(t, "", nil)
		if !a.DryRun || a.Summary.Create == 0 || a.Summary.Failed != 0 {
			t.Fatalf("preview = %+v", a.Summary)
		}
		for _, want := range []struct{ section, name, action string }{
			{"teams", "team-b", "create"},
			{"identity", "session timeouts", "update"},
			{"policies", "validation", "update"},
			{"policies", "egress", "unchanged"},
			{"workerPools", "builders", "create"},
			{"providers", "railpack", "update"},
			{"recipeStatuses", "node-service@1.0.0", "update"},
			{"organizationRuntimes", "node-service@1.0.0 runtime 25", "create"},
			{"customRecipes", "shop-base@1.0.0", "create"},
			{"registries", "lab", "create"},
			{"credentialProfiles", "lab-open", "create"},
			{"credentialProfiles", "lab-push", "skip"},
			{"destinations", "orders", "create"},
			{"notificationChannels", "ops-mail", "create"},
			{"notificationChannels", "ops-slack", "skip"},
			{"notificationChannels", "ops-hook", "skip"},
			{"notificationRules", "ops-mail: build.failed for team-a/app", "create"},
		} {
			if got := a.action(want.section, want.name); got != want.action {
				t.Errorf("%s %s = %s, want %s", want.section, want.name, got, want.action)
			}
		}
		if a.attention("credentialProfiles", "lab-push") != "credential" || a.attention("notificationChannels", "ops-slack") != "secret" || a.attention("notificationChannels", "ops-hook") != "secret" {
			t.Errorf("needs attention = %+v", a.NeedsAttention)
		}
		if count(t, dst, `SELECT COUNT(*) FROM registries`) != 1 || count(t, dst, `SELECT COUNT(*) FROM audit_events`) != before {
			t.Error("the preview changed something")
		}
	})

	t.Run("apply", func(t *testing.T) {
		if st, _, out := dst.raw(t, admin, "POST", "/configuration/import?dryRun=false", "application/yaml", yml, nil); st != 400 {
			t.Fatalf("apply without a key = %d %s", st, out)
		}
		a := importFile(t, "?dryRun=false", map[string]string{"Idempotency-Key": "config-import-000001"})
		if a.DryRun || !a.Complete || a.Summary.Failed != 0 {
			t.Fatalf("apply = %+v %+v", a.Summary, a.Items)
		}
		if st, _, _ := dst.raw(t, admin, "POST", "/configuration/import?dryRun=false", "application/yaml", yml, map[string]string{"Idempotency-Key": "config-import-000001"}); st != 409 {
			t.Errorf("replayed key = %d", st)
		}
		got, _ := exportSpec(t, dst, admin)
		for _, section := range []string{"teams", "identity", "auditRetention", "policies", "buildMethods", "recipes", "workerPools", "notificationRules"} {
			if !reflect.DeepEqual(normalize(got[section]), normalize(spec[section])) {
				t.Errorf("%s differs after import:\n got %v\nwant %v", section, got[section], spec[section])
			}
		}
		without := func(list any, drop ...string) []any {
			var out []any
			for _, x := range list.([]any) {
				if n := x.(map[string]any)["name"].(string); !slices.Contains(drop, n) {
					out = append(out, x)
				}
			}
			return out
		}
		for section, drop := range map[string][]string{"registries": {"keep-me"}, "credentialProfiles": {"lab-push"}, "destinations": nil} {
			if !reflect.DeepEqual(normalize(without(got[section], "keep-me")), normalize(without(spec[section], drop...))) {
				t.Errorf("%s differs after import:\n got %v\nwant %v", section, got[section], spec[section])
			}
		}
		if !reflect.DeepEqual(normalize(got["notificationChannels"]), normalize(without(spec["notificationChannels"], "ops-slack", "ops-hook"))) {
			t.Errorf("channels after import = %v", got["notificationChannels"])
		}
		if count(t, dst, `SELECT COUNT(*) FROM registries WHERE name = 'keep-me'`) != 1 {
			t.Error("the import deleted a registry missing from the file")
		}
		if count(t, dst, `SELECT COUNT(*) FROM users`) != users || count(t, dst, `SELECT COUNT(*) FROM role_bindings`) != bindings {
			t.Error("the import created users or role bindings")
		}
		if count(t, dst, `SELECT COUNT(*) FROM audit_events WHERE action = 'configuration.import'`) != 1 || count(t, dst, `SELECT COUNT(*) FROM audit_events WHERE action = 'registry.create'`) != 2 {
			t.Error("the import or its items are not audited")
		}

		again := importFile(t, "?dryRun=false", map[string]string{"Idempotency-Key": "config-import-000002"})
		if again.Summary.Create != 0 || again.Summary.Update != 0 || again.Summary.Failed != 0 || again.Summary.Unchanged == 0 {
			t.Errorf("second apply = %+v %+v", again.Summary, again.Items)
		}
	})

	t.Run("CLI", func(t *testing.T) {
		expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		_, _, tok := dst.json(t, admin, "POST", "/me/api-tokens", `{"name":"cli","expiresAt":"`+expires+`","scopes":["read","write"]}`, nil)
		envs := map[string]string{"SAMUORI_URL": dst.srv.URL, "SAMUORI_TOKEN": tok["token"].(string), "HOME": t.TempDir()}
		run := func(want int, args ...string) string {
			t.Helper()
			var out, errOut bytes.Buffer
			if code := cli.Run(context.Background(), args, func(k string) string { return envs[k] }, strings.NewReader(""), &out, &errOut); code != want {
				t.Fatalf("%v = %d, want %d\n%s\n%s", args, code, want, out.String(), errOut.String())
			}
			return out.String() + errOut.String()
		}
		file := filepath.Join(t.TempDir(), "configuration.yml")
		run(0, "config", "export", "-o", file)
		raw, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(raw), "kind: Configuration") {
			t.Fatalf("exported file = %v %s", err, raw)
		}
		if out := run(0, "config", "import", file); !strings.Contains(out, "unchanged") || !strings.Contains(out, "preview") {
			t.Errorf("preview output = %s", out)
		}
		if out := run(0, "config", "import", file, "--apply"); !strings.Contains(out, "applied") {
			t.Errorf("apply output = %s", out)
		}
		bad := filepath.Join(t.TempDir(), "bad.yml")
		_ = os.WriteFile(bad, []byte("apiVersion: samuori.invalid/v1alpha1\nkind: Configuration\nspec: {nope: 1}\n"), 0o600)
		if out := run(3, "config", "import", bad); !strings.Contains(out, "/spec") {
			t.Errorf("invalid file output = %s", out)
		}
	})
}

// normalize compares documents decoded from YAML and JSON alike.
func normalize(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
