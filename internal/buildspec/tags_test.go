package buildspec

import (
	"slices"
	"testing"
)

// TestDefaultTags: {version} resolves to the build's version, a build without one drops it, and absent tags mean the
// version alone (ADR 0054).
func TestDefaultTags(t *testing.T) {
	with := func(tags ...string) *Document {
		return &Document{Spec: Spec{Outputs: &Outputs{Image: &ImageOutput{Tags: tags}}}}
	}
	for name, c := range map[string]struct {
		d       *Document
		version string
		want    []string
	}{
		"absent":         {&Document{}, "2026-10-27.1", []string{"2026-10-27.1"}},
		"listed":         {with("{version}", "stable", "latest"), "2026-10-27.2", []string{"2026-10-27.2", "stable", "latest"}},
		"no version yet": {with("{version}", "stable"), "", []string{"stable"}},
		"nothing at all": {&Document{}, "", []string{}},
	} {
		if got := c.d.DefaultTags(c.version); !slices.Equal(got, c.want) {
			t.Errorf("%s = %v, want %v", name, got, c.want)
		}
	}
	if issues := imageTagRules(with("samuori-1", "a", "a")); len(issues) != 2 {
		t.Errorf("issues = %v", issues)
	}
}
