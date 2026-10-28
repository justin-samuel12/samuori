package help

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"samuori.invalid/samuori/internal/authz"
)

func principal(roles ...authz.Role) authz.Principal {
	p := authz.Principal{ID: "u", OrgID: "o1", Kind: authz.User}
	for _, r := range roles {
		p.Bindings = append(p.Bindings, authz.Binding{Role: r, ScopeType: authz.ScopeTeam, ScopeID: "t1"})
	}
	return p
}

func TestGuides(t *testing.T) {
	t.Run("SEC-RBAC-09", func(t *testing.T) {
		l, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		ids := func(ts []Topic) map[string]bool {
			out := map[string]bool{}
			for _, x := range ts {
				out[x.ID] = true
			}
			return out
		}
		none := ids(l.For(principal()))
		dev := ids(l.For(principal(authz.Developer)))
		admin := ids(l.For(principal(authz.Administrator)))
		if !none["getting-started"] || none["new-image"] || none["secrets-admin"] {
			t.Errorf("a user without roles sees %v", none)
		}
		if !dev["new-image"] || dev["secrets-admin"] || dev["workers"] {
			t.Errorf("a developer sees %v", dev)
		}
		if !admin["secrets-admin"] || admin["new-image"] {
			t.Errorf("an administrator sees %v", admin)
		}
		if _, ok := l.Get(principal(authz.Developer), "secrets-admin"); ok {
			t.Error("a developer read an administrator guide")
		}
		if _, ok := l.Get(principal(authz.Developer, authz.Operator), "workers"); !ok {
			t.Error("roles do not add up")
		}

		link := regexp.MustCompile(`\(help:([a-z0-9-]+)\)`)
		for _, x := range l.topics {
			for _, m := range link.FindAllStringSubmatch(x.Body, -1) {
				if _, ok := l.byID[m[1]]; !ok {
					t.Errorf("%s links to missing guide %s", x.ID, m[1])
				}
			}
		}

		for name, file := range map[string]string{
			"no roles":     "---\ntitle: T\nsection: S\n---\nbody",
			"unknown role": "---\ntitle: T\nsection: S\nroles: [root]\n---\nbody",
			"no title":     "---\nsection: S\nroles: [everyone]\n---\nbody",
		} {
			if _, err := load(fstest.MapFS{"c/x.md": {Data: []byte(file)}}, "c"); err == nil || !strings.Contains(err.Error(), "x.md") {
				t.Errorf("%s: loaded (%v)", name, err)
			}
		}
	})
}
