package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestAllowedSecrets(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	s, admin, dev := f.s, f.admin, f.dev
	kv := &fakeKV{keys: map[string]string{"sa_password": "v", "tde_key": "k"}}
	s.UseExternal(ProviderVault, kv)
	vault, err := s.CreateProvider(ctx, admin, "vault", ProviderVault, ExternalConfig{Address: "https://vault.example:8200", Mount: "docker", AuthMethod: "approle", RoleID: "samuori"}, nil, "")
	if err != nil {
		t.Fatalf("an AppRole store without a secret ID was refused: %v", err)
	}
	if _, err := s.CreateProvider(ctx, admin, "bare", ProviderVault, ExternalConfig{Address: "https://vault.example:8200", AuthMethod: "token"}, nil, ""); err == nil {
		t.Error("a token store without a token was accepted")
	}
	grant := GrantRequest{ProjectID: "p1", ProviderID: vault.ID, ProviderReference: "operations/mssql-base#sa_password", Usage: "build", Reason: "SQL Server base image"}
	var inv *Invalid
	if _, err := s.CreateGrant(ctx, admin, grant, ""); !errors.As(err, &inv) {
		t.Fatalf("a secret that is not allowed was granted: %v", err)
	}

	if _, err := s.AllowSecret(ctx, dev, vault.ID, "operations/mssql-base", "", "", ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("a developer allowed a secret: %v", err)
	}
	if _, err := s.AllowSecret(ctx, admin, vault.ID, "operations/missing", "", "", ""); !errors.As(err, &inv) {
		t.Errorf("a secret that does not exist was allowed: %v", err)
	}
	local, err := s.LocalProvider(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AllowSecret(ctx, admin, local.ID, "anything", "", "", ""); !errors.As(err, &inv) {
		t.Errorf("the local store took an allowed entry: %v", err)
	}
	a, err := s.AllowSecret(ctx, admin, vault.ID, "operations/mssql-base", "SQL Server base", "TDE and logins", "")
	if err != nil || a.Label != "SQL Server base" || len(a.Fields) != 2 {
		t.Fatalf("allow = %+v, %v", a, err)
	}
	if _, err := s.AllowSecret(ctx, admin, vault.ID, "operations/mssql-base", "", "", ""); !errors.As(err, &inv) {
		t.Errorf("the same secret was allowed twice: %v", err)
	}
	if list, err := s.AllowedSecrets(ctx, admin); err != nil || len(list) != 1 {
		t.Errorf("allowed list = %+v, %v", list, err)
	}
	if _, err := s.AllowedSecrets(ctx, dev); !errors.Is(err, ErrForbidden) {
		t.Errorf("a developer listed allowed secrets: %v", err)
	}

	if _, err := s.CreateGrant(ctx, admin, grant, ""); err != nil {
		t.Errorf("one key of an allowed secret was refused: %v", err)
	}
	g, err := s.CreateGrant(ctx, admin, GrantRequest{ProjectID: "p2", AllowedSecretID: a.ID, Usage: "build", Reason: "from the list"}, "")
	if err != nil || g.Name != "SQL Server base" || g.ProviderReference != "operations/mssql-base" || len(g.Fields) != 2 {
		t.Fatalf("grant from the list = %+v, %v", g, err)
	}
	grant.ProviderReference = "operations/mssql-base#other"
	if _, err := s.CreateGrant(ctx, admin, grant, ""); err == nil {
		t.Error("a key outside the allowed secret was granted")
	}

	if err := s.RemoveAllowedSecret(ctx, dev, a.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("a developer removed an allowed secret: %v", err)
	}
	if err := s.RemoveAllowedSecret(ctx, admin, a.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGrant(ctx, admin, GrantRequest{ProjectID: "p1", AllowedSecretID: a.ID, Name: "again", Usage: "build", Reason: "after removal"}, ""); !errors.As(err, &inv) {
		t.Errorf("a removed entry was granted: %v", err)
	}
	if got, err := s.grant(ctx, "o1", "p2", g.ID); err != nil || got.Status != Active {
		t.Errorf("removing the entry changed an existing grant: %+v, %v", got, err)
	}
	if _, err := s.AllowSecret(ctx, admin, vault.ID, "operations/mssql-base", "", "", ""); err != nil {
		t.Errorf("a removed secret could not be allowed again: %v", err)
	}

	m, err := s.AllowSecret(ctx, admin, vault.ID, "kv-ops:operations/mssql-base", "", "", "")
	if err != nil || len(m.Fields) != 2 || kv.mounts[len(kv.mounts)-1] != "kv-ops" {
		t.Fatalf("a secret on another mount = %+v, %v; read from %v", m, err, kv.mounts)
	}
	g2, err := s.CreateGrant(ctx, admin, GrantRequest{ProjectID: "p1", Name: "other mount", ProviderID: vault.ID, ProviderReference: "kv-ops:operations/mssql-base#tde_key", Usage: "build", Reason: "second mount"}, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.BindGrant(ctx, dev, "p1", "tde_key", g2.ID, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.Value(ctx, "o1", b.ID, b.Version, "build"); err != nil || string(v) != "k" || kv.mounts[len(kv.mounts)-1] != "kv-ops" {
		t.Fatalf("released %q, %v from mount %v", v, err, kv.mounts)
	}
	if _, err := s.AllowSecret(ctx, admin, vault.ID, "../sys:operations/mssql-base", "", "", ""); !errors.As(err, &inv) {
		t.Errorf("a mount with a dot segment was accepted: %v", err)
	}
}
