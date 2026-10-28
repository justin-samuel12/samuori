package clusterjoin

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/config"
	"samuori.invalid/samuori/internal/db"
)

func store(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return NewStore(d)
}

func principal(role authz.Role) authz.Principal {
	return authz.Principal{ID: "u1", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: role, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
}

func TestJoinTokens(t *testing.T) {
	t.Run("SEC-HA-03", func(t *testing.T) {
		ctx := context.Background()
		s := store(t)
		admin := principal(authz.Administrator)
		if _, _, err := s.Create(ctx, principal(authz.Operator), time.Hour, ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("an operator issued a join token: %v", err)
		}
		tok, value, err := s.Create(ctx, admin, 10*time.Hour, "")
		if err != nil {
			t.Fatal(err)
		}
		if tok.ExpiresAt.Sub(tok.CreatedAt) > MaxTTL {
			t.Fatalf("token lifetime %s exceeds %s", tok.ExpiresAt.Sub(tok.CreatedAt), MaxTTL)
		}
		var stored string
		_ = s.db.SQL.QueryRow(`SELECT token_hash FROM controller_join_tokens WHERE id = ?`, tok.ID).Scan(&stored)
		if stored == "" || strings.Contains(stored, value) {
			t.Fatal("the token value is stored instead of its hash")
		}
		if _, err := s.Redeem(ctx, value, "ctrl-2.example; rm -rf /", "192.0.2.1", testSecretHash, ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a malformed host was accepted: %v", err)
		}
		if _, err := s.Redeem(ctx, value, "ctrl-2.example", "192.0.2.1", testSecretHash, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Redeem(ctx, value, "ctrl-3.example", "192.0.2.1", testSecretHash, ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a token was redeemed twice: %v", err)
		}

		revoked, rv, _ := s.Create(ctx, admin, time.Hour, "")
		if err := s.Revoke(ctx, admin, revoked.ID, ""); err != nil {
			t.Fatal(err)
		}
		_, ev, _ := s.Create(ctx, admin, time.Hour, "")
		s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		for name, v := range map[string]string{"revoked": rv, "expired": ev, "unknown": "samuorij_" + strings.Repeat("A", 43), "foreign": "samuoriw_x"} {
			if _, err := s.Redeem(ctx, v, "ctrl-4.example", "192.0.2.1", testSecretHash, ""); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s token: %v", name, err)
			}
		}
		list, err := s.List(ctx, admin)
		if err != nil || len(list) != 3 || list[len(list)-1].UsedHost != "ctrl-2.example" {
			t.Fatalf("list = %+v, %v", list, err)
		}
	})
}

func TestBundleCertificatesAndFiles(t *testing.T) {
	t.Run("SEC-HA-03", func(t *testing.T) {
		ca, err := Create(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		b := Bundle{ClusterKey: make([]byte, 32), DatabaseDSN: "postgres://samuori:pw@db.example/samuori", NATSURLs: []string{NATSURL("ctrl-1.example")}, NATSReplicas: 3,
			Storage: map[string]string{"s3.bucket": "blobs", "s3.authMethod": "workload-identity"}, Files: map[string][]byte{"s3-credentials": []byte("k:s")}}
		b, err = b.ForHost(ca, "ctrl-2.example")
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca.CertPEM())
		usage := func(p Pair, want x509.ExtKeyUsage, host string) {
			blk, _ := pem.Decode(p.Cert)
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Verify(x509.VerifyOptions{Roots: pool, DNSName: host, KeyUsages: []x509.ExtKeyUsage{want}}); err != nil {
				t.Errorf("%s for %s: %v", c.Subject.CommonName, host, err)
			}
		}
		usage(b.NATSServer, x509.ExtKeyUsageServerAuth, "ctrl-2.example")
		usage(b.NATSServer, x509.ExtKeyUsageClientAuth, "ctrl-2.example")
		usage(b.NATSClient, x509.ExtKeyUsageClientAuth, "")
		usage(b.API, x509.ExtKeyUsageServerAuth, "ctrl-2.example")
		blk, _ := pem.Decode(b.API.Cert)
		api, _ := x509.ParseCertificate(blk.Bytes)
		if _, err := api.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
			t.Error("the API certificate also works as a NATS client certificate")
		}
		if b.NATSURLs[0] != NATSURL("ctrl-2.example") || len(b.NATSURLs) != 2 {
			t.Fatalf("NATS URLs = %v", b.NATSURLs)
		}

		me, _ := user.Current()
		dir := filepath.Join(t.TempDir(), "etc-samuori")
		old := syscall.Umask(0o077)
		err = b.Install(Layout{ConfigDir: dir, DataDir: "/var/lib/samuori", Owner: me.Username, NATSOwner: me.Username}, "")
		syscall.Umask(old)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{dir, filepath.Join(dir, "nats")} {
			if st, err := os.Stat(d); err != nil || st.Mode().Perm() != 0o755 {
				t.Errorf("%s under umask 077: mode %v, %v; the nats user could not read its configuration", d, st.Mode(), err)
			}
		}
		for _, f := range []string{"cluster.key", "env", "api.key", "nats-client.key", "nats/server.key", "s3-credentials"} {
			st, err := os.Stat(filepath.Join(dir, f))
			if err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("%s: mode %v, %v", f, st.Mode(), err)
			}
		}
		conf, _ := os.ReadFile(filepath.Join(dir, "server.yml"))
		for _, want := range []string{"role: controller", "enabled: \"true\"", "bucket: \"blobs\"", "credentialsFile: \"" + filepath.Join(dir, "s3-credentials") + "\""} {
			if !strings.Contains(string(conf), want) {
				t.Errorf("server.yml lacks %q:\n%s", want, conf)
			}
		}
		if strings.Contains(string(conf), "pw@") || strings.Contains(string(conf), "bootstrap") {
			t.Errorf("server.yml holds the database password or a bootstrap administrator:\n%s", conf)
		}
		nats, _ := os.ReadFile(filepath.Join(dir, "nats", "nats.conf"))
		if !strings.Contains(string(nats), "nats-route://ctrl-1.example:6222") || !strings.Contains(string(nats), "nats-route://ctrl-2.example:6222") {
			t.Errorf("nats.conf routes:\n%s", nats)
		}
		bad := b
		bad.Files = map[string][]byte{"../../etc/passwd": []byte("x")}
		if err := bad.Install(Layout{ConfigDir: t.TempDir(), Owner: me.Username, NATSOwner: me.Username}, ""); err == nil {
			t.Error("a bundle file outside the known names was written")
		}
	})
}

