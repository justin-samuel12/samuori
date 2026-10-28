// Command tokensrv is a spike-only registry token issuer standing in for Samuori (OD-14). See docs/spikes/mirror-auth.md.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

var policy = map[string]map[string]bool{
	"p1": {"private/app": true},
	"p2": {"private/other": true},
}

func main() {
	addr := flag.String("addr", "127.0.0.1:5099", "listen address")
	certOut := flag.String("cert", "token.crt", "where to write the signing certificate")
	passwords := flag.String("passwords", "", "comma-separated project:password pairs")
	flag.Parse()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "samuori-spike-token-signer"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(err)
	must(os.WriteFile(*certOut, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))

	creds := map[string]string{}
	for _, pair := range strings.Split(*passwords, ",") {
		if p, pw, ok := strings.Cut(pair, ":"); ok {
			creds[p] = pw
		}
	}

	http.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		project := ""
		if u, pw, ok := r.BasicAuth(); ok && creds[u] != "" && creds[u] == pw {
			project = u
		}
		var access []map[string]any
		for _, scope := range r.URL.Query()["scope"] {
			parts := strings.Split(scope, ":")
			if len(parts) != 3 || parts[0] != "repository" {
				continue
			}
			granted := policy[project][parts[1]]
			log.Printf("token project=%q scope=%s granted=%v", project, scope, granted)
			if granted {
				access = append(access, map[string]any{"type": "repository", "name": parts[1], "actions": []string{"pull"}})
			}
		}
		now := time.Now()
		header := map[string]any{"alg": "ES256", "typ": "JWT", "x5c": []string{base64.StdEncoding.EncodeToString(der)}}
		claims := map[string]any{"iss": "samuori-spike", "sub": project, "aud": r.URL.Query().Get("service"),
			"iat": now.Unix(), "nbf": now.Add(-30 * time.Second).Unix(), "exp": now.Add(5 * time.Minute).Unix(),
			"jti": base64.RawURLEncoding.EncodeToString(randBytes(12)), "access": access}
		token := sign(key, header, claims)
		w.Header().Set("Content-Type", "application/json")
		must(json.NewEncoder(w).Encode(map[string]any{"token": token, "access_token": token, "expires_in": 300}))
	})
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func sign(key *ecdsa.PrivateKey, header, claims map[string]any) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	must(err)
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
