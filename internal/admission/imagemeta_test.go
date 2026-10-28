package admission

import (
	"fmt"
	"slices"
	"testing"

	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/policy"
)

// TestImageMetadataWarnings: admission says which spec labels and annotations a method does not apply instead of
// dropping them silently, and warns about secret-looking entries, including ones the build-args policy names.
func TestImageMetadataWarnings(t *testing.T) {
	codes := func(method string, labels, annotations map[string]string, eff policy.Effective) []string {
		d := &buildspec.Document{Spec: buildspec.Spec{Build: buildspec.Build{Method: method},
			Outputs: &buildspec.Outputs{Image: &buildspec.ImageOutput{Labels: labels, Annotations: annotations}}}}
		var out []string
		imageMetadataWarnings(d, eff, func(code, format string, args ...any) { out = append(out, code+": "+fmt.Sprintf(format, args...)) })
		return out
	}
	has := func(got []string, prefix string) bool {
		return slices.ContainsFunc(got, func(s string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix })
	}
	plain := map[string]string{"com.example.team": "payments"}

	if got := codes("dockerfile", plain, plain, policy.Effective{}); len(got) != 0 {
		t.Errorf("dockerfile: %v", got)
	}
	if got := codes("railpack", plain, plain, policy.Effective{}); !has(got, "image-labels-not-applied") || has(got, "image-annotations-not-applied") {
		t.Errorf("railpack: %v", got)
	}
	if got := codes("buildpacks", plain, plain, policy.Effective{}); !has(got, "image-labels-not-applied") || !has(got, "image-annotations-not-applied") {
		t.Errorf("buildpacks: %v", got)
	}
	if got := codes("buildpacks", nil, nil, policy.Effective{}); len(got) != 0 {
		t.Errorf("buildpacks without metadata: %v", got)
	}
	if got := codes("recipe", map[string]string{"com.example.api-token": "x"}, nil, policy.Effective{}); !has(got, `image-metadata-secret-like: Label "com.example.api-token"`) {
		t.Errorf("secret-looking label key: %v", got)
	}
	eff := policy.Effective{BuildArgs: policy.BuildArgs{Rules: []policy.ArgRule{{ID: "internal-host", ValuePattern: `corp\.internal`, Reason: "internal hosts"}}}}
	if got := codes("dockerfile", nil, map[string]string{"com.example.endpoint": "db.corp.internal"}, eff); !has(got, `image-metadata-secret-like: Annotation "com.example.endpoint"`) {
		t.Errorf("build-args policy match: %v", got)
	}
}
