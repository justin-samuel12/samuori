// Package recipes holds the versioned, approved build recipes a BuildSpec may reference (D16). Built-in recipes come
// from the embedded catalog; administrators control each version's status per organization.
package recipes

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/recipedef"
	"samuori.invalid/samuori/internal/recipes/catalog"
)

const (
	Approved   = "approved"
	Deprecated = "deprecated"
	Withdrawn  = "withdrawn"
)

const (
	SourceBuiltIn      = "built-in"
	SourceOrganization = "organization"
)

type Defaults = catalog.Defaults

// Runtime is a shipped runtime or one an administrator added to a built-in recipe (ADR 0051). ID is set only for
// organization runtimes. A shipped runtime's Status is the organization's decision about it (ADR 0060); StatusBy and
// StatusAt are set when an administrator made one.
type Runtime struct {
	catalog.Runtime
	ID, Source, Status, Reason string
	StatusBy                   string
	StatusAt                   *time.Time
}

type Recipe struct {
	ID               string    `json:"id"`
	Version          string    `json:"version"`
	DisplayName      string    `json:"displayName"`
	Description      string    `json:"description"`
	Languages        []string  `json:"languages"`
	ApplicationTypes []string  `json:"applicationTypes"`
	Platforms        []string  `json:"platforms"`
	EgressPresets    []string  `json:"egressPresets"`
	Runtimes         []Runtime `json:"runtimes"`
	Defaults         Defaults  `json:"defaults"`
	// Unavailable holds withdrawn shipped runtimes and organization runtimes that are drafts or withdrawn, so
	// resolution can say why.
	Unavailable []Runtime `json:"-"`
	// Custom is set for an administrator's recipe (ADR 0041); it has no runtimes.
	Custom *recipedef.Definition `json:"-"`
}

var pinned = regexp.MustCompile(`^[a-z0-9.-]+(?::[0-9]+)?/[a-z0-9._/-]+:[A-Za-z0-9._-]+@sha256:[a-f0-9]{64}$`)

// Catalog is the built-in set, keyed by "id@version".
type Catalog struct{ byKey map[string]Recipe }

func LoadBuiltin() (*Catalog, error) {
	presets, err := egress.Builtin()
	if err != nil {
		return nil, err
	}
	entries, err := catalog.Load()
	if err != nil {
		return nil, err
	}
	c := &Catalog{byKey: map[string]Recipe{}}
	for _, e := range entries {
		r := Recipe{ID: e.ID, Version: e.Version, DisplayName: e.DisplayName, Description: e.Description, Languages: e.Languages,
			ApplicationTypes: e.ApplicationTypes, Platforms: e.Platforms, EgressPresets: e.EgressPresets, Runtimes: []Runtime{}, Defaults: e.Defaults}
		for _, rt := range e.Runtimes {
			r.Runtimes = append(r.Runtimes, Runtime{Runtime: rt, Source: SourceBuiltIn, Status: Approved})
		}
		for _, id := range r.EgressPresets {
			if _, ok := presets.Get(id); !ok {
				return nil, fmt.Errorf("recipe %s %s: unknown egress preset %q", r.ID, r.Version, id)
			}
		}
		for _, rt := range r.Runtimes {
			images := []string{rt.BuildImage, rt.RunImage}
			for _, img := range rt.RunImages {
				images = append(images, img)
			}
			for _, img := range images {
				if !pinned.MatchString(img) {
					return nil, fmt.Errorf("recipe %s %s: image %q is not pinned by digest", r.ID, r.Version, img)
				}
			}
		}
		c.byKey[r.ID+"@"+r.Version] = r
	}
	return c, nil
}

func (c *Catalog) Get(id, version string) (Recipe, bool) {
	r, ok := c.byKey[id+"@"+version]
	return r, ok
}

// idsNamed lists the built-in versions with this ID.
func (c *Catalog) idsNamed(id string) []string {
	var out []string
	for k, r := range c.byKey {
		if r.ID == id {
			out = append(out, k)
		}
	}
	return out
}

