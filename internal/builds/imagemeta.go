package builds

import (
	"regexp"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
)

const (
	ociCreated  = "org.opencontainers.image.created"
	ociRevision = "org.opencontainers.image.revision"
	ociTitle    = "org.opencontainers.image.title"
)

var gitCommit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// imageMetadata merges the spec's labels and annotations over the automatic OCI values, keeping only what the
// method applies. created is the BuildRun's admission time; revision is set only for a snapshot fetched from a
// known Git commit (docs/architecture/buildspec.md#image-labels-and-annotations).
func imageMetadata(doc *buildspec.Document, method string, created time.Time, revision string) *buildplan.PlanImage {
	auto := map[string]string{ociCreated: created.UTC().Format(time.RFC3339)}
	if gitCommit.MatchString(revision) {
		auto[ociRevision] = revision
	}
	if doc.Metadata.Name != "" {
		auto[ociTitle] = doc.Metadata.Name
	}
	labels, annotations := doc.ImageMetadata()
	applyLabels, applyAnnotations := buildspec.ImageMetadataApplied(method)
	img := &buildplan.PlanImage{}
	if applyLabels {
		img.Labels = merged(auto, labels)
	}
	if applyAnnotations {
		img.Annotations = merged(auto, annotations)
	}
	if img.Labels == nil && img.Annotations == nil {
		return nil
	}
	return img
}

// merged lets spec values win over automatic ones; validation keeps the spec out of the reserved io.samuori. prefix.
func merged(auto, spec map[string]string) map[string]string {
	out := make(map[string]string, len(auto)+len(spec))
	for k, v := range auto {
		out[k] = v
	}
	for k, v := range spec {
		out[k] = v
	}
	return out
}
