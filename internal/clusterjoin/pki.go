// Package clusterjoin lets a controller join an existing cluster with a short-lived token instead of copying keys
// between hosts (docs/operators/high-availability.md#join). It owns the cluster CA, which signs only controller-side
// certificates (NATS members, the controller's NATS client and the controller API); worker certificates come from
// the separate internal CA, so a worker certificate can never reach NATS.
package clusterjoin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"samuori.invalid/samuori/internal/pki"
)

const (
	caCert = "ca.crt"
	caKey  = "ca.key"
	// CertValidity is how long issued controller certificates last; rejoin or reissue before they expire.
	CertValidity = 397 * 24 * time.Hour
)

type Authority struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

// Create writes a new cluster CA into dir, which must not hold one yet.
func Create(dir string) (*Authority, error) {
	if _, err := os.Stat(filepath.Join(dir, caKey)); err == nil {
		return nil, errors.New("clusterjoin: a cluster CA already exists in " + dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "samuori cluster CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, caKey), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, caCert), certPEM, 0o600); err != nil {
		return nil, err
	}
	return Open(dir)
}

// Open loads the cluster CA from dir.
func Open(dir string) (*Authority, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, caCert))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKey))
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("clusterjoin: the cluster CA is unreadable")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("clusterjoin: the cluster CA key is not ECDSA")
	}
	return &Authority{cert: cert, key: key, certPEM: certPEM}, nil
}

func (a *Authority) CertPEM() []byte { return a.certPEM }

// Kind is what an issued certificate is for.
type Kind int

const (
	// NATSServer certificates serve clients and routes, so they carry both usages.
	NATSServer Kind = iota
	NATSClient
	API
)

// Pair is a PEM certificate and its private key.
type Pair struct {
	Cert []byte `json:"cert"`
	Key  []byte `json:"key"`
}

// Issue signs a certificate of kind for host, a DNS name or an IP address.
func (a *Authority) Issue(kind Kind, host string) (Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: host}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(CertValidity),
		KeyUsage: x509.KeyUsageDigitalSignature}
	switch kind {
	case NATSServer:
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	case NATSClient:
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		tmpl.Subject.CommonName = "samuori-controller " + host
	case API:
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	default:
		return Pair{}, fmt.Errorf("clusterjoin: unknown certificate kind %d", kind)
	}
	if kind != NATSClient {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = []net.IP{ip}
		} else {
			tmpl.DNSNames = []string{host}
		}
		if kind == API {
			tmpl.DNSNames = append(tmpl.DNSNames, "localhost")
			tmpl.IPAddresses = append(tmpl.IPAddresses, net.IPv4(127, 0, 0, 1))
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return Pair{}, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Pair{}, err
	}
	return Pair{Cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), Key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// Pin is the SHA-256 of a certificate's public key, in the format joining controllers check (pki.Pin).
func Pin(certPEM []byte) (string, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return "", errors.New("clusterjoin: no certificate")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return "", err
	}
	return pki.Pin(c), nil
}
