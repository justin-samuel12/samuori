// Package redact masks delivered secret values in build logs. Masking is defense in depth only: it cannot catch
// values the build transforms (docs/architecture/secrets.md).
package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"sync"
)

// MinLength is the shortest value masked; shorter values would mask ordinary log text.
const MinLength = 4

type Masker struct {
	mu       sync.Mutex
	patterns map[string]string
}

func New() *Masker { return &Masker{patterns: map[string]string{}} }

// Add registers a released value and its common encodings under a label such as the logical secret ID.
func (m *Masker) Add(label string, value []byte) {
	if len(value) < MinLength {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	repl := "[masked secret " + label + "]"
	for _, p := range [][]byte{value, bytes.TrimRight(value, "\r\n"), []byte(base64.StdEncoding.EncodeToString(value)), []byte(base64.RawStdEncoding.EncodeToString(value)),
		[]byte(base64.URLEncoding.EncodeToString(value)), []byte(base64.RawURLEncoding.EncodeToString(value)), []byte(hex.EncodeToString(value))} {
		if len(p) >= MinLength {
			m.patterns[string(p)] = repl
		}
	}
}

// Mask replaces every registered pattern, longest first.
func (m *Masker) Mask(b []byte) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.patterns))
	for k := range m.patterns {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		b = bytes.ReplaceAll(b, []byte(k), []byte(m.patterns[k]))
	}
	return b
}

// Contains reports whether b holds a registered value or encoding.
func (m *Masker) Contains(b []byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.patterns {
		if bytes.Contains(b, []byte(k)) {
			return true
		}
	}
	return false
}
