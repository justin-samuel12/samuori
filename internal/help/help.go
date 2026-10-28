// Package help serves the in-app user guides. Each guide names the roles that may read it; the server filters, so
// a guide a caller may not read is not found (docs/product/help.md).
package help

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/authz"
)

//go:embed content/*.md
var content embed.FS

// Everyone marks a guide every signed-in user may read, including users who hold no role yet.
const Everyone = "everyone"

type Topic struct {
	ID      string
	Title   string   `yaml:"title"`
	Section string   `yaml:"section"`
	Summary string   `yaml:"summary"`
	Roles   []string `yaml:"roles"`
	Order   int      `yaml:"order"`
	Body    string
}

type Library struct {
	topics []Topic
	byID   map[string]Topic
}

var topicID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var knownRoles = map[string]bool{Everyone: true}

func init() {
	for _, r := range []authz.Role{authz.Developer, authz.Maintainer, authz.Publisher, authz.Operator, authz.SecurityReviewer, authz.Administrator} {
		knownRoles[string(r)] = true
	}
}

// Load parses the embedded guides. A guide without roles, with an unknown role or with a bad ID fails the load, so
// a mistake cannot publish a guide to everyone.
func Load() (*Library, error) {
	return load(content, "content")
}

func load(fsys fs.FS, dir string) (*Library, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	l := &Library{byID: map[string]Topic{}}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".md" {
			continue
		}
		raw, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		t, err := parse(strings.TrimSuffix(e.Name(), ".md"), raw)
		if err != nil {
			return nil, fmt.Errorf("help: %s: %w", e.Name(), err)
		}
		l.topics = append(l.topics, t)
		l.byID[t.ID] = t
	}
	sort.SliceStable(l.topics, func(i, j int) bool {
		a, b := l.topics[i], l.topics[j]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.Title < b.Title
	})
	return l, nil
}

func parse(id string, raw []byte) (Topic, error) {
	if !topicID.MatchString(id) {
		return Topic{}, fmt.Errorf("the file name must be a lower-case topic ID")
	}
	rest, ok := bytes.CutPrefix(raw, []byte("---\n"))
	if !ok {
		return Topic{}, fmt.Errorf("missing front matter")
	}
	head, body, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return Topic{}, fmt.Errorf("unterminated front matter")
	}
	var t Topic
	if err := yaml.Unmarshal(head, &t); err != nil {
		return Topic{}, err
	}
	if t.Title == "" || t.Section == "" || len(t.Roles) == 0 {
		return Topic{}, fmt.Errorf("title, section and roles are required")
	}
	for _, r := range t.Roles {
		if !knownRoles[r] {
			return Topic{}, fmt.Errorf("unknown role %q", r)
		}
	}
	t.ID = id
	t.Body = strings.TrimSpace(string(body))
	return t, nil
}

// VisibleTo reports whether p may read the guide: it is for everyone, or p holds one of its roles at any scope.
func (t Topic) VisibleTo(p authz.Principal) bool {
	if slices.Contains(t.Roles, Everyone) {
		return true
	}
	for _, b := range p.Bindings {
		if slices.Contains(t.Roles, string(b.Role)) {
			return true
		}
	}
	return false
}

// For lists the guides p may read, in reading order.
func (l *Library) For(p authz.Principal) []Topic {
	out := []Topic{}
	for _, t := range l.topics {
		if t.VisibleTo(p) {
			out = append(out, t)
		}
	}
	return out
}

// Get returns a guide p may read; any other guide is reported as missing.
func (l *Library) Get(p authz.Principal, id string) (Topic, bool) {
	t, ok := l.byID[id]
	if !ok || !t.VisibleTo(p) {
		return Topic{}, false
	}
	return t, true
}
