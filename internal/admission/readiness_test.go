package admission

import (
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/discovery"
	"samuori.invalid/samuori/internal/egress"
)

func sharpInput(sharp string) discovery.Input {
	lock := `{"lockfileVersion":3,"packages":{"":{},"node_modules/express":{"version":"4.21.2"},"node_modules/sharp":{"version":"` + sharp + `","hasInstallScript":true}}}`
	return discovery.Input{Files: []discovery.InputFile{
		{Path: "Dockerfile", Content: []byte("FROM node:22-alpine\nWORKDIR /app\nCOPY . .\nRUN npm ci\nRUN curl -fsSL https://example.org/x -o /tmp/x\nCMD [\"node\", \"server.js\"]\n")},
		{Path: "package.json", Content: []byte(`{"name":"shop","dependencies":{"express":"^4","sharp":"^` + sharp + `"}}`)},
		{Path: "package-lock.json", Content: []byte(lock)},
		{Path: "server.js"},
	}}
}

func item(rs []discovery.Readiness, code string) *discovery.Readiness {
	for i := range rs {
		if rs[i].Code == code {
			return &rs[i]
		}
	}
	return nil
}

// TestPredictedEgressUnderPolicy: under the default policy, Dockerfile builds get only the OS package archives, so
// npm ci, sharp's libvips download and a literal curl URL are each predicted with the administrator advice the
// build page would give after a denial; sharp 0.33 needs only npm (ADR 0050).
func TestPredictedEgressUnderPolicy(t *testing.T) {
	cat, err := egress.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p := cat.DefaultPolicy()
	df := discovery.Analyse(sharpInput("0.32.6")).Dockerfiles[0]
	rs := predictEgress(cat, df.NetworkNeeds, "dockerfile", p.DockerfilePresets, p, nil)

	npm := item(rs, "egress-package-manager")
	if npm == nil || npm.Severity != discovery.WillFail || npm.Fixer != discovery.Administrator || npm.Line != 4 ||
		!strings.Contains(npm.Message, "needs the npm preset (registry.npmjs.org:443), which Dockerfile builds may not use") ||
		!strings.Contains(npm.Fix, "set npm to reach Dockerfile builds") {
		t.Errorf("npm = %+v", npm)
	}
	sharp := item(rs, "egress-downloader")
	if sharp == nil || sharp.Fixer != discovery.Developer || !strings.Contains(sharp.Message, "sharp 0.32.6 downloads a prebuilt libvips") ||
		!strings.Contains(sharp.Message, "github.com:443") || !strings.Contains(sharp.Fix, "0.33") ||
		!strings.Contains(sharp.Alternative, "github.com:443, objects.githubusercontent.com:443 and release-assets.githubusercontent.com:443 are in the github-releases preset") ||
		!strings.Contains(sharp.Alternative, "set github-releases to reach Dockerfile builds") ||
		strings.Contains(sharp.Alternative, "mise") {
		t.Errorf("sharp = %+v", sharp)
	}
	url := item(rs, "egress-url")
	if url == nil || !strings.Contains(url.Message, "example.org:443") || !strings.Contains(url.Fix, "in no egress preset") || url.Blocking {
		t.Errorf("curl = %+v", url)
	}

	p.DockerfilePresets = append(p.DockerfilePresets, "npm")
	p.Destinations = append(p.Destinations, egress.Destination{Name: "example", Host: "example.org", Port: 443, Reason: "test"})
	rs = predictEgress(cat, df.NetworkNeeds, "dockerfile", p.DockerfilePresets, p, nil)
	if item(rs, "egress-package-manager") != nil || item(rs, "egress-url") != nil || item(rs, "egress-downloader") == nil {
		t.Errorf("with npm and example.org allowed, only sharp should remain: %+v", rs)
	}

	fixed := discovery.Analyse(sharpInput("0.33.5")).Dockerfiles[0]
	if rs := predictEgress(cat, fixed.NetworkNeeds, "dockerfile", p.DockerfilePresets, p, nil); len(rs) != 0 {
		t.Errorf("sharp 0.33.5 with npm allowed: %+v", rs)
	}
}

// TestDiscoveryReadinessForApplications: an application without a Dockerfile is compared with everything the policy
// allows, so hosts in no allowed preset or destination are reported whatever build method is chosen later.
func TestDiscoveryReadinessForApplications(t *testing.T) {
	cat, _ := egress.Builtin()
	in := sharpInput("0.32.6")
	in.Files = in.Files[1:]
	res := discovery.Analyse(in)
	if err := DiscoveryReadiness(&res, cat.DefaultPolicy(), nil); err != nil {
		t.Fatal(err)
	}
	it := item(res.Findings[0].Readiness, "egress-downloader")
	if it == nil || !strings.Contains(it.Message, "not allowed for any build") || !strings.Contains(it.Message, "objects.githubusercontent.com:443") ||
		!strings.Contains(it.Message, "github.com:443") || !strings.Contains(it.Alternative, "github-releases preset, which reaches only builds that need it") {
		t.Errorf("finding readiness = %+v", res.Findings[0].Readiness)
	}
	p := cat.DefaultPolicy()
	p.Destinations = []egress.Destination{
		{Name: "github", Host: "github.com", Port: 443, Network: egress.NetworkPublic},
		{Name: "github-objects", Host: "objects.githubusercontent.com", Port: 443, Network: egress.NetworkPublic},
		{Name: "github-assets", Host: "release-assets.githubusercontent.com", Port: 443, Network: egress.NetworkPublic},
	}
	again := discovery.Analyse(in)
	if err := DiscoveryReadiness(&again, p, nil); err != nil {
		t.Fatal(err)
	}
	if it := item(again.Findings[0].Readiness, "egress-downloader"); it != nil {
		t.Errorf("a public policy destination does not cover sharp: %+v", it)
	}
	every := cat.DefaultPolicy()
	every.EveryBuildPresets = []string{"github-releases"}
	third := discovery.Analyse(in)
	if err := DiscoveryReadiness(&third, every, nil); err != nil {
		t.Fatal(err)
	}
	if it := item(third.Findings[0].Readiness, "egress-downloader"); it != nil {
		t.Errorf("GitHub releases at All builds does not cover sharp: %+v", it)
	}
}
