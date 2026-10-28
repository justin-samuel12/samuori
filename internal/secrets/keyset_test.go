package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeKV stands in for OpenBao or Vault: one KV v2 secret with three keys. mounts records the mount of each read.
type fakeKV struct {
	keys   map[string]string
	mounts []string
}

func (f *fakeKV) Read(_ context.Context, _ string, cfg ExternalConfig, _ []byte, ref, _ string) ([]byte, error) {
	f.mounts = append(f.mounts, cfg.Mount)
	path, key, hasKey := strings.Cut(ref, "#")
	if path != "operations/mssql-base" {
		return nil, errors.New("not found")
	}
	if !hasKey {
		return json.Marshal(f.keys)
	}
	v, ok := f.keys[key]
	if !ok {
		return nil, errors.New("no such key")
	}
	return []byte(v), nil
}

func (f *fakeKV) Check(context.Context, string, ExternalConfig, []byte) (string, error) {
	return "test", nil
}

func TestKeySetGrants(t *testing.T) {
	t.Run("SEC-SCR-10", func(t *testing.T) {
		ctx := context.Background()
		f := newFixture(t)
		s, admin, dev := f.s, f.admin, f.dev
		kv := &fakeKV{keys: map[string]string{"sa_password": "sa-value", "tde_key": "tde-value", "old_key": "old-value"}}
		s.UseExternal(ProviderVault, kv)
		vault, err := s.CreateProvider(ctx, admin, "vault", ProviderVault, ExternalConfig{Address: "https://vault.example:8200", Mount: "docker", AuthMethod: "token"}, []byte("token"), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AllowSecret(ctx, admin, vault.ID, "operations/mssql-base", "", "", ""); err != nil {
			t.Fatal(err)
		}
		g, err := s.CreateGrant(ctx, admin, GrantRequest{ProjectID: "p1", Name: "mssql-base", ProviderID: vault.ID, ProviderReference: "operations/mssql-base", Usage: "build", Reason: "SQL Server base image"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(g.Fields, ",") != "old_key,sa_password,tde_key" {
			t.Fatalf("key names = %v", g.Fields)
		}

		if _, err := s.Bind(ctx, admin, BindRequest{ProjectID: "p1", LogicalID: "everything", ProviderID: vault.ID, ProviderReference: "operations/mssql-base", Reason: "whole"}, ""); err == nil {
			t.Error("a direct binding received a whole key set")
		}
		if _, err := s.BindGrant(ctx, dev, "p1", "sa_password", g.ID, "", "", ""); err == nil {
			t.Error("a key set was bound without choosing a key")
		}
		if _, err := s.BindGrant(ctx, dev, "p1", "sa_password", g.ID, "missing", "", ""); err == nil {
			t.Error("a key outside the set was bound")
		}
		if _, err := s.BindGrant(ctx, f.outsider, "p1", "sa_password", g.ID, "sa_password", "", ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("a developer of another team bound the set: %v", err)
		}
		sa, err := s.BindGrant(ctx, dev, "p1", "sa_password", g.ID, "sa_password", "", "")
		if err != nil {
			t.Fatal(err)
		}
		old, err := s.BindGrant(ctx, dev, "p1", "legacy", g.ID, "old_key", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if v, err := s.Value(ctx, "o1", sa.ID, sa.Version, "build"); err != nil || string(v) != "sa-value" {
			t.Fatalf("released %q, %v; want only the chosen key's value", v, err)
		}
		_, uses, err := s.ListGrants(ctx, dev, "p1")
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range uses {
			if u.LogicalID == "sa_password" && u.Field != "sa_password" {
				t.Errorf("use = %+v", u)
			}
		}

		delete(kv.keys, "old_key")
		if _, err := s.RefreshGrantKeys(ctx, dev, "p1", g.ID, ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("a developer refreshed a key set: %v", err)
		}
		g, err = s.RefreshGrantKeys(ctx, admin, "p1", g.ID, "")
		if err != nil || strings.Join(g.Fields, ",") != "sa_password,tde_key" {
			t.Fatalf("after refresh: %v, %v", g.Fields, err)
		}
		if _, err := s.Value(ctx, "o1", old.ID, old.Version, "build"); err == nil {
			t.Error("a binding to a removed key was still released")
		}
		if _, err := s.Value(ctx, "o1", sa.ID, sa.Version, "build"); err != nil {
			t.Errorf("a binding to a kept key stopped: %v", err)
		}
	})
}

func TestBase64Bindings(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	s, admin, dev := f.s, f.admin, f.dev
	cert := []byte{0x30, 0x82, 0x01, 0x0a, 0x00, 0xff, 0x10, 0x7f}
	wrapped := base64.StdEncoding.EncodeToString(cert)
	wrapped = wrapped[:4] + "\n" + wrapped[4:] + "\n"
	kv := &fakeKV{keys: map[string]string{
		"tde_cert":    wrapped,
		"tde_key":     base64.RawStdEncoding.EncodeToString([]byte("key-bytes")),
		"sa_password": "not base64!",
	}}
	s.UseExternal(ProviderVault, kv)
	vault, err := s.CreateProvider(ctx, admin, "vault", ProviderVault, ExternalConfig{Address: "https://vault.example:8200", Mount: "docker", AuthMethod: "token"}, []byte("token"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AllowSecret(ctx, admin, vault.ID, "operations/mssql-base", "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGrant(ctx, admin, GrantRequest{ProjectID: "p1", Name: "mssql-base", ProviderID: vault.ID, ProviderReference: "operations/mssql-base", Usage: "build", Reason: "SQL Server base image"}, "")
	if err != nil {
		t.Fatal(err)
	}

	b, err := s.BindGrant(ctx, dev, "p1", "tde_cert", g.ID, "tde_cert", EncodingBase64, "")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.Value(ctx, "o1", b.ID, b.Version, "build"); err != nil || !bytes.Equal(v, cert) {
		t.Fatalf("released %x, %v; want the decoded bytes", v, err)
	}
	k, err := s.Bind(ctx, admin, BindRequest{ProjectID: "p1", LogicalID: "tde_key", ProviderID: vault.ID, ProviderReference: "operations/mssql-base#tde_key", Reason: "unpadded", Encoding: EncodingBase64}, "")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.Value(ctx, "o1", k.ID, k.Version, "build"); err != nil || string(v) != "key-bytes" {
		t.Fatalf("released %q, %v", v, err)
	}
	plain, err := s.BindGrant(ctx, dev, "p1", "sa_password", g.ID, "sa_password", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.Value(ctx, "o1", plain.ID, plain.Version, "build"); err != nil || string(v) != "not base64!" {
		t.Fatalf("an unencoded binding changed its value: %q, %v", v, err)
	}

	var inv *Invalid
	if _, err := s.BindGrant(ctx, dev, "p1", "sa_password", g.ID, "sa_password", EncodingBase64, ""); !errors.As(err, &inv) {
		t.Errorf("a value that is not base64 was bound for decoding: %v", err)
	}
	if _, err := s.BindGrant(ctx, dev, "p1", "tde_cert", g.ID, "tde_cert", "hex", ""); !errors.As(err, &inv) {
		t.Errorf("an unknown encoding was accepted: %v", err)
	}
	local, err := s.LocalProvider(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := s.Store(ctx, admin, local.ID, "stored", "text", []byte("dmFsdWU="), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Bind(ctx, admin, BindRequest{ProjectID: "p1", LogicalID: "stored", ProviderID: local.ID, ProviderReference: sec.ID, Reason: "local", Encoding: EncodingBase64}, ""); !errors.As(err, &inv) {
		t.Errorf("base64 decoding was accepted for the local store: %v", err)
	}

	_, uses, err := s.ListGrants(ctx, dev, "p1")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range uses {
		if want := map[string]string{"tde_cert": EncodingBase64, "tde_key": EncodingBase64}[u.LogicalID]; u.Encoding != want {
			t.Errorf("use %s encoding = %q, want %q", u.LogicalID, u.Encoding, want)
		}
	}
	kv.keys["tde_cert"] = "corrupted!"
	if _, err := s.Value(ctx, "o1", b.ID, b.Version, "build"); err == nil {
		t.Error("a value that no longer decodes was released")
	}
}
