package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/tenancy"
)

func TestDevWeakPasswords(t *testing.T) {
	t.Run("SEC-AUTH-10", func(t *testing.T) {
		ctx := context.Background()
		open := func(devWeak bool) *Service {
			d, err := db.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { d.Close() })
			if _, err := d.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			p := DefaultPolicy()
			p.DevWeakPasswords = devWeak
			return NewService(d, tenancy.NewStore(d), p)
		}
		weak := BootstrapInput{OrganizationName: "Dev", Username: "admin", Password: "admin1"}

		strict := open(false)
		if err := strict.Bootstrap(ctx, weak, t.TempDir(), "", false); !errors.Is(err, ErrWeakPassword) {
			t.Fatalf("a weak administrator password was accepted under the normal policy: %v", err)
		}
		good := weak
		good.Password = "a long enough passphrase"
		good.Email = "admin@example.test"
		if err := strict.Bootstrap(ctx, good, t.TempDir(), "", false); err != nil {
			t.Fatal(err)
		}
		var email string
		if err := strict.db.SQL.QueryRow(`SELECT email FROM users WHERE username = 'admin'`).Scan(&email); err != nil || email != good.Email {
			t.Fatalf("bootstrap stored email %q: %v", email, err)
		}
		admin := adminOf(t, strict)
		if _, err := strict.CreateLocalUser(ctx, admin, "dana", "", "dana1", ""); !errors.Is(err, ErrWeakPassword) {
			t.Fatalf("a weak user password was accepted under the normal policy: %v", err)
		}

		dev := open(true)
		empty := weak
		empty.Password = ""
		if err := dev.Bootstrap(ctx, empty, t.TempDir(), "", false); !errors.Is(err, ErrWeakPassword) {
			t.Fatalf("an empty password was accepted on a development server: %v", err)
		}
		if err := dev.Bootstrap(ctx, weak, t.TempDir(), "", false); err != nil {
			t.Fatalf("the development server refused admin1: %v", err)
		}
		if _, err := dev.CreateLocalUser(ctx, adminOf(t, dev), "dana", "", "dana1", ""); err != nil {
			t.Fatalf("the development server refused dana1: %v", err)
		}
		for user, pw := range map[string]string{"admin": "admin1", "dana": "dana1"} {
			if _, err := dev.Login(ctx, "", user, pw, "127.0.0.1", ""); err != nil {
				t.Errorf("signing in as %s: %v", user, err)
			}
		}
	})
}

func adminOf(t *testing.T, s *Service) authz.Principal {
	t.Helper()
	var userID, orgID string
	if err := s.db.SQL.QueryRow(`SELECT u.id, u.org_id FROM users u WHERE u.username = 'admin'`).Scan(&userID, &orgID); err != nil {
		t.Fatal(err)
	}
	return authz.Principal{ID: userID, OrgID: orgID, Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: orgID}}}
}
