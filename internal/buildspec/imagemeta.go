package buildspec

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Image label and annotation limits (docs/architecture/buildspec.md#image-labels-and-annotations).
const (
	ReservedMetadataPrefix = "io.samuori."
	MaxMetadataEntries     = 64
	MaxMetadataKeyBytes    = 255
	MaxMetadataValueBytes  = 4096
)

// metadataKey is a reverse-DNS key: a lowercase domain-style head, then dot-separated segments that may use mixed
// case, underscores and slashes, as OCI and common tooling keys do.
var metadataKey = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(\.[a-zA-Z0-9][a-zA-Z0-9._/-]*)*$`)

// ImageMetadataApplied reports which of outputs.image.labels and outputs.image.annotations a build method writes
// onto the image. Railpack's frontend is not known to honour label options, and buildpacks images are exported by
// the lifecycle, so those methods apply less (docs/architecture/build-execution.md#image-labels-and-annotations).
func ImageMetadataApplied(method string) (labels, annotations bool) {
	switch method {
	case "dockerfile", "recipe":
		return true, true
	case "railpack":
		return false, true
	}
	return false, false
}

// ImageMetadata returns the spec's image labels and annotations, never nil.
func (d *Document) ImageMetadata() (labels, annotations map[string]string) {
	labels, annotations = map[string]string{}, map[string]string{}
	if out := d.Spec.Outputs; out != nil && out.Image != nil {
		for k, v := range out.Image.Labels {
			labels[k] = v
		}
		for k, v := range out.Image.Annotations {
			annotations[k] = v
		}
	}
	return labels, annotations
}

func imageMetadataRules(d *Document) []Issue {
	var issues []Issue
	labels, annotations := d.ImageMetadata()
	for _, kind := range []struct {
		field string
		m     map[string]string
	}{{"labels", labels}, {"annotations", annotations}} {
		base := "/spec/outputs/image/" + kind.field
		if len(kind.m) > MaxMetadataEntries {
			issues = append(issues, Issue{Rule: "image-metadata-count", Path: base, Message: fmt.Sprintf("at most %d %s are allowed", MaxMetadataEntries, kind.field)})
		}
		keys := make([]string, 0, len(kind.m))
		for k := range kind.m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			path := base + "/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
			if msg := MetadataKeyProblem(k); msg != "" {
				issues = append(issues, Issue{Rule: "image-metadata-key", Path: path, Message: msg})
			}
			if msg := metadataValueProblem(kind.m[k]); msg != "" {
				issues = append(issues, Issue{Rule: "image-metadata-value", Path: path, Message: msg})
			}
		}
	}
	return issues
}

// MetadataKeyProblem explains why k is not a usable label or annotation key, or returns "".
func MetadataKeyProblem(k string) string {
	switch {
	case len(k) > MaxMetadataKeyBytes:
		return fmt.Sprintf("keys are at most %d bytes", MaxMetadataKeyBytes)
	case strings.HasPrefix(k, ReservedMetadataPrefix) || k == strings.TrimSuffix(ReservedMetadataPrefix, "."):
		return fmt.Sprintf("keys starting with %s are reserved for Samuori", ReservedMetadataPrefix)
	case !metadataKey.MatchString(k):
		return fmt.Sprintf("%q is not a reverse-DNS key such as org.opencontainers.image.source or com.example.team: start with lowercase letters or digits, separate parts with dots", k)
	}
	return ""
}

func metadataValueProblem(v string) string {
	switch {
	case len(v) > MaxMetadataValueBytes:
		return fmt.Sprintf("values are at most %d bytes", MaxMetadataValueBytes)
	case strings.IndexFunc(v, unicode.IsControl) >= 0:
		return "the value contains a control character such as a line break or tab"
	}
	return ""
}
