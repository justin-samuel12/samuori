package rootlesshost

import (
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/adapters/build/buildkit"
)

// TestConversionStepHasNoNetworkOrCredentials (SEC-K8S-02): the conversion step pulls nothing, runs with no network,
// mounts the tool and input read-only, and its solve carries only local contexts: no proxy, build arguments or
// registry contexts.
func TestConversionStepHasNoNetworkOrCredentials(t *testing.T) {
	df := ConversionDockerfile()
	lines := strings.Split(strings.TrimSpace(df), "\n")
	if lines[0] != "FROM scratch AS samuori-convert" || lines[len(lines)-2] != "FROM scratch" {
		t.Errorf("the step must start from scratch:\n%s", df)
	}
	var runs int
	for _, l := range lines {
		if strings.HasPrefix(l, "RUN ") {
			runs++
			if !strings.HasPrefix(l, "RUN --network=none ") || strings.Contains(l, "type=secret") || strings.Contains(l, "type=ssh") || strings.Contains(l, ",rw") {
				t.Errorf("unsafe step: %s", l)
			}
		}
		if strings.HasPrefix(l, "ARG ") || strings.HasPrefix(l, "ENV ") {
			t.Errorf("the step takes no arguments or environment: %s", l)
		}
	}
	if runs != 1 {
		t.Errorf("%d RUN steps", runs)
	}
	attrs := buildkit.ToolAttrs(buildkit.ToolRequest{Contexts: map[string]string{"samuoritool": "/t", "samuoriinput": "/i"}, Platform: "linux/arm64"})
	for k, v := range attrs {
		if strings.HasPrefix(k, "build-arg:") || (strings.HasPrefix(k, "context:") && !strings.HasPrefix(v, "local:")) {
			t.Errorf("unexpected frontend attribute %s=%s", k, v)
		}
	}
	if attrs["context:samuoriinput"] != "local:samuoriinput" || attrs["context:samuoritool"] != "local:samuoritool" {
		t.Errorf("attrs = %v", attrs)
	}
}
