// Package providers holds the administrator's approval of optional third-party build and conversion tools and the
// build method policy (ADR 0047, ADR 0048). A provider runs only when enabled with a pinned version.
package providers

import (
	"fmt"
	"regexp"
	"samuori.invalid/samuori/internal/buildplan"
	"slices"
	"sort"
)

type Kind string

const (
	Build      Kind = "build"
	Conversion Kind = "conversion"
)

// Methods are the BuildSpec build methods; recipe and dockerfile need no provider.
const (
	MethodRecipe     = "recipe"
	MethodDockerfile = "dockerfile"
	MethodRailpack   = "railpack"
	MethodBuildpacks = "buildpacks"
)

var Methods = []string{MethodRecipe, MethodDockerfile, MethodRailpack, MethodBuildpacks}

// Artifact is one pinned input an administrator approves: a file checksum or an image digest.
type Artifact struct {
	Key         string `json:"key"`
	Description string `json:"description"`
	Image       bool   `json:"image"`
}

// Definition describes a provider Samuori knows how to run.
type Definition struct {
	Name          string     `json:"name"`
	Kind          Kind       `json:"kind"`
	DisplayName   string     `json:"displayName"`
	Method        string     `json:"method,omitempty"`
	License       string     `json:"license"`
	Architectures []string   `json:"architectures"`
	Prerequisites []string   `json:"prerequisites"`
	Artifacts     []Artifact `json:"artifacts"`
	Maintenance   string     `json:"maintenance,omitempty"`
	// WorkerVersion is what a capable worker reports when nothing is installed for the provider; empty means
	// workers report the installed, approved version.
	WorkerVersion string `json:"workerVersion,omitempty"`
}

// CNBPlatformAPI is the Cloud Native Buildpacks platform API Samuori drives the lifecycle with (buildplan).
const CNBPlatformAPI = buildplan.CNBPlatformAPI

var Catalog = []Definition{
	{Name: "railpack", Kind: Build, DisplayName: "Railpack", Method: MethodRailpack, License: "MIT", Architectures: []string{"amd64", "arm64"},
		Prerequisites: []string{"rootless BuildKit on the worker", "egress to the sources mise downloads runtimes from, or a staged mise cache"},
		Artifacts: []Artifact{
			{Key: "binary-amd64", Description: "SHA-256 of the railpack binary for linux/amd64"},
			{Key: "binary-arm64", Description: "SHA-256 of the railpack binary for linux/arm64"},
			{Key: "frontend", Description: "ghcr.io/railwayapp/railpack-frontend pinned by digest, of the same version", Image: true},
			{Key: "builder", Description: "ghcr.io/railwayapp/railpack-builder with the tag this version plans (mise-<version>), pinned by digest", Image: true},
			{Key: "runtime", Description: "ghcr.io/railwayapp/railpack-runtime with the tag this version plans (mise-<version>), pinned by digest", Image: true},
		}},
	{Name: "buildpacks", Kind: Build, DisplayName: "Paketo Buildpacks (CNB lifecycle)", Method: MethodBuildpacks, License: "Apache-2.0", Architectures: []string{"amd64", "arm64"},
		Prerequisites: []string{"rootless BuildKit on the worker; nothing else is installed", "a builder and run image for the worker's architecture whose lifecycle supports platform API " + CNBPlatformAPI, "egress to the buildpacks' dependency sources"},
		Artifacts: []Artifact{
			{Key: "builder", Description: "CNB builder image pinned by digest, such as docker.io/paketobuildpacks/ubuntu-noble-builder", Image: true},
			{Key: "run-image", Description: "the run image the builder names (its io.buildpacks.builder.metadata), pinned by digest; it becomes the image's base", Image: true},
		},
		WorkerVersion: CNBPlatformAPI},
	{Name: "kompose", Kind: Conversion, DisplayName: "Kompose", License: "Apache-2.0", Architectures: []string{"amd64", "arm64"},
		Prerequisites: []string{"the kompose binary installed on the worker"},
		Artifacts: []Artifact{
			{Key: "binary-amd64", Description: "SHA-256 of the kompose binary for linux/amd64"},
			{Key: "binary-arm64", Description: "SHA-256 of the kompose binary for linux/arm64"},
		}},
	{Name: "move2kube", Kind: Conversion, DisplayName: "Move2Kube", License: "Apache-2.0", Architectures: []string{"amd64", "arm64"},
		Prerequisites: []string{"the move2kube binary installed on the worker"},
		Artifacts: []Artifact{
			{Key: "binary-amd64", Description: "SHA-256 of the move2kube binary for linux/amd64"},
			{Key: "binary-arm64", Description: "SHA-256 of the move2kube binary for linux/arm64"},
		},
		Maintenance: "No release or commit since March 2025 (v0.3.15, checked 2026-10-24); enable only after reviewing that risk."},
}