func (c *Catalog) All() []Recipe {
	out := make([]Recipe, 0, len(c.byKey))
	for _, r := range c.byKey {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return compare(parse(out[i].Version), parse(out[j].Version)) < 0
	})
	return out
}

// ResolveRuntime picks the newest runtime satisfying constraint (for example ">=22", "^3.12", "3.13", "net9.0").
// Shipped runtimes win over organization runtimes, so adding one never changes what an existing spec resolves to
// (ADR 0051). Withdrawn runtimes are never picked (ADR 0060).
func (r Recipe) ResolveRuntime(constraint string) (Runtime, error) {
	for _, source := range []string{SourceBuiltIn, SourceOrganization} {
		best, err := newest(r.Runtimes, source, constraint)
		if err != nil {
			return Runtime{}, err
		}
		if best != nil {
			return *best, nil
		}
	}
	if rt, _ := newest(r.Unavailable, "", constraint); rt != nil {
		if rt.Status == Draft {
			return Runtime{}, fmt.Errorf("runtime %s of %s is a draft that an administrator has not approved", rt.Version, r.DisplayName)
		}
		return Runtime{}, fmt.Errorf("runtime %s of %s has been withdrawn by an administrator: %s", rt.Version, r.DisplayName, rt.Reason)
	}
	versions := make([]string, 0, len(r.Runtimes))
	for _, rt := range r.Runtimes {
		versions = append(versions, rt.Version)
	}
	return Runtime{}, fmt.Errorf("no runtime of %s matches %q (available: %s)", r.DisplayName, constraint, strings.Join(versions, ", "))
}

// Listed is what a recipe offers, shipped runtimes first; withWithdrawn adds the withdrawn shipped runtimes that
// administrators manage.
func (r Recipe) Listed(withWithdrawn bool) []Runtime {
	out := slices.Clone(r.Runtimes)
	if withWithdrawn {
		for _, rt := range r.Unavailable {
			if rt.Source == SourceBuiltIn {
				out = append(out, rt)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Runtime) int {
		if a.Source != b.Source {
			if a.Source == SourceBuiltIn {
				return -1
			}
			return 1
		}
		return compare(parse(a.Version), parse(b.Version))
	})
	return out
}

func newest(list []Runtime, source, constraint string) (*Runtime, error) {
	var best *Runtime
	for i := range list {
		rt := list[i]
		if source != "" && rt.Source != source {
			continue
		}
		ok, err := Satisfies(rt.Version, constraint)
		if err != nil {
			return nil, err
		}
		if ok && (best == nil || compare(parse(rt.Version), parse(best.Version)) > 0) {
			best = &rt
		}
	}
	return best, nil
}

var comparator = regexp.MustCompile(`^(>=|<=|>|<|==|=|\^|~)?v?([0-9]+(?:\.[0-9]+)*)(?:\.[xX*])?$`)

// Satisfies reports whether version meets every space- or comma-separated comparator in constraint.
func Satisfies(version, constraint string) (bool, error) {
	constraint = strings.TrimPrefix(strings.TrimSpace(constraint), "net")
	if constraint == "" || constraint == "*" || constraint == "lts" {
		return true, nil
	}
	v := parse(version)
	for _, part := range strings.FieldsFunc(constraint, func(r rune) bool { return r == ',' || r == ' ' }) {
		m := comparator.FindStringSubmatch(part)
		if m == nil {
			return false, fmt.Errorf("unsupported version constraint %q", constraint)
		}
		want := parse(m[2])
		c := compare(v, want)
		var ok bool
		switch m[1] {
		case ">=":
			ok = c >= 0
		case ">":
			ok = c > 0
		case "<=":
			ok = c <= 0
		case "<":
			ok = c < 0
		case "^":
			ok = c >= 0 && v[0] == want[0]
		case "~":
			ok = c >= 0 && prefix(v, want[:min(2, len(want))])
		default:
			ok = prefix(v, want)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func parse(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

func compare(a, b []int) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func prefix(v, want []int) bool {
	if len(want) > len(v) {
		return false
	}
	for i := range want {
		if v[i] != want[i] {
			return false
		}
	}
	return true
}
