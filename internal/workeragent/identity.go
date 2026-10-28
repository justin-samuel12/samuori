package workeragent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"samuori.invalid/samuori/internal/pki"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

// ErrIdentityExpired stops the agent: an expired certificate cannot be renewed, so the worker must be re-enrolled
// with a new token (ADR 0026). Re-enrolling revokes the old identity.
var ErrIdentityExpired = errors.New("the worker certificate has expired; re-enroll this worker with a new enrollment token")

// Expired reports an enrolled worker whose certificate has expired by its own clock.
func (a *Agent) Expired() bool {
	c, err := a.certificate()
	return err == nil && a.clock().After(c.NotAfter)
}

// ErrIdentityRevoked stops the agent: the controller refuses this identity, so retrying cannot help.
var ErrIdentityRevoked = errors.New("the controller revoked this worker identity; enroll the worker again with a new token")

func revoked(err error) bool {
	if status.Code(err) != codes.PermissionDenied {
		return false
	}
	msg := status.Convert(err).Message()
	return strings.Contains(msg, "revoked") || strings.Contains(msg, "not enrolled with this certificate")
}

// maxSkew is how far the worker's clock may differ from the controller's before it warns.
const maxSkew = 2 * time.Minute

func (a *Agent) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *Agent) certificate() (*x509.Certificate, error) {
	raw, err := os.ReadFile(a.path("worker.crt"))
	if err != nil {
		return nil, err
	}
	b, _ := pem.Decode(raw)
	if b == nil {
		return nil, errors.New("worker.crt is not PEM")
	}
	return x509.ParseCertificate(b.Bytes)
}

// checkIdentity refuses to connect with a certificate that is expired or not yet valid by this worker's clock.
func (a *Agent) checkIdentity() error {
	c, err := a.certificate()
	if err != nil {
		return err
	}
	now := a.clock()
	switch {
	case now.After(c.NotAfter):
		return fmt.Errorf("%w (expired %s, local time %s)", ErrIdentityExpired, c.NotAfter.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	case now.Before(c.NotBefore):
		return fmt.Errorf("clock skew: the worker certificate is valid from %s but the local clock reads %s; fix time synchronization (NTP)", c.NotBefore.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	return nil
}

// diagnose turns certificate-time failures into an actionable message (SEC-ID-06).
func (a *Agent) diagnose(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "certificate has expired or is not yet valid") || strings.Contains(msg, "not yet valid") {
		return fmt.Errorf("clock skew or expired certificate: the local clock reads %s; check time synchronization (NTP) and the controller's clock: %w", a.clock().UTC().Format(time.RFC3339), err)
	}
	return err
}

func (a *Agent) checkSkew(controller time.Time) {
	if controller.IsZero() {
		return
	}
	skew := a.clock().Sub(controller)
	if skew > maxSkew || skew < -maxSkew {
		a.log.Warn("clock skew: the worker and controller clocks differ; certificate renewal and TLS will fail once it grows", "skew", skew.Round(time.Second).String())
	}
}

func (a *Agent) bundleVersion() uint64 {
	raw, err := os.ReadFile(a.path("bundle-version"))
	if err != nil {
		return 1
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 1
	}
	return v
}

// storeBundle replaces the trusted CAs with a bundle received over an authenticated channel.
func (a *Agent) storeBundle(b *workerv1.TrustBundle) error {
	if b == nil || len(b.GetAnchors()) == 0 || (a.haveBundle() && b.GetBundleVersion() <= a.bundleVersion()) {
		return nil
	}
	var out []byte
	for _, anchor := range b.GetAnchors() {
		ca, err := x509.ParseCertificate(anchor.GetCaCertificateDer())
		if err != nil || !ca.IsCA {
			return errors.New("the trust bundle holds an invalid CA certificate")
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})...)
	}
	if err := writeAtomic(a.path("ca-bundle.pem"), out, 0o644); err != nil {
		return err
	}
	if err := writeAtomic(a.path("bundle-version"), []byte(strconv.FormatUint(b.GetBundleVersion(), 10)+"\n"), 0o644); err != nil {
		return err
	}
	a.log.Info("trust bundle updated", "version", b.GetBundleVersion(), "cas", len(b.GetAnchors()))
	return nil
}

func (a *Agent) haveBundle() bool {
	_, err := os.Stat(a.path("ca-bundle.pem"))
	return err == nil
}

// renewDue reports whether the certificate has passed two-thirds of its lifetime.
func (a *Agent) renewDue() bool {
	c, err := a.certificate()
	if err != nil {
		return false
	}
	life := c.NotAfter.Sub(c.NotBefore)
	return a.clock().After(c.NotBefore.Add(life * 2 / 3))
}

// renew replaces the worker's key and certificate with fresh ones over the current mTLS channel (SEC-ID-01).
func (a *Agent) renew(ctx context.Context, client workerv1.WorkerServiceClient) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := client.RenewCertificate(rctx, &workerv1.RenewCertificateRequest{CsrDer: csr, AcknowledgedBundleVersion: a.bundleVersion()})
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(resp.GetCertificateDer())
	if err != nil {
		return err
	}
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey) {
		return errors.New("the renewed certificate does not match the new key")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := a.storeBundle(resp.GetTrustBundle()); err != nil {
		return err
	}
	if err := writeAtomic(a.path("worker.key.new"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := writeAtomic(a.path("worker.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644); err != nil {
		return err
	}
	if err := os.Rename(a.path("worker.key.new"), a.path("worker.key")); err != nil {
		return err
	}
	a.log.Info("worker certificate renewed", "expires", cert.NotAfter)
	return nil
}

// recoverKey finishes a renewal interrupted between writing the certificate and the key.
func (a *Agent) recoverKey() {
	if _, err := os.Stat(a.path("worker.key.new")); err != nil {
		return
	}
	if a.stagedKeyMatches() {
		_ = os.Rename(a.path("worker.key.new"), a.path("worker.key"))
	} else {
		_ = os.Remove(a.path("worker.key.new"))
	}
}

func (a *Agent) stagedKeyMatches() bool {
	c, err := a.certificate()
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(a.path("worker.key.new"))
	if err != nil {
		return false
	}
	b, _ := pem.Decode(raw)
	if b == nil {
		return false
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return false
	}
	pk, ok := k.(*ecdsa.PrivateKey)
	cpk, cok := c.PublicKey.(*ecdsa.PublicKey)
	return ok && cok && cpk.Equal(&pk.PublicKey)
}

func writeAtomic(name string, data []byte, mode os.FileMode) error {
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

func previousID(a *Agent) string {
	raw, err := os.ReadFile(a.path("worker-id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// bundleHasPin reports whether a bundle holds the CA the operator pinned.
func bundleHasPin(b *workerv1.TrustBundle, pin string) bool {
	for _, anchor := range b.GetAnchors() {
		if ca, err := x509.ParseCertificate(anchor.GetCaCertificateDer()); err == nil && pki.Pin(ca) == pin {
			return true
		}
	}
	return false
}
