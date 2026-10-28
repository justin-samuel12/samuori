package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA states in a trust bundle (ADR 0026). Exactly one CA issues; a "next" CA is trusted before it issues, and a
// retiring CA stays trusted until the certificates it issued have expired.
const (
	StateIssuing  = "issuing"
	StateNext     = "next"
	StateRetiring = "retiring"
)

var (
	ErrRotationInProgress = errors.New("pki: a CA rotation is already in progress")
	ErrNoRotation         = errors.New("pki: no CA rotation is in progress")
)

type entry struct {
	Name        string     `json:"name"`
	State       string     `json:"state"`
	RetireAfter *time.Time `json:"retireAfter,omitempty"`
}

type authorityState struct {
	Version uint64  `json:"bundleVersion"`
	CAs     []entry `json:"cas"`
}

// Authority holds the controller's CAs and its trust bundle in a directory. The bundle version changes whenever the
// set of trusted CAs changes, so workers can acknowledge exactly what they hold.
type Authority struct {
	dir    string
	mu     sync.Mutex
	st     authorityState
	cas    map[string]*CA
	onSave func() error
}

// AnchorInfo describes one CA in the bundle.
type AnchorInfo struct {
	Pin         string
	State       string
	NotBefore   time.Time
	NotAfter    time.Time
	RetireAfter *time.Time
}

// Open loads the authority in dir, creating its first CA on first use. A directory from before CA rotation, holding
// only ca.crt and ca.key, becomes a bundle with that CA issuing.
func Open(dir string) (*Authority, error) {
	a := &Authority{dir: dir, cas: map[string]*CA{}}
	raw, err := os.ReadFile(filepath.Join(dir, "authority.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, err := LoadOrCreate(dir); err != nil {
			return nil, err
		}
		a.st = authorityState{Version: 1, CAs: []entry{{Name: "ca", State: StateIssuing}}}
		if err := a.save(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &a.st); err != nil {
			return nil, fmt.Errorf("pki: authority.json: %w", err)
		}
	}
	if err := a.loadCAs(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Authority) loadCAs() error {
	cas := map[string]*CA{}
	for _, e := range a.st.CAs {
		ca, err := load(a.dir, e.Name)
		if err != nil {
			return fmt.Errorf("pki: CA %s: %w", e.Name, err)
		}
		cas[e.Name] = ca
	}
	a.cas = cas
	if a.issuingEntry() == nil {
		return errors.New("pki: no issuing CA in authority.json")
	}
	return nil
}

// OnSave runs fn after every change is written, for example to share the directory with other controllers. A
// failing fn fails the change.
func (a *Authority) OnSave(fn func() error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onSave = fn
}

// Reload reads the directory again after another controller changed it, reporting whether the trust bundle changed.
// On error the authority keeps its previous state.
func (a *Authority) Reload() (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(a.dir, "authority.json"))
	if err != nil {
		return false, err
	}
	var st authorityState
	if err := json.Unmarshal(raw, &st); err != nil {
		return false, fmt.Errorf("pki: authority.json: %w", err)
	}
	prevSt, prevCAs := a.st, a.cas
	a.st = st
	if err := a.loadCAs(); err != nil {
		a.st, a.cas = prevSt, prevCAs
		return false, err
	}
	return st.Version != prevSt.Version || a.issuingEntry().Name != issuingName(prevSt), nil
}

func issuingName(st authorityState) string {
	for _, e := range st.CAs {
		if e.State == StateIssuing {
			return e.Name
		}
	}
	return ""
}

func load(dir, name string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, name+".crt"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, name+".key"))
	if err != nil {
		return nil, err
	}
	return parse(certPEM, keyPEM)
}

