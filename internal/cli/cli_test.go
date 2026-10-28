package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveSettings: flags win over the environment and the environment over the profile; a profile's token is
// used only for its own URL and only from a private file.
func TestResolveSettings(t *testing.T) {
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "samuori")
	if err := saveConfig(dir, config{Current: "dev", Profiles: map[string]profile{
		"dev": {URL: "https://dev.example.com"}, "prod": {URL: "https://prod.example.com", CAFile: "/etc/ca.pem"}}}); err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"dev": "dev-token", "prod": "prod-token"} {
		if err := writePrivate(tokenPath(dir, name), []byte(token+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	fileToken := filepath.Join(cfg, "file-token")
	_ = os.WriteFile(fileToken, []byte("file-token\n"), 0o600)
	env := map[string]string{"XDG_CONFIG_HOME": cfg}
	getenv := func(k string) string { return env[k] }

	for _, c := range []struct {
		name               string
		g                  globalFlags
		envToken           string
		url, token, caFile string
	}{
		{"default profile", globalFlags{}, "", "https://dev.example.com", "dev-token", ""},
		{"selected profile", globalFlags{profile: "prod"}, "", "https://prod.example.com", "prod-token", "/etc/ca.pem"},
		{"environment token beats the profile", globalFlags{}, "env-token", "https://dev.example.com", "env-token", ""},
		{"token file beats the environment", globalFlags{tokenFile: fileToken}, "env-token", "https://dev.example.com", "file-token", ""},
		{"no profile token for another URL", globalFlags{url: "https://other.example.com"}, "", "https://other.example.com", "", ""},
		{"no profile CA for another URL", globalFlags{url: "https://other.example.com", profile: "prod"}, "env-token", "https://other.example.com", "env-token", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			env["SAMUORI_TOKEN"] = c.envToken
			s, err := resolveSettings(c.g, getenv)
			if err != nil || s.url != c.url || s.token != c.token || s.caFile != c.caFile {
				t.Fatalf("settings = %+v, %v", s, err)
			}
		})
	}
	env["SAMUORI_TOKEN"] = ""
	if _, err := resolveSettings(globalFlags{profile: "missing"}, getenv); err == nil || !strings.Contains(err.Error(), "samuori login") {
		t.Fatalf("unknown profile = %v", err)
	}
	if err := os.Chmod(tokenPath(dir, "dev"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSettings(globalFlags{}, getenv); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("readable token file = %v", err)
	}
}
