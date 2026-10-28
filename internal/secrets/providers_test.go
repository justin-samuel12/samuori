package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestUpdateAndDeleteProvider(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	s, admin, dev := f.s, f.admin, f.dev
	s.UseExternal(ProviderVault, &fakeKV{keys: map[string]string{"sa_password": "v"}})
	cfg := ExternalConfig{Address: "https://vault.example:8200", Mount: "docker", AuthMethod: "token"}
	p, err := s.CreateProvider(ctx, admin, "vault", ProviderVault, cfg, []byte("token-1"), "")
	if err != nil {
		t.Fatal(err)
	}
	credential := func(id string) string {
		t.Helper()
		got, err := s.provider(ctx, "o1", id)
		if err != nil {
			t.Fatal(err)
		}
		if got.CredentialSecretID == "" {
			return ""
		}
		v, err := s.ControllerValue(ctx, "o1", got.CredentialSecretID)
		if err != nil {
			t.Fatal(err)
		}
		return string(v)
	}

	if _, err := s.UpdateProvider(ctx, dev, p.ID, "vault", cfg, nil, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("a developer edited a store: %v", err)
	}
	cfg.Mount, cfg.Namespace = "kv", "ops"
	got, err := s.UpdateProvider(ctx, admin, p.ID, "vault-ops", cfg, nil, "")
	if err != nil || got.Name != "vault-ops" || got.Config.Mount != "kv" || credential(p.ID) != "token-1" {
		t.Fatalf("rename and mount change = %+v, %v; credential %q", got, err, credential(p.ID))
	}

	var inv *Invalid
	moved := cfg
	moved.Address = "https://elsewhere.example:8200"
	if _, err := s.UpdateProvider(ctx, admin, p.ID, "vault-ops", moved, nil, ""); !errors.As(err, &inv) {
		t.Errorf("the stored token would have gone to a new address: %v", err)
	}
	approle := cfg
	approle.AuthMethod, approle.RoleID = "approle", "role-1"
	if _, err := s.UpdateProvider(ctx, admin, p.ID, "vault-ops", approle, nil, ""); !errors.As(err, &inv) {
		t.Errorf("a token was kept as an AppRole secret ID: %v", err)
	}
	if _, err := s.UpdateProvider(ctx, admin, p.ID, "vault-ops", approle, []byte("secret-id"), ""); err != nil || credential(p.ID) != "secret-id" {
		t.Fatalf("switch to AppRole = %v; credential %q", err, credential(p.ID))
	}
	if _, err := s.UpdateProvider(ctx, admin, p.ID, "vault-ops", moved, []byte("token-2"), ""); err != nil || credential(p.ID) != "token-2" {
		t.Fatalf("move with a new token = %v; credential %q", err, credential(p.ID))
	}

	other, err := s.CreateProvider(ctx, admin, "spare", ProviderVault, cfg, []byte("t"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProvider(ctx, admin, p.ID, "spare", moved, nil, ""); !errors.As(err, &inv) {
		t.Errorf("two stores share a name: %v", err)
	}
	spareCred := mustProvider(t, s, other.ID).CredentialSecretID
	if err := s.DeleteProvider(ctx, dev, other.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("a developer deleted a store: %v", err)
	}
	if err := s.DeleteProvider(ctx, admin, other.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.secret(ctx, "o1", spareCred); !errors.Is(err, ErrNotFound) {
		t.Errorf("the deleted store's credential remains: %v", err)
	}

	if _, err := s.AllowSecret(ctx, admin, p.ID, "operations/mssql-base", "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGrant(ctx, admin, GrantRequest{ProjectID: "p1", Name: "sa", ProviderID: p.ID, ProviderReference: "operations/mssql-base#sa_password", Usage: "build", Reason: "SQL Server base image"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var inUse *InUse
	if err := s.DeleteProvider(ctx, admin, p.ID, ""); !errors.As(err, &inUse) || !inUse.Store {
		t.Errorf("a store with an active grant was deleted: %v", err)
	}
	if err := s.RevokeGrant(ctx, admin, "p1", g.ID, "done", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProvider(ctx, admin, p.ID, ""); !errors.As(err, &inUse) {
		t.Errorf("a store a revoked grant names was deleted: %v", err)
	}
	local, err := s.LocalProvider(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProvider(ctx, admin, local.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("the local store was deleted: %v", err)
	}
}

func mustProvider(t *testing.T, s *Service, id string) Provider {
	t.Helper()
	p, err := s.provider(context.Background(), "o1", id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
