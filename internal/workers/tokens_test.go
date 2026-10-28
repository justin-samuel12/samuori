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
)

// TestEnrollmentTokenRevocation: only administrators revoke, a revoked token never enrolls a worker, a used
// token cannot be revoked, and revocation is audited.
func TestEnrollmentTokenRevocation(t *testing.T) {
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
	operator := authz.Principal{ID: "op", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Operator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	enroll := func(tok Token) error {
		_, _, err := store.Enroll(ctx, EnrollRequest{Token: tok.Value, Name: "w-" + tok.ID[20:], Version: version.Version, CSR: csr, ProtocolMinor: 2})
		return err
	}

	revoked, err := store.CreateToken(ctx, admin, time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeToken(ctx, operator, revoked.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an operator revoked a token: %v", err)
	}
	if _, err := store.ListTokens(ctx, operator, 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an operator listed tokens: %v", err)
	}
	if err := store.RevokeToken(ctx, admin, revoked.ID, "req-1"); err != nil {
		t.Fatal(err)
	}
	if err := enroll(revoked); !errors.Is(err, ErrToken) {
		t.Fatalf("a revoked token enrolled a worker: %v", err)
	}
	if err := store.RevokeToken(ctx, admin, revoked.ID, ""); !errors.Is(err, ErrTokenSpent) {
		t.Fatalf("revoking twice = %v", err)
	}

	used, err := store.CreateToken(ctx, admin, time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := enroll(used); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeToken(ctx, admin, used.ID, ""); !errors.Is(err, ErrTokenSpent) {
		t.Fatalf("revoking a used token = %v", err)
	}
	if err := store.RevokeToken(ctx, admin, "01J00000000000000000000000", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking a missing token = %v", err)
	}

	list, err := store.ListTokens(ctx, admin, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	for _, tk := range list {
		switch tk.ID {
		case revoked.ID:
			if tk.RevokedAt == nil || tk.RevokedBy != "admin" || tk.UsedAt != nil {
				t.Errorf("revoked token listed as %+v", tk)
			}
		case used.ID:
			if tk.UsedAt == nil || tk.WorkerName == "" || tk.RevokedAt != nil {
				t.Errorf("used token listed as %+v", tk)
			}
		}
	}
	var audited int
	_ = d.SQL.QueryRowContext(ctx, d.Rebind(`SELECT COUNT(*) FROM audit_events WHERE action = 'enrollment_token.revoke' AND resource_id = ? AND request_id = 'req-1'`), revoked.ID).Scan(&audited)
	if audited != 1 {
		t.Errorf("revocation audit events = %d", audited)
	}
}
