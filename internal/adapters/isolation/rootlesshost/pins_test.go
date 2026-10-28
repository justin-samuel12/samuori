package rootlesshost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/adapters/build/buildah"
	"samuori.invalid/samuori/internal/adapters/build/buildkit"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/engine"
)

// TestPinContexts: each pin becomes the docker-image named context BuildKit's Dockerfile frontend uses in place of
// the FROM reference; Buildah refuses pins rather than building the tag (ADR 0049).
func TestPinContexts(t *testing.T) {
	image := "docker.io/library/node@sha256:" + strings.Repeat("e", 64)
	contexts, err := pinContexts([]buildplan.BaseImagePin{{Ref: "docker.io/library/node:18-alpine", Image: image}, {Ref: "node", Image: image}})
	if err != nil {
		t.Fatal(err)
	}
	attrs := buildkit.Attrs(buildkit.Request{Filename: "Dockerfile", ImageContexts: contexts})
	if attrs["context:node:18-alpine"] != "docker-image://"+image || attrs["context:node"] != "docker-image://"+image || len(attrs) != 3 {
		t.Errorf("frontend attributes = %v", attrs)
	}
	if _, err := pinContexts([]buildplan.BaseImagePin{{Ref: "node:18", Image: "docker.io/library/node:18"}}); err == nil {
		t.Error("a pin without a digest was accepted")
	}
	r := &buildah.Runner{}
	if _, err := r.Build(context.Background(), engine.Request{ImageContexts: contexts}); !errors.Is(err, buildah.ErrPins) {
		t.Errorf("Buildah with pins = %v", err)
	}
}