func TestExternalNATSJoin(t *testing.T) {
	t.Run("SEC-HA-03", func(t *testing.T) {
		ca, err := Create(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		me, _ := user.Current()
		first := Bundle{ClusterKey: make([]byte, 32), DatabaseDSN: "postgres://samuori:pw@db.example/samuori?sslmode=verify-full", ExternalNATS: true,
			NATSURLs: []string{"tls://nats-1.example:4222", "tls://nats-2.example:4222"}, NATSReplicas: 3, Storage: map[string]string{"s3.bucket": "blobs", "s3.region": "us-east-1"},
			Files: map[string][]byte{"messaging-ca": []byte("external CA"), "messaging-credentials": []byte("creds")}}
		first, err = first.ForHost(ca, "ctrl-1.example")
		if err != nil {
			t.Fatal(err)
		}
		if len(first.NATSURLs) != 2 || first.NATSServer.Cert != nil {
			t.Fatalf("an external NATS bundle has a local member: %v", first.NATSURLs)
		}
		dir := t.TempDir()
		if err := first.Install(Layout{ConfigDir: dir, DataDir: t.TempDir(), Owner: me.Username, NATSOwner: me.Username}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "nats", "nats.conf")); err == nil {
			t.Error("nats.conf was written for an external NATS cluster")
		}
		cfg, _, err := config.Load([]string{"--config", filepath.Join(dir, "server.yml")}, []string{"SAMUORI_DATABASE=" + first.DatabaseDSN}, config.OSReadFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Messaging.CAFile != filepath.Join(dir, "messaging-ca") || cfg.Messaging.CredentialsFile != filepath.Join(dir, "messaging-credentials") {
			t.Fatalf("messaging = %+v", cfg.Messaging)
		}

		joined, err := FromConfig(cfg, ca)
		if err != nil {
			t.Fatal(err)
		}
		if joined, err = joined.ForHost(ca, "ctrl-2.example"); err != nil {
			t.Fatal(err)
		}
		if !joined.ExternalNATS || len(joined.NATSURLs) != 2 || string(joined.Files["messaging-credentials"]) != "creds" {
			t.Fatalf("join bundle: external=%v urls=%v files=%v", joined.ExternalNATS, joined.NATSURLs, len(joined.Files))
		}
	})
}

// A joining host gets the database CA from the bundle, so the DSN's sslrootcert never names a file it lacks.
func TestBundledDatabaseCA(t *testing.T) {
	dir := t.TempDir()
	b := Bundle{DatabaseDSN: "postgres://samuori:p%40ss@db.example:5432/samuori?sslmode=verify-full&sslrootcert=/etc/samuori/rds-ca.pem",
		Files: map[string][]byte{"database-ca": []byte("-----BEGIN CERTIFICATE-----\n")}}
	if err := b.UseBundledDatabaseCA(dir); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "database-ca")
	if got := sslRootCert(b.DatabaseDSN); got != want {
		t.Fatalf("sslrootcert = %q, want %q", got, want)
	}
	if !strings.Contains(b.DatabaseDSN, "samuori:p%40ss@db.example:5432/samuori") || !strings.Contains(b.DatabaseDSN, "sslmode=verify-full") {
		t.Errorf("the DSN lost its credentials or TLS mode: %s", b.DatabaseDSN)
	}
	if data, err := os.ReadFile(want); err != nil || string(data) != "-----BEGIN CERTIFICATE-----\n" {
		t.Errorf("CA file: %q, %v", data, err)
	}
	plain := Bundle{DatabaseDSN: "postgres://samuori:pw@db.example/samuori?sslmode=require"}
	if err := plain.UseBundledDatabaseCA(dir); err != nil || plain.DatabaseDSN != "postgres://samuori:pw@db.example/samuori?sslmode=require" {
		t.Errorf("a DSN without sslrootcert changed: %s, %v", plain.DatabaseDSN, err)
	}
}
