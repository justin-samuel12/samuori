package discovery

import "testing"

func TestNodeLanguage(t *testing.T) {
	pkg := `{"name":"app","main":"src/server.js","scripts":{"start":"node src/server.js"},"dependencies":{"express":"4"},"devDependencies":{"typescript":"5","@types/node":"24"}}`
	for _, c := range []struct {
		name  string
		files []InputFile
		want  string
	}{
		{"typescript as a tool", []InputFile{{Path: "package.json", Content: []byte(pkg)}}, "javascript"},
		{"tsconfig", []InputFile{{Path: "package.json", Content: []byte(pkg)}, {Path: "tsconfig.json", Content: []byte("{}")}}, "typescript"},
	} {
		r := Analyse(Input{Files: c.files})
		if len(r.Findings) != 1 || r.Findings[0].Language != c.want {
			t.Errorf("%s: findings = %+v; want language %s", c.name, r.Findings, c.want)
		}
	}
}