func (a *Authority) save() error {
	raw, err := json.MarshalIndent(a.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(a.dir, "authority.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(a.dir, "authority.json")); err != nil {
		return err
	}
	if a.onSave != nil {
		return a.onSave()
	}
	return nil
}

func (a *Authority) issuingEntry() *entry {
	for i := range a.st.CAs {
		if a.st.CAs[i].State == StateIssuing {
			return &a.st.CAs[i]
		}
	}
	return nil
}

// Issuing returns the CA that signs new worker and server certificates.
func (a *Authority) Issuing() *CA {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cas[a.issuingEntry().Name]
}

// Bundle returns the trusted CA certificates and the bundle version.
func (a *Authority) Bundle() (uint64, []*x509.Certificate) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*x509.Certificate, 0, len(a.st.CAs))
	for _, e := range a.st.CAs {
		out = append(out, a.cas[e.Name].Cert)
	}
	return a.st.Version, out
}

// Pool returns the trusted CAs as a certificate pool, for verifying worker client certificates.
func (a *Authority) Pool() *x509.CertPool {
	_, cas := a.Bundle()
	pool := x509.NewCertPool()
	for _, c := range cas {
		pool.AddCert(c)
	}
	return pool
}

// Anchors describes the bundle for operators.
func (a *Authority) Anchors() (uint64, []AnchorInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AnchorInfo, 0, len(a.st.CAs))
	for _, e := range a.st.CAs {
		c := a.cas[e.Name].Cert
		out = append(out, AnchorInfo{Pin: Pin(c), State: e.State, NotBefore: c.NotBefore, NotAfter: c.NotAfter, RetireAfter: e.RetireAfter})
	}
	return a.st.Version, out
}

// BeginRotation creates the next CA and adds it to the bundle; it does not issue until Switch.
func (a *Authority) BeginRotation() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.st.CAs {
		if e.State == StateNext {
			return ErrRotationInProgress
		}
	}
	name, ca, err := a.create()
	if err != nil {
		return err
	}
	a.cas[name] = ca
	a.st.CAs = append(a.st.CAs, entry{Name: name, State: StateNext})
	a.st.Version++
	return a.save()
}

// Switch makes the next CA issue. The previous issuing CA stays trusted until retireAfter, when every certificate
// it issued has expired.
func (a *Authority) Switch(retireAfter time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var next *entry
	for i := range a.st.CAs {
		if a.st.CAs[i].State == StateNext {
			next = &a.st.CAs[i]
		}
	}
	if next == nil {
		return ErrNoRotation
	}
	old := a.issuingEntry()
	old.State, old.RetireAfter = StateRetiring, &retireAfter
	next.State = StateIssuing
	return a.save()
}

// RetireExpired removes retiring CAs whose retirement time has passed, reporting whether the bundle changed.
func (a *Authority) RetireExpired(now time.Time) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.st.CAs[:0]
	changed := false
	for _, e := range a.st.CAs {
		if e.State == StateRetiring && e.RetireAfter != nil && now.After(*e.RetireAfter) {
			a.destroy(e.Name)
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	if !changed {
		return false, nil
	}
	a.st.CAs = kept
	a.st.Version++
	return true, a.save()
}

// Emergency replaces every CA with a new issuing CA and deletes the old keys. Nothing signed by an old CA is trusted
// afterwards, so every worker must re-enroll out of band with the new pin (ADR 0026).
func (a *Authority) Emergency() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	name, ca, err := a.create()
	if err != nil {
		return err
	}
	for _, e := range a.st.CAs {
		a.destroy(e.Name)
	}
	a.cas = map[string]*CA{name: ca}
	a.st.CAs = []entry{{Name: name, State: StateIssuing}}
	a.st.Version++
	return a.save()
}

func (a *Authority) destroy(name string) {
	delete(a.cas, name)
	_ = os.Remove(filepath.Join(a.dir, name+".key"))
	_ = os.Rename(filepath.Join(a.dir, name+".crt"), filepath.Join(a.dir, name+".crt.retired"))
}

func (a *Authority) create() (string, *CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", nil, err
	}
	now := time.Now()
	name := fmt.Sprintf("ca-%d", now.UnixNano())
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Samuori internal CA " + now.UTC().Format("2006-01-02")},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return "", nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(a.dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(a.dir, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	return name, &CA{Cert: cert, key: key}, nil
}
