package workerjoin

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/pki"
)

var (
	testPin   = "sha256:" + strings.Repeat("ab", 32)
	testToken = "samuoriw_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
)

func TestWorkerJoinCode(t *testing.T) {
	t.Run("SEC-WRK-11 round trip", func(t *testing.T) {
		in := Code{Controllers: []string{"ctrl1.example:8444", "ctrl2.example:8444"}, Pin: testPin, Token: testToken}
		s, err := Encode(in)
		if err != nil || !strings.HasPrefix(s, Prefix) {
			t.Fatalf("encode = %q, %v", s, err)
		}
		out, err := Decode("  " + s + "\n")
		if err != nil || !slices.Equal(out.Controllers, in.Controllers) || out.Pin != in.Pin || out.Token != in.Token {
			t.Fatalf("decode = %+v, %v", out, err)
		}
		if out.ServerName() != "ctrl1.example" {
			t.Fatalf("server name = %q", out.ServerName())
		}
	})

	t.Run("SEC-WRK-11 malformed codes are refused", func(t *testing.T) {
		enc := func(raw string) string { return Prefix + base64.RawURLEncoding.EncodeToString([]byte(raw)) }
		for name, s := range map[string]string{
			"controller code":  "samuoric_" + base64.RawURLEncoding.EncodeToString([]byte(`{}`)),
			"bare token":       testToken,
			"not base64":       Prefix + "!!!",
			"no endpoint":      enc(`{"controllers":[],"pin":"` + testPin + `","token":"` + testToken + `"}`),
			"endpoint no port": enc(`{"controllers":["ctrl1"],"pin":"` + testPin + `","token":"` + testToken + `"}`),
			"shell in host":    enc(`{"controllers":["a;reboot:8444"],"pin":"` + testPin + `","token":"` + testToken + `"}`),
			"short pin":        enc(`{"controllers":["c:8444"],"pin":"sha256:ab","token":"` + testToken + `"}`),
			"controller token": enc(`{"controllers":["c:8444"],"pin":"` + testPin + `","token":"samuorij_` + strings.Repeat("A", 43) + `"}`),
			"unknown field":    enc(`{"controllers":["c:8444"],"pin":"` + testPin + `","token":"` + testToken + `","dsn":"x"}`),
			"too long":         Prefix + strings.Repeat("A", maxLen),
		} {
			if _, err := Decode(s); err == nil {
				t.Errorf("%s: decoded", name)
			}
		}
		if _, err := Encode(Code{Pin: testPin, Token: testToken}); err == nil {
			t.Error("encoded a code without endpoints")
		}
	})

	t.Run("endpoints", func(t *testing.T) {
		if got := Endpoints("127.0.0.1:8444", []string{"samuori.example"}, nil); got != nil {
			t.Errorf("loopback listener = %v, want none", got)
		}
		got := Endpoints("0.0.0.0:8444", []string{"localhost", "127.0.0.1", "samuori.example", "::1"}, []string{"ctrl2.example", "samuori.example", ""})
		if want := []string{"samuori.example:8444", "ctrl2.example:8444"}; !slices.Equal(got, want) {
			t.Errorf("endpoints = %v, want %v", got, want)
		}
	})

	t.Run("SEC-WRK-11 the pinned certificate is checked before installing", func(t *testing.T) {
		ca, err := pki.LoadOrCreate(filepath.Join(t.TempDir(), "ca"))
		if err != nil {
			t.Fatal(err)
		}
		other, err := pki.LoadOrCreate(filepath.Join(t.TempDir(), "other"))
		if err != nil {
			t.Fatal(err)
		}
		addr := serve(t, ca, "samuori.example")
		dead := closedPort(t)
		ctx := context.Background()

		// The test endpoint answers on 127.0.0.1; dial it by IP with the certificate's name as server name.
		code := func(pin string, endpoints ...string) Code {
			return Code{Controllers: endpoints, Pin: pin, Token: testToken}
		}
		if err := checkAs(ctx, code(ca.Pin(), dead, addr), "samuori.example"); err != nil {
			t.Fatalf("a matching pin after an unreachable endpoint was refused: %v", err)
		}
		if err := checkAs(ctx, code(other.Pin(), addr), "samuori.example"); err == nil || !strings.Contains(err.Error(), "pinned CA") {
			t.Fatalf("a pin mismatch was accepted: %v", err)
		}
		if err := checkAs(ctx, code(ca.Pin(), addr), "elsewhere.example"); err == nil {
			t.Fatal("a certificate without the expected name was accepted")
		}
		if err := checkAs(ctx, code(ca.Pin(), dead), "samuori.example"); err == nil || !strings.Contains(err.Error(), "no controller answers") {
			t.Fatalf("no reachable controller = %v", err)
		}
	})
}

func checkAs(ctx context.Context, c Code, serverName string) error {
	return check(ctx, c, serverName, 2*time.Second)
}

func serve(t *testing.T, ca *pki.CA, host string) string {
	t.Helper()
	cert, err := ca.ServerCertificate([]string{host}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}, ClientAuth: tls.VerifyClientCertIfGiven})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
