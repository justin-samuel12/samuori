package admission

import (
	"sort"

	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/imagescan"
	"samuori.invalid/samuori/internal/policy"
)

// imageMetadataWarnings explains labels and annotations the method does not apply, and flags secret-looking ones:
// anyone who can pull or inspect the image reads them. Values in a known credential format are already semantic
// errors (secret-literal); keys and values matching the build-args policy are warned about here.
func imageMetadataWarnings(d *buildspec.Document, eff policy.Effective, warn func(code, format string, args ...any)) {
	labels, annotations := d.ImageMetadata()
	applyLabels, applyAnnotations := buildspec.ImageMetadataApplied(d.Spec.Build.Method)
	if len(labels) > 0 && !applyLabels {
		warn("image-labels-not-applied", "%s builds do not apply spec.outputs.image.labels; the image will not carry them.", d.Spec.Build.Method)
	}
	if len(annotations) > 0 && !applyAnnotations {
		warn("image-annotations-not-applied", "%s builds do not apply spec.outputs.image.annotations; the image will not carry them.", d.Spec.Build.Method)
	}
	rules := eff.ArgRules()
	for _, m := range []struct {
		what string
		kv   map[string]string
	}{{"Label", labels}, {"Annotation", annotations}} {
		keys := make([]string, 0, len(m.kv))
		for k := range m.kv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := m.kv[k]
			flagged := imagescan.SecretLikeEntry(k, v)
			for _, r := range rules {
				flagged = flagged || (r.NamePattern != nil && r.NamePattern.MatchString(k)) || (r.ValuePattern != nil && r.ValuePattern.MatchString(v))
			}
			if flagged {
				warn("image-metadata-secret-like", "%s %q looks like it holds a secret. Anyone who can pull or inspect the image can read it, and the sensitive-content check flags it. Remove it; pass secrets to the build as build secrets.", m.what, k)
			}
		}
	}
}
