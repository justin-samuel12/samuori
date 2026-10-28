package main

import "testing"

func TestPolicy(t *testing.T) {
	reviews := map[string]review{
		"example.com/mixed": {Name: "example.com/mixed", License: "BSD-3-Clause AND MPL-2.0", Packages: []string{"example.com/mixed"}},
		"example.com/docs":  {Name: "example.com/docs", License: "Apache-2.0 AND CC-BY-SA-4.0"},
		"example.com/gpl":   {Name: "example.com/gpl", License: "GPL-2.0"},
	}
	for _, c := range []struct {
		comp Component
		want string
	}{
		{Component{Name: "example.com/a", License: "Apache-2.0"}, "pass"},
		{Component{Name: "example.com/b", License: "MIT OR Apache-2.0"}, "pass"},
		{Component{Name: "example.com/c", License: "NOASSERTION"}, "fail"},
		{Component{Name: "example.com/d", License: ""}, "fail"},
		{Component{Name: "example.com/e", License: "MPL-2.0"}, "fail"},
		{Component{Name: "example.com/gpl", License: "GPL-2.0"}, "fail"},
		{Component{Name: "example.com/f", License: "AGPL-3.0"}, "fail"},
		{Component{Name: "example.com/docs", License: "Apache-2.0 AND CC-BY-SA-4.0"}, "pass"},
		{Component{Name: "example.com/docs", License: "Apache-2.0 AND CC-BY-SA-4.0 AND MPL-2.0"}, "fail"},
		{Component{Name: "example.com/mixed", License: "BSD-3-Clause AND MPL-2.0", Packages: []string{"example.com/mixed"}}, "pass"},
		{Component{Name: "example.com/mixed", License: "BSD-3-Clause AND MPL-2.0", Packages: []string{"example.com/mixed", "example.com/mixed/mpl"}}, "fail"},
	} {
		judge(&c.comp, reviews)
		if c.comp.Verdict != c.want {
			t.Errorf("%s %q %v: %s, want %s", c.comp.Name, c.comp.License, c.comp.Packages, c.comp.Verdict, c.want)
		}
	}
}
