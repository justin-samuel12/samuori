package workers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/pki"
	"samuori.invalid/samuori/internal/version"
)

func TestWorkerAssignment(t *testing.T) {
	t.Run("SEC-WRK-10", func(t *testing.T) {
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
		srv := NewServer(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		admin := authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
		other := authz.Principal{ID: "admin-2", OrgID: "o1", Kind: authz.User, Bindings: admin.Bindings}
		operator := authz.Principal{ID: "op", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Operator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}

		p1, err := store.CreatePool(ctx, admin, Pool{Name: "one", IsolationProfile: "rootless-dedicated-vm", TrustDomain: "default"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if p1, _ = store.Pool(ctx, "o1", p1.ID); !p1.ProfileApproved || p1.ApprovedBy != "admin" || p1.Location != DefaultLocation {
			t.Fatalf("a new pool = %+v; want approved by its creator in the default location", p1)
		}

		if _, err := store.CreateToken(ctx, operator, time.Hour, ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("an operator issued an enrollment token: %v", err)
		}
		tok, err := store.CreateToken(ctx, admin, time.Hour, "")
		if err != nil || tok.PoolID != "" {
			t.Fatalf("token = %+v, %v; want one without a pool", tok, err)
		}
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
		caps := Capabilities{OS: "linux", Architectures: []string{"amd64"}, IsolationProfiles: []string{"rootless-dedicated-vm"}, DaemonSlots: 1, StrictEgress: true, ConfigDigest: "c1"}
		enrolled, _, err := store.Enroll(ctx, EnrollRequest{Token: tok.Value, Name: "build-1", Version: version.Version, CSR: csr, Capabilities: caps, ProtocolMinor: 2})
		if err != nil || enrolled.PoolID != "" {
			t.Fatalf("enroll = %+v, %v; want an unassigned worker", enrolled, err)
		}

		w, _ := store.Get(ctx, "o1", enrolled.ID)
		now := time.Now()
		for _, req := range []isolation.Requirement{{}, {Untrusted: true}, {Secrets: true}} {
			if ok, why := isolation.Eligible(Pool{}.State(), w.State(now), req, now); ok || !slices.Contains(why, "unassigned") {
				t.Errorf("an unassigned worker is eligible for %+v: %v", req, why)
			}
		}
		if NeedsSelfTest(w, p1, now) {
			t.Error("an unassigned worker would be self-tested")
		}
		if _, err := srv.RequestWorkerSelfTest(ctx, admin, w.ID, ""); !errors.Is(err, ErrUnassigned) {
			t.Errorf("an unassigned worker's self-test was requested: %v", err)
		}
		if _, err := store.StartSelfTest(ctx, authz.Principal{OrgID: "o1", Kind: authz.System}, w, p1, ""); !errors.Is(err, ErrUnassigned) {
			t.Errorf("a self-test started for a pool the worker is not in: %v", err)
		}

		if _, err := store.AssignWorker(ctx, operator, w.ID, p1.ID, "", ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("an operator assigned a worker: %v", err)
		}
		if w, err = store.AssignWorker(ctx, admin, w.ID, p1.ID, "first pool", ""); err != nil || w.PoolID != p1.ID {
			t.Fatalf("assign = %+v, %v", w, err)
		}
		if _, err := store.AssignWorker(ctx, admin, w.ID, p1.ID, "", ""); !errors.Is(err, ErrConflict) {
			t.Errorf("assigning a worker to its own pool = %v; want a conflict", err)
		}
		if !NeedsSelfTest(w, p1, now) {
			t.Error("an assigned worker does not need its self-test")
		}
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO pool_self_tests (id, org_id, pool_id, worker_id, status, failed_checks, started_at, finished_at, profile, config_digest) VALUES ('t1', 'o1', ?, ?, 'passed', '[]', ?, ?, 'rootless-dedicated-vm', 'c1')`),
			p1.ID, w.ID, now.UTC(), now.UTC()); err != nil {
			t.Fatal(err)
		}
		if w, _ = store.Get(ctx, "o1", w.ID); !Verified(w, p1, now) {
			t.Fatalf("the worker is not verified after passing its self-test: %+v", w.SelfTest)
		}

		p2, err := store.CreatePool(ctx, admin, Pool{Name: "two", IsolationProfile: "rootless-dedicated-vm", TrustDomain: "default"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if w, err = store.AssignWorker(ctx, admin, w.ID, p2.ID, "move", ""); err != nil {
			t.Fatal(err)
		}
		if Verified(w, p2, now) || w.SelfTest.Status != "never-run" {
			t.Errorf("trust carried over to a new pool with the same profile: %+v", w.SelfTest)
		}
		var n int
		_ = d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action = 'worker.assign'`).Scan(&n)
		if n != 2 {
			t.Errorf("assignment audited %d times, want 2", n)
		}

		profile := "rootless-host"
		if p2, err = store.UpdatePool(ctx, other, p2.ID, PoolUpdate{IsolationProfile: &profile}, ""); err != nil {
			t.Fatal(err)
		}
		if !p2.ProfileApproved || p2.ApprovedBy != "admin-2" {
			t.Errorf("an edited pool = approved %v by %q; want approved by the editing administrator", p2.ProfileApproved, p2.ApprovedBy)
		}
	})
}
