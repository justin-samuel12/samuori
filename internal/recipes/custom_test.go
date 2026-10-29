package recipes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/engine/render"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

const recipe = `
id: tooling-base
version: 1.0.0
displayName: Tooling base
platforms: [linux/amd64]
egressPresets: [ubuntu]
secrets: [npm_token, optional_token]
parameters:
  - {name: tag, type: string, default: base}
stages:
  - name: build
    image: docker.io/library/ubuntu:24.04@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    steps:
      - run: {shell: "apt-get update", network: egress, secrets: [npm_token, optional_token]}
      - label: {name: org.opencontainers.image.version, value: "${tag}"}
      - user: "10001"
`

func TestCustomRecipeLifecycle(t *testing.T) {
	t.Run("SEC-SPC-07", func(t *testing.T) {
		ctx := context.Background()
		d, err := db.Open(ctx, dbtest.SQLite(t, t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		if _, err := d.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := d.SQL.ExecContext(ctx, `INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		cat, err := LoadBuiltin()
		if err != nil {
			t.Fatal(err)
		}
		s := NewStatuses(d, cat)
		admin := authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
		maintainer := authz.Principal{ID: "m", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Maintainer, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}

		if _, err := s.CreateCustom(ctx, maintainer, []byte(recipe), ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("a maintainer created a recipe: %v", err)
		}
		var inv *Invalid
		if _, err := s.CreateCustom(ctx, admin, []byte(strings.Replace(recipe, "id: tooling-base", "id: node-service", 1)), ""); !errors.As(err, &inv) {
			t.Errorf("a custom recipe took a built-in ID: %v", err)
		}
		if _, err := s.CreateCustom(ctx, admin, []byte(strings.Replace(recipe, "[ubuntu]", "[nowhere]", 1)), ""); !errors.As(err, &inv) {
			t.Errorf("an unknown egress preset was accepted: %v", err)
		}
		if _, err := s.CreateCustom(ctx, admin, []byte(recipe), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateCustom(ctx, admin, []byte(recipe), ""); !errors.Is(err, ErrExists) {
			t.Errorf("the same version was created twice: %v", err)
		}
		if _, _, err := s.Get(ctx, "o1", "tooling-base", "1.0.0"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a draft is usable by builds: %v", err)
		}
		if _, err := s.DecideCustom(ctx, admin, "tooling-base", "1.0.0", "approve", "", ""); !errors.As(err, &inv) {
			t.Error("approved without a reason")
		}
		if _, err := s.DecideCustom(ctx, maintainer, "tooling-base", "1.0.0", "approve", "looks right", ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("a maintainer approved a recipe: %v", err)
		}
		if _, err := s.DecideCustom(ctx, admin, "tooling-base", "1.0.0", "approve", "reviewed the preview", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReplaceDraft(ctx, admin, "tooling-base", "1.0.0", []byte(recipe), ""); !errors.Is(err, ErrNotDraft) {
			t.Errorf("an approved version was changed: %v", err)
		}
		if err := s.DiscardDraft(ctx, admin, "tooling-base", "1.0.0", ""); !errors.Is(err, ErrNotDraft) {
			t.Errorf("an approved version was discarded: %v", err)
		}
		draft2 := strings.Replace(recipe, "version: 1.0.0", "version: 1.1.0", 1)
		if _, err := s.CreateCustom(ctx, admin, []byte(draft2), ""); err != nil {
			t.Fatal(err)
		}
		if err := s.DiscardDraft(ctx, maintainer, "tooling-base", "1.1.0", ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("a maintainer discarded a draft: %v", err)
		}
		if err := s.DiscardDraft(ctx, admin, "tooling-base", "1.1.0", ""); err != nil {
			t.Fatal(err)
		}
		if err := s.DiscardDraft(ctx, admin, "tooling-base", "1.1.0", ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("a discarded draft is still there: %v", err)
		}
		var discards int
		if err := d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action = 'recipe.discard' AND resource_id = 'tooling-base@1.1.0'`).Scan(&discards); err != nil || discards != 1 {
			t.Errorf("discard audit records = %d, %v", discards, err)
		}
		r, st, err := s.Get(ctx, "o1", "tooling-base", "1.0.0")
		if err != nil || r.Custom == nil || st.Status != Approved {
			t.Fatalf("approved recipe = %+v, %+v, %v", r, st, err)
		}
		all, _, err := s.All(ctx, "o1")
		if err != nil || !strings.Contains(func() string {
			var ids []string
			for _, x := range all {
				ids = append(ids, x.ID)
			}
			return strings.Join(ids, ",")
		}(), "tooling-base") {
			t.Errorf("the approved recipe is not listed: %v", err)
		}

		plan := buildplan.Plan{Method: "recipe", BuildArgs: map[string]string{"tag": "release"},
			Secrets: []buildplan.PlanSecret{{ID: "npm_token", Required: true, Target: "/run/secrets/npm_token", BindingID: "b1"}, {ID: "optional_token", Target: "/run/secrets/optional_token"}},
			Recipe:  &buildplan.PlanRecipe{ID: r.ID, Version: r.Version, Custom: r.Custom, Parameters: map[string]string{"tag": "release"}}}
		df, err := render.Dockerfile(plan)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(df), "--mount=type=secret,id=npm_token,") || strings.Contains(string(df), "optional_token") {
			t.Errorf("secret mounts are wrong; only bound secrets are mounted:\n%s", df)
		}

		if _, err := s.DecideCustom(ctx, admin, "tooling-base", "1.0.0", "approve", "again", ""); !errors.As(err, &inv) {
			t.Error("an approved version was approved again")
		}
		if _, err := s.DecideCustom(ctx, admin, "tooling-base", "1.0.0", "withdraw", "base image retired", ""); err != nil {
			t.Fatal(err)
		}
		if _, st, _ := s.Get(ctx, "o1", "tooling-base", "1.0.0"); st.Status != Withdrawn {
			t.Errorf("status after withdraw = %q", st.Status)
		}
	})
}
