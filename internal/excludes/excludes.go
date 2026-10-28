// Package excludes holds the upload exclusion rules shared by the server and the CLI (docs/architecture/sources.md).
package excludes

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Excluded by default: version-control metadata, dependency and cache directories, OS clutter, and files that
// usually hold credentials. Template env files are kept because applications document settings with them.
var defaultExcludes = []string{
	".git", ".hg", ".svn", "node_modules", "__pycache__", ".venv", ".DS_Store", "Thumbs.db",
	".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
}

var keptEnvTemplates = map[string]bool{".env.example": true, ".env.sample": true, ".env.template": true, ".env.dist": true}

var secretFilePatterns = map[string]bool{"*.pem": true, "*.key": true, "*.p12": true, "*.pfx": true, "id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true, ".env": true, ".env.*": true}

// Defaults, KeptFiles and SecretFilePatterns describe the fixed exclusion rules for upload previews.
func Defaults() []string { return append([]string{}, defaultExcludes...) }

func KeptFiles() []string {
	out := make([]string, 0, len(keptEnvTemplates))
	for n := range keptEnvTemplates {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func SecretFilePatterns() []string {
	var out []string
	for _, p := range defaultExcludes {
		if secretFilePatterns[p] {
			out = append(out, p)
		}
	}
	return out
}

// Matcher applies the default exclusions and a user's additional patterns.
type Matcher struct{ patterns []string }

// Validate rejects user patterns that are malformed or try to negate the defaults.
func Validate(patterns []string) error {
	for _, p := range patterns {
		if p == "" || strings.HasPrefix(p, "!") || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
			return fmt.Errorf("exclude pattern %q is not allowed", p)
		}
		if _, err := path.Match(strings.TrimSuffix(p, "/"), "x"); err != nil {
			return fmt.Errorf("exclude pattern %q is malformed", p)
		}
	}
	return nil
}

func New(user []string) Matcher {
	ps := append([]string{}, defaultExcludes...)
	for _, p := range user {
		ps = append(ps, strings.TrimSuffix(p, "/"))
	}
	return Matcher{patterns: ps}
}

// Match reports the pattern excluding p: patterns without a slash match any path segment, others match the full
// relative path or one of its parent directories.
func (e Matcher) Match(p string) (string, bool) {
	segs := strings.Split(p, "/")
	for _, pat := range e.patterns {
		if strings.Contains(pat, "/") {
			for i := len(segs); i > 0; i-- {
				if ok, _ := path.Match(pat, strings.Join(segs[:i], "/")); ok {
					return pat, true
				}
			}
			continue
		}
		for _, s := range segs {
			if ok, _ := path.Match(pat, s); ok && !(pat == ".env.*" && keptEnvTemplates[s]) {
				return pat, true
			}
		}
	}
	return "", false
}

// IsSecretFile reports whether a default pattern exists to keep credential files out.
func IsSecretFile(pattern string) bool { return secretFilePatterns[pattern] }
