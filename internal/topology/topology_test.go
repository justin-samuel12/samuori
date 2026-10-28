package topology

import (
	"strings"
	"testing"
)

func TestAssess(t *testing.T) {
	ha := Facts{Controllers: 3, Pools: map[string]int{"default": 2}, Database: PostgreSQL, DatabaseReplicated: true, Storage: S3}
	cases := []struct {
		name string
		f    Facts
		tier Tier
		gaps []string
	}{
		{"single server", Facts{Controllers: 1, Pools: map[string]int{"default": 1}, Database: SQLite, Storage: Local, PortalOnController: true}, Standalone,
			[]string{"controllers", "workers", "database", "storage", "entry point"}},
		{"three controllers, replicated database, S3, two workers, load balancer", ha, HA, nil},
		{"one controller with two workers", Facts{Controllers: 1, Pools: map[string]int{"default": 2}, Database: PostgreSQL, Storage: S3, PortalOnController: true}, PartialHA,
			[]string{"controllers", "database", "entry point"}},
		{"two controllers tolerate nothing", func() Facts { f := ha; f.Controllers = 2; return f }(), PartialHA, []string{"controllers"}},
		{"a pool with one worker", func() Facts { f := ha; f.Pools = map[string]int{"default": 2, "gpu": 1}; return f }(), PartialHA, []string{"workers"}},
		{"undeclared OCI storage", func() Facts { f := ha; f.Storage = OCI; return f }(), PartialHA, []string{"storage"}},
	}
	for _, c := range cases {
		a := Assess(c.f)
		if a.Tier != c.tier {
			t.Errorf("%s: tier %s, want %s", c.name, a.Tier, c.tier)
		}
		var gaps []string
		for _, comp := range a.Components {
			if comp.Tolerates == 0 {
				gaps = append(gaps, comp.Name)
				if comp.Gap == "" {
					t.Errorf("%s: %s tolerates nothing but names no gap", c.name, comp.Name)
				}
			}
		}
		if strings.Join(gaps, ",") != strings.Join(c.gaps, ",") {
			t.Errorf("%s: gaps %v, want %v", c.name, gaps, c.gaps)
		}
	}
	if w := Assess(Facts{Controllers: 4}).Warnings; len(w) != 1 || !strings.Contains(w[0], "no more failures than 3") {
		t.Errorf("four controllers: warnings %v", w)
	}
	if a := Assess(ha); a.Components[2].Declared != true {
		t.Error("a replicated database is declared, not measured")
	}
}
