package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/recipes"
)

// TestBaseImageTagPinning: pinTags accepts a tag from an approved repository only when digests are required, and
// leaves the strict refusal in place when it is off (ADR 0049).
func TestBaseImageTagPinning(t *testing.T) {
	if _, err := Validate(KindBaseImages, []byte(`{"allowedRepositories":[],"requireDigest":false,"pinTags":true}`)); err == nil {
		t.Error("pinTags without requireDigest was accepted")
	}
	canonical, err := Validate(KindBaseImages, []byte(`{"allowedRepositories":["docker.io/library/node"],"requireDigest":true,"pinTags":true}`))
	if err != nil || !strings.Contains(string(canonical), `"pinTags":true`) {
		t.Fatalf("valid pinning document = %s, %v", canonical, err)
	}

	var stored Effective
	if err := stored.Set(KindBaseImages, json.RawMessage(`{"allowedRepositories":["docker.io/library/node"],"requireDigest":true}`)); err != nil {
		t.Fatal(err)
	}
	if stored.BaseImages.PinTags || stored.BaseImageAllowed("node:18-alpine") != ErrNotPinned || stored.NeedsPin("node:18-alpine") {
		t.Error("a stored policy without pinTags no longer refuses tags")
	}

	pinning := stored
	pinning.BaseImages.PinTags = true
	digest := "node:18-alpine@sha256:" + strings.Repeat("a", 64)
	for _, c := range []struct {
		ref     string
		allowed bool
		pin     bool
	}{
		{"node:18-alpine", true, true},
		{"docker.io/library/node", true, true},
		{digest, true, false},
		{"python:3.12", false, true},
	} {
		if got := pinning.BaseImageAllowed(c.ref) == ""; got != c.allowed {
			t.Errorf("%s allowed = %v, want %v", c.ref, got, c.allowed)
		}
		if got := pinning.NeedsPin(c.ref); got != c.pin {
			t.Errorf("%s needs a pin = %v, want %v", c.ref, got, c.pin)
		}
	}

	open := pinning
	open.BaseImages.RequireDigest, open.BaseImages.PinTags = false, false
	if open.NeedsPin("node:18-alpine") || open.BaseImageAllowed("node:18-alpine") != "" {
		t.Error("with digests not required, a tag must build unpinned as before")
	}

	cat, err := recipes.LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	if b := Defaults(cat)[KindBaseImages].(BaseImages); !b.RequireDigest || !b.PinTags {
		t.Errorf("fresh installs should require digests and pin tags: %+v", b)
	}
}
