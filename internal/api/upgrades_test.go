package api

import (
	"testing"

	"samuori.invalid/samuori/internal/version"
)

// TestUpgradeStatus covers the requests the Upgrades & Diagnostics page makes.
func TestUpgradeStatus(t *testing.T) {
	l := newLive(t, false)
	l.setup(t)
	m := l.adminMember(t)
	for _, path := range []string{"/system/upgrades", "/system/releases", "/system/rollouts?limit=20"} {
		if code, _, b := l.json(t, m, "GET", path, "", nil); code != 200 {
			t.Errorf("GET %s = %d %v", path, code, b)
		}
	}
}

func TestVersionHeader(t *testing.T) {
	l := newLive(t, false)
	l.setup(t)
	m := l.adminMember(t)
	if _, h, _ := l.json(t, m, "GET", "/auth/session", "", nil); h.Get(VersionHeader) != version.Version {
		t.Errorf("signed in: %s = %q, want %q", VersionHeader, h.Get(VersionHeader), version.Version)
	}
	r := l.call(t, l.client(), "GET", "/me", "", nil)
	if v := r.Header.Get(VersionHeader); r.StatusCode != 401 || v != "" {
		t.Errorf("anonymous: status %d, %s = %q; the release is for signed-in users only", r.StatusCode, VersionHeader, v)
	}
}
