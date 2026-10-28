// Package catalog holds the built-in recipes: what the controller offers (runtimes, defaults) and the declarative
// stages the worker renders with the same renderer as custom recipes (ADR 0047).
package catalog

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"sync"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/recipedef"
)

//go:embed *.yml
var files embed.FS

type Runtime struct {
	Version    string            `json:"version" yaml:"version"`
	BuildImage string            `json:"buildImage" yaml:"buildImage"`
	RunImage   string            `json:"runImage" yaml:"runImage"`
	RunImages  map[string]string `json:"runImages,omitempty" yaml:"runImages,omitempty"`
	// Distro is the distribution of images whose tag does not name one (alpine, debian or ubuntu); system packages
	// need it to choose a package manager.
	Distro string `json:"distro,omitempty" yaml:"distro,omitempty"`
}

// Images returns the base images used for an application type at a runtime.
func (rt Runtime) Images(appType string) []string {
	run := rt.RunImage
	if img, ok := rt.RunImages[appType]; ok {
		run = img
	}
	if run == rt.BuildImage {
		return []string{run}
	}
	return []string{rt.BuildImage, run}
}

type Defaults struct {
	Install string `json:"install,omitempty" yaml:"install,omitempty"`
	Build   string `json:"build,omitempty" yaml:"build,omitempty"`
}

// Entry is one built-in recipe version.
type Entry struct {
	recipedef.Definition `yaml:",inline"`
	Runtimes             []Runtime `yaml:"runtimes"`
	Defaults             Defaults  `yaml:"defaults,omitempty"`
}

// Load reads and validates every built-in recipe. Unknown fields are refused.
func Load() ([]Entry, error) {
	var out []Entry
	err := fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		var e Entry
		if err := dec.Decode(&e); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if p != e.ID+"-"+e.Version+".yml" {
			return fmt.Errorf("%s: the file name must be %s-%s.yml", p, e.ID, e.Version)
		}
		if err := e.ValidateBuiltin(); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

type index struct {
	byKey map[string]recipedef.Definition
	ids   map[string]bool
}

var loaded = sync.OnceValues(func() (index, error) {
	entries, err := Load()
	ix := index{byKey: map[string]recipedef.Definition{}, ids: map[string]bool{}}
	for _, e := range entries {
		ix.byKey[e.ID+"@"+e.Version] = e.Definition
		ix.ids[e.ID] = true
	}
	return ix, err
})

// Definition returns the stages of a built-in recipe version; known reports whether any version of id exists.
func Definition(id, version string) (d recipedef.Definition, ok, known bool, err error) {
	ix, err := loaded()
	if err != nil {
		return recipedef.Definition{}, false, false, err
	}
	d, ok = ix.byKey[id+"@"+version]
	return d, ok, ix.ids[id], nil
}