func Lookup(name string) (Definition, bool) {
	i := slices.IndexFunc(Catalog, func(d Definition) bool { return d.Name == name })
	if i < 0 {
		return Definition{}, false
	}
	return Catalog[i], true
}

// ForMethod returns the provider a build method needs, if any.
func ForMethod(method string) (Definition, bool) {
	i := slices.IndexFunc(Catalog, func(d Definition) bool { return d.Method == method && method != "" })
	if i < 0 {
		return Definition{}, false
	}
	return Catalog[i], true
}

// Config is an administrator's approval. Empty Pools or Projects mean every pool or project.
type Config struct {
	Enabled        bool              `json:"enabled"`
	Version        string            `json:"version"`
	Artifacts      map[string]string `json:"artifacts"`
	Pools          []string          `json:"pools"`
	Projects       []string          `json:"projects"`
	EgressPresets  []string          `json:"egressPresets"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	// RunAsRoot keeps Railpack's root image user instead of the non-root user layer Samuori adds (Railpack only).
	RunAsRoot bool `json:"runAsRoot,omitempty"`
}

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

var (
	versionRe = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)
	sha256Re  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	imageRe   = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+(:[A-Za-z0-9._-]+)?@sha256:[a-f0-9]{64}$`)
)

// Validate checks c against d. An enabled provider needs a version and every artifact pinned.
func (c *Config) Validate(d Definition) error {
	if c.Artifacts == nil {
		c.Artifacts = map[string]string{}
	}
	for k := range c.Artifacts {
		if !slices.ContainsFunc(d.Artifacts, func(a Artifact) bool { return a.Key == k }) {
			return &Invalid{fmt.Sprintf("%s has no artifact %q", d.Name, k)}
		}
	}
	for _, a := range d.Artifacts {
		v := c.Artifacts[a.Key]
		switch {
		case v == "" && c.Enabled:
			return &Invalid{fmt.Sprintf("enabling %s needs %s", d.DisplayName, a.Description)}
		case v == "":
		case a.Image && !imageRe.MatchString(v):
			return &Invalid{fmt.Sprintf("%s: %q must be an image reference pinned by digest (…@sha256:<64 hex>)", a.Key, v)}
		case !a.Image && !sha256Re.MatchString(v):
			return &Invalid{fmt.Sprintf("%s: %q must be sha256:<64 hex>", a.Key, v)}
		}
	}
	if c.Version != "" && !versionRe.MatchString(c.Version) {
		return &Invalid{fmt.Sprintf("%q is not a release version such as v0.43.0", c.Version)}
	}
	if c.Enabled && c.Version == "" {
		return &Invalid{fmt.Sprintf("enabling %s needs an approved version", d.DisplayName)}
	}
	if c.RunAsRoot && d.Method != MethodRailpack {
		return &Invalid{"only Railpack images can be kept running as root"}
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 7200 {
		return &Invalid{"the time limit is 0 (the default) to 7200 seconds"}
	}
	for _, list := range [][]string{c.Pools, c.Projects, c.EgressPresets} {
		sort.Strings(list)
	}
	c.Pools, c.Projects, c.EgressPresets = compact(c.Pools), compact(c.Projects), compact(c.EgressPresets)
	return nil
}

// Allows reports whether the provider may run for project in pool. An empty pool skips the pool check, for admission
// before a pool is chosen; dispatch checks it again with the pool.
func (c Config) Allows(pool, project string) bool {
	return c.Enabled && (pool == "" || len(c.Pools) == 0 || slices.Contains(c.Pools, pool)) && (len(c.Projects) == 0 || slices.Contains(c.Projects, project))
}

// Policy is the organization's build method policy. An empty Default means Samuori recommends a method from discovery.
type Policy struct {
	Allowed []string `json:"allowed"`
	Default string   `json:"default"`
}

func DefaultPolicy() Policy { return Policy{Allowed: []string{MethodRecipe, MethodDockerfile}} }

func (p *Policy) Validate() error {
	if len(p.Allowed) == 0 {
		return &Invalid{"allow at least one build method"}
	}
	for _, m := range p.Allowed {
		if !slices.Contains(Methods, m) {
			return &Invalid{fmt.Sprintf("%q is not a build method", m)}
		}
	}
	if p.Default != "" && !slices.Contains(p.Allowed, p.Default) {
		return &Invalid{"the default method must be allowed"}
	}
	p.Allowed = compact(slices.Sorted(slices.Values(p.Allowed)))
	return nil
}

func compact(s []string) []string {
	out := []string{}
	for _, v := range s {
		if v != "" && (len(out) == 0 || out[len(out)-1] != v) {
			out = append(out, v)
		}
	}
	return out
}
