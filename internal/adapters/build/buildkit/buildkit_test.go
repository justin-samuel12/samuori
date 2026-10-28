package buildkit

import (
	"maps"
	"testing"
)

// TestImageMetadataAttrs: labels reach only the Dockerfile frontend as label:<key>; annotations reach the OCI
// exporter as annotation.<key> for every platform's manifest, plus annotation-index.<key> only for a
// multi-platform build, since BuildKit v0.34.0 refuses index annotations on a single-platform export.
func TestImageMetadataAttrs(t *testing.T) {
	r := Request{Filename: "Dockerfile", Platforms: []string{"linux/amd64"},
		Labels:      map[string]string{"org.opencontainers.image.title": "api"},
		Annotations: map[string]string{"org.opencontainers.image.created": "2026-10-26T00:00:00Z"}}
	if got := Attrs(r)["label:org.opencontainers.image.title"]; got != "api" {
		t.Errorf("label attr = %q", got)
	}
	want := map[string]string{"annotation.org.opencontainers.image.created": "2026-10-26T00:00:00Z"}
	if got := ExportAttrs(r); !maps.Equal(got, want) {
		t.Errorf("single-platform export attrs = %v, want %v", got, want)
	}
	r.Platforms = []string{"linux/amd64", "linux/arm64"}
	want["annotation-index.org.opencontainers.image.created"] = "2026-10-26T00:00:00Z"
	if got := ExportAttrs(r); !maps.Equal(got, want) {
		t.Errorf("multi-platform export attrs = %v, want %v", got, want)
	}
	r.Frontend = "docker.io/railwayapp/railpack-frontend@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for k := range Attrs(r) {
		if len(k) > 6 && k[:6] == "label:" {
			t.Errorf("a gateway frontend received %s", k)
		}
	}
	if len(ExportAttrs(Request{})) != 0 {
		t.Error("a request without annotations has exporter attributes")
	}
}
