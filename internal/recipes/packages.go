package recipes

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
)

const (
	ManagerAPK = "apk"
	ManagerApt = "apt"
)

var (
	distroManager = map[string]string{"alpine": ManagerAPK, "debian": ManagerApt, "ubuntu": ManagerApt}
	distroTokens  = map[string]string{
		"alpine": "alpine",
		"debian": "debian", "bookworm": "debian", "bullseye": "debian", "buster": "debian", "trixie": "debian",
		"ubuntu": "ubuntu", "focal": "ubuntu", "jammy": "ubuntu", "noble": "ubuntu",
	}
	tokenSep = regexp.MustCompile(`[^a-z0-9]+`)
)

// Distro reads an image's distribution from its repository name and tag, falling back to the runtime's declared
// one; "" means unknown.
func Distro(image, declared string) string {
	ref, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	found := ""
	for _, t := range tokenSep.Split(strings.ToLower(ref), -1) {
		if d, ok := distroTokens[t]; ok {
			if found != "" && found != d {
				return ""
			}
			found = d
		}
	}
	if found != "" {
		return found
	}
	if _, ok := distroManager[declared]; ok {
		return declared
	}
	return ""
}

// PackageProblem is a system package request a runtime's images cannot serve; Path is the BuildSpec field.
type PackageProblem struct{ Path, Message string }

// SystemPackages pairs the BuildSpec's OS packages with each image's package manager and returns the egress presets
// of the distributions they come from (docs/architecture/buildspec.md#system-packages).
func (rt Runtime) SystemPackages(appType string, sp *buildspec.SystemPackages) (*buildplan.PlanSystemPackages, []string, []PackageProblem) {
	if sp.IsZero() {
		return nil, nil, nil
	}
	images := rt.Images(appType)
	out := &buildplan.PlanSystemPackages{}
	var presets []string
	var problems []PackageProblem
	for _, side := range []struct {
		image, field string
		list         []string
		manager      *string
		target       *[]string
	}{
		{field: "build", list: sp.Build, image: images[0], manager: &out.BuildManager, target: &out.Build},
		{field: "runtime", list: sp.Runtime, image: images[len(images)-1], manager: &out.RuntimeManager, target: &out.Runtime},
	} {
		if len(side.list) == 0 {
			continue
		}
		d := Distro(side.image, rt.Distro)
		if d == "" {
			problems = append(problems, PackageProblem{"/spec/build/recipe/systemPackages/" + side.field,
				fmt.Sprintf("Samuori cannot tell which package manager %s uses, so it cannot install system packages there; it knows apk (Alpine) and apt-get (Debian, Ubuntu)", side.image)})
			continue
		}
		*side.manager, *side.target = distroManager[d], slices.Clone(side.list)
		if !slices.Contains(presets, d) {
			presets = append(presets, d)
		}
	}
	if len(problems) > 0 {
		return nil, nil, problems
	}
	return out, presets, nil
}
