// Package pki runs the controller's internal certificate authority for worker identities (ADR 0012).
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const workerURIPrefix = "urn:samuori:worker:"

type CA struct {
	Cert *x509.Certificate
	key  crypto.Signer
}

// LoadOrCreate reads ca.crt and ca.key from dir, creating a new CA (ECDSA P-256, 10 years) on first use.
func LoadOrCreate(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if certPEM, err := os.ReadFile(certPath); err == nil {
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		return parse(certPEM, keyPEM)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Samuori internal CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	return &CA{Cert: cert, key: key}, nil
}

func parse(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("pki: unreadable CA files")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("pki: CA key cannot sign")
	}
	return &CA{Cert: cert, key: signer}, nil
}

// Pin is the hex SHA-256 of a certificate's SubjectPublicKeyInfo, prefixed "sha256:".
func Pin(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (ca *CA) Pin() string { return Pin(ca.Cert) }

// ServerCertificate issues the worker endpoint's certificate, chained with the CA so a new worker can match its pin.
func (ca *CA) ServerCertificate(hosts []string, ttl time.Duration) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "samuori-controller"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(ttl),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

// SignWorker issues a client certificate for workerID from a CSR. The requested subject is ignored: the controller
// alone decides the identity.
func (ca *CA) SignWorker(csrDER []byte, workerID string, ttl time.Duration) (*x509.Certificate, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("pki: invalid CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("pki: CSR signature: %w", err)
	}
	switch k := csr.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return nil, errors.New("pki: worker keys must be ECDSA P-256/P-384 or Ed25519")
		}
	case ed25519.PublicKey:
	default:
		return nil, errors.New("pki: worker keys must be ECDSA P-256/P-384 or Ed25519")
	}
	id, _ := url.Parse(workerURIPrefix + workerID)
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "samuori-worker " + workerID},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(ttl), URIs: []*url.URL{id},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// WorkerID extracts the identity from a verified worker certificate.
func WorkerID(c *x509.Certificate) (string, bool) {
	for _, u := range c.URIs {
		if s := u.String(); strings.HasPrefix(s, workerURIPrefix) {
			return strings.TrimPrefix(s, workerURIPrefix), true
		}
	}
	return "", false
}

// VerifyPinned checks a server chain against a CA public-key pin, for a worker that has no trust bundle yet.
func VerifyPinned(pin, serverName string) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		var certs []*x509.Certificate
		for _, r := range raw {
			c, err := x509.ParseCertificate(r)
			if err != nil {
				return err
			}
			certs = append(certs, c)
		}
		if len(certs) == 0 {
			return errors.New("pki: no server certificate")
		}
		roots := x509.NewCertPool()
		for _, c := range certs[1:] {
			if c.IsCA && Pin(c) == pin {
				roots.AddCert(c)
			}
		}
		_, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, DNSName: serverName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		if err != nil {
			return fmt.Errorf("pki: controller does not chain to the pinned CA: %w", err)
		}
		return nil
	}
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

const federationScheme = "samuori-federation"

// FederationCertificate issues this installation's federation identity: a certificate for both ends of a peer
// connection, naming the organization in a URI SAN and chained with the CA so peers can match their pin (ADR 0037).
func (ca *CA) FederationCertificate(orgID string, hosts []string, ttl time.Duration) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "samuori-federation"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(ttl),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{{Scheme: federationScheme, Opaque: "org:" + orgID}},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

// ErrPeerIdentity means a presented federation certificate does not chain to the pinned peer CA or names another
// organization.
var ErrPeerIdentity = errors.New("pki: the peer's certificate does not match its registration")

// VerifyFederationPeer checks a presented chain [leaf, CA] against a peer's CA pin and organization, for use as both
// client and server verification. serverName, when set, must be in the leaf.
func VerifyFederationPeer(raw [][]byte, pin, orgID, serverName string, usage x509.ExtKeyUsage, now time.Time) error {
	if len(raw) < 2 {
		return ErrPeerIdentity
	}
	leaf, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return ErrPeerIdentity
	}
	ca, err := x509.ParseCertificate(raw[len(raw)-1])
	if err != nil || !ca.IsCA || Pin(ca) != pin {
		return ErrPeerIdentity
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	opts := x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}, DNSName: serverName}
	if _, err := leaf.Verify(opts); err != nil {
		return ErrPeerIdentity
	}
	if got, ok := FederationOrg(leaf); !ok || got != orgID {
		return ErrPeerIdentity
	}
	return nil
}

// FederationOrg returns the organization a federation certificate names.
func FederationOrg(c *x509.Certificate) (string, bool) {
	for _, u := range c.URIs {
		if org, ok := strings.CutPrefix(u.Opaque, "org:"); u.Scheme == federationScheme && ok && org != "" {
			return org, true
		}
	}
	return "", false
}
