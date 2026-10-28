package workers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/pki"
	"samuori.invalid/samuori/internal/version"
	"samuori.invalid/samuori/internal/workerjoin"
)

// TestJoinCodeToken checks that the token a worker join code carries is as single-use and short-lived as the
// separately shown one: the code adds no way to enrol twice or late (SEC-WRK-11).
func TestJoinCodeToken(t *testing.T) {
	t.Run("SEC-WRK-11", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		d, err := db.Open(ctx, "sqlite://"+filepath.Join(dir, "samuori.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if _, err := d.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		ca, err := pki.Open(filepath.Join(dir, "pki"))
		if err != nil {
			t.Fatal(err)
		}
		store := NewStore(d, ca)
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		admin := authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
		viaCode := func() workerjoin.Code {
			tok, err := store.CreateToken(ctx, admin, time.Hour, "")
			if err != nil {
				t.Fatal(err)
			}
			s, err := workerjoin.Encode(workerjoin.Code{Controllers: []string{"samuori.example:8444"}, Pin: tok.CAPin, Token: tok.Value})
			if err != nil {
				t.Fatal(err)
			}
			c, err := workerjoin.Decode(s)
			if err != nil || c.Pin != ca.Issuing().Pin() {
				t.Fatalf("decode = %+v, %v", c, err)
			}
			return c
		}
		enroll := func(token, name string) error {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
			_, _, err := store.Enroll(ctx, EnrollRequest{Token: token, Name: name, Version: version.Version, CSR: csr, Capabilities: Capabilities{OS: "linux"}, ProtocolMinor: 2})
			return err
		}

		used := viaCode()
		if err := enroll(used.Token, "w1"); err != nil {
			t.Fatalf("first use: %v", err)
		}
		if err := enroll(used.Token, "w2"); !errors.Is(err, ErrToken) {
			t.Fatalf("a used code enrolled again: %v", err)
		}
		late := viaCode()
		store.now = func() time.Time { return time.Now().Add(61 * time.Minute) }
		if err := enroll(late.Token, "w3"); !errors.Is(err, ErrToken) {
			t.Fatalf("an expired code enrolled: %v", err)
		}
	})
}
