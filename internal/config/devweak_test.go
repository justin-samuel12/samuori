package config

import (
	"strings"
	"testing"
)

func TestDevWeakPasswordNeedsLocalAllInOne(t *testing.T) {
	t.Run("SEC-AUTH-10", func(t *testing.T) {
		base := []string{"--dev-weak-passwords=true"}
		load := func(extra ...string) error {
			_, _, err := Load(append(append([]string{}, base...), extra...), []string{"SAMUORI_DATABASE=sqlite:///tmp/x.db"}, func(string) ([]byte, error) { return nil, nil })
			return err
		}
		if err := load("--listen", "127.0.0.1:8080"); err != nil {
			t.Fatalf("refused on loopback all-in-one: %v", err)
		}
		for name, extra := range map[string][]string{
			"all interfaces": {"--listen", "0.0.0.0:8080"},
			"a LAN address":  {"--listen", "10.0.0.5:8080"},
			"a controller":   {"--listen", "127.0.0.1:8080", "--role", "controller"},
		} {
			if err := load(extra...); err == nil || !strings.Contains(err.Error(), "dev.weakPasswords") {
				t.Errorf("%s: accepted (%v)", name, err)
			}
		}
	})
}
