package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDeleteSecret(t *testing.T) {
	t.Run("SEC-SCR-09", func(t *testing.T) {
		ctx := context.Background()
		f := newFixture(t)
		s, admin := f.s, f.admin
		local, err := s.LocalProvider(ctx, "o1")
		if err != nil {
			t.Fatal(err)
		}
		sec, err := s.CreateSecret(ctx, admin, "npm-token", "text", "", []byte("v1"), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Rotate(ctx, admin, sec.ID, sec.Version, []byte("v2"), ""); err != nil {
			t.Fatal(err)
		}
		inUse := func(label, want string) {
			t.Helper()
			var e *InUse
			err := s.DeleteSecret(ctx, admin, local.ID, sec.ID, "")
			if !errors.As(err, &e) || !strings.Contains(e.Error(), want) {
				t.Fatalf("%s: delete = %v, want in use (%s)", label, err, want)
			}
		}

		if err := s.DeleteSecret(ctx, f.dev, local.ID, sec.ID, ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("a developer deleted a secret: %v", err)
		}
		g, err := s.CreateGrant(ctx, admin, GrantRequest{ProjectID: "p1", Name: "npm", ProviderID: local.ID, ProviderReference: sec.ID, Usage: "build", Reason: "packages"}, "")
		if err != nil {
			t.Fatal(err)
		}
		inUse("granted", `granted to project orders as "npm"`)
		if err := s.RevokeGrant(ctx, admin, "p1", g.ID, "done", ""); err != nil {
			t.Fatal(err)
		}
		b, err := s.Bind(ctx, admin, BindRequest{ProjectID: "p2", LogicalID: "npm_token", ProviderID: local.ID, ProviderReference: sec.ID, Reason: "direct"}, "")
		if err != nil {
			t.Fatal(err)
		}
		inUse("bound directly", "bound in project billing as npm_token")
		if _, err := s.Disable(ctx, admin, b.ID, "done", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.SQL.ExecContext(ctx, f.db.Rebind(`UPDATE secret_providers SET credential_secret_id = ? WHERE id = ?`), sec.ID, local.ID); err != nil {
			t.Fatal(err)
		}
		inUse("a Samuori credential", "used by Samuori as an external secret store's credential")
		if _, err := f.db.SQL.ExecContext(ctx, f.db.Rebind(`UPDATE secret_providers SET credential_secret_id = NULL WHERE id = ?`), local.ID); err != nil {
			t.Fatal(err)
		}

		if err := s.DeleteSecret(ctx, admin, local.ID, sec.ID, ""); err != nil {
			t.Fatalf("delete after every use ended: %v", err)
		}
		var versions int
		if err := f.db.SQL.QueryRowContext(ctx, f.db.Rebind(`SELECT COUNT(*) FROM secret_versions WHERE secret_id = ?`), sec.ID).Scan(&versions); err != nil || versions != 0 {
			t.Fatalf("%d encrypted versions remain: %v", versions, err)
		}
		if _, err := s.Value(ctx, "o1", b.ID, b.Version, "build"); err == nil {
			t.Error("a deleted secret was released")
		}
		if err := s.DeleteSecret(ctx, admin, local.ID, sec.ID, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleting twice = %v, want not found", err)
		}
	})

	t.Run("leftover controller-only secret", func(t *testing.T) {
		ctx := context.Background()
		f := newFixture(t)
		s, admin := f.s, f.admin
		local, err := s.LocalProvider(ctx, "o1")
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.CreateControllerSecret(ctx, admin, "provider-credential", "credential", []byte("v1"), "")
		if err != nil {
			t.Fatal(err)
		}
		uses := func() []string {
			t.Helper()
			list, err := s.ListSecrets(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range list {
				if x.ID == id {
					return x.SamuoriUses
				}
			}
			t.Fatal("the secret is not listed")
			return nil
		}
		bindable := func(label string) {
			t.Helper()
			var inv *Invalid
			if _, err := s.Bind(ctx, admin, BindRequest{ProjectID: "p1", LogicalID: "cred", ProviderID: local.ID, ProviderReference: id, Reason: "try"}, ""); !errors.As(err, &inv) {
				t.Errorf("%s: binding a controller-only secret = %v", label, err)
			}
			if _, err := s.CreateGrant(ctx, admin, GrantRequest{ProjectID: "p1", Name: "cred", ProviderID: local.ID, ProviderReference: id, Usage: "build", Reason: "try"}, ""); !errors.As(err, &inv) {
				t.Errorf("%s: granting a controller-only secret = %v", label, err)
			}
		}

		if _, err := f.db.SQL.ExecContext(ctx, f.db.Rebind(`UPDATE secret_providers SET credential_secret_id = ? WHERE id = ?`), id, local.ID); err != nil {
			t.Fatal(err)
		}
		if u := uses(); len(u) != 1 {
			t.Errorf("uses while referenced = %v", u)
		}
		bindable("referenced")
		var e *InUse
		if err := s.DeleteSecret(ctx, admin, local.ID, id, ""); !errors.As(err, &e) {
			t.Fatalf("delete while referenced = %v, want in use", err)
		}
		if _, err := f.db.SQL.ExecContext(ctx, f.db.Rebind(`UPDATE secret_providers SET credential_secret_id = NULL WHERE id = ?`), local.ID); err != nil {
			t.Fatal(err)
		}
		if u := uses(); len(u) != 0 {
			t.Errorf("uses once unreferenced = %v", u)
		}
		bindable("unreferenced")
		if err := s.DeleteSecret(ctx, f.dev, local.ID, id, ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("a developer deleted a leftover secret: %v", err)
		}
		if err := s.DeleteSecret(ctx, admin, local.ID, id, ""); err != nil {
			t.Fatalf("delete a leftover secret: %v", err)
		}
	})
}
