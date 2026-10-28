package sources

import (
	"errors"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var errBadPath = errors.New("unsafe path")

const (
	maxPathBytes    = 1024
	maxSegmentBytes = 255
)

// normalizePath returns the NFC, slash-separated relative form of an archive entry name, or errBadPath for
// absolute, traversing, drive-qualified, backslashed, control-character or overlong names (TM-10).
func normalizePath(raw string, maxDepth int) (string, error) {
	if raw == "" || !utf8.ValidString(raw) || strings.ContainsRune(raw, '\\') || strings.HasPrefix(raw, "/") {
		return "", errBadPath
	}
	if len(raw) >= 2 && raw[1] == ':' && (raw[0]|0x20 >= 'a' && raw[0]|0x20 <= 'z') {
		return "", errBadPath
	}
	for _, r := range raw {
		if unicode.IsControl(r) || r == unicode.ReplacementChar || unicode.Is(unicode.Bidi_Control, r) {
			return "", errBadPath
		}
	}
	p := strings.TrimSuffix(raw, "/")
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	p = norm.NFC.String(p)
	if p == "" || p == "." || len(p) > maxPathBytes {
		return "", errBadPath
	}
	segs := strings.Split(p, "/")
	if len(segs) > maxDepth {
		return "", errBadPath
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || len(s) > maxSegmentBytes {
			return "", errBadPath
		}
	}
	return p, nil
}

var folder = cases.Fold()

// collisionKey identifies paths that would clash on case-insensitive or normalization-insensitive filesystems.
func collisionKey(p string) string { return norm.NFC.String(folder.String(p)) }

// tree records accepted paths and detects collisions between files, directories and case/normalization variants.
type tree struct {
	kinds map[string]bool // collision key -> isDir
	names map[string]string
}

func newTree() *tree { return &tree{kinds: map[string]bool{}, names: map[string]string{}} }

type collision struct{ a, b string }

func (c *collision) Error() string { return "paths collide: " + c.a + " and " + c.b }

func (t *tree) add(p string, dir bool) error {
	parent := path.Dir(p)
	for parent != "." {
		k := collisionKey(parent)
		if isDir, ok := t.kinds[k]; ok && !isDir {
			return &collision{t.names[k], p}
		}
		if _, ok := t.kinds[k]; !ok {
			t.kinds[k], t.names[k] = true, parent
		}
		parent = path.Dir(parent)
	}
	k := collisionKey(p)
	if isDir, ok := t.kinds[k]; ok {
		if dir && isDir && t.names[k] == p {
			return nil
		}
		return &collision{t.names[k], p}
	}
	t.kinds[k], t.names[k] = dir, p
	return nil
}
