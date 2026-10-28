package buildspec

import (
	"fmt"
	"strings"
)

// VersionTag stands for the build's version (<UTC date>.<n>) in outputs.image.tags (ADR 0054).
const VersionTag = "{version}"

// DefaultTags are the tags a publication of this spec's image applies when the publisher gives none: the spec's
// outputs.image.tags, or the version alone. {version} is dropped when the build has no version.
func (d *Document) DefaultTags(version string) []string {
	tags := []string{VersionTag}
	if out := d.Spec.Outputs; out != nil && out.Image != nil && len(out.Image.Tags) > 0 {
		tags = out.Image.Tags
	}
	resolved := []string{}
	for _, t := range tags {
		if t == VersionTag {
			if version == "" {
				continue
			}
			t = version
		}
		resolved = append(resolved, t)
	}
	return resolved
}

func imageTagRules(d *Document) []Issue {
	out := d.Spec.Outputs
	if out == nil || out.Image == nil {
		return nil
	}
	var issues []Issue
	seen := map[string]bool{}
	for i, t := range out.Image.Tags {
		path := fmt.Sprintf("/spec/outputs/image/tags/%d", i)
		switch {
		case strings.HasPrefix(t, "samuori-"):
			issues = append(issues, Issue{Rule: "image-tag", Path: path, Message: "tags starting with samuori- are reserved for build reference tags"})
		case seen[t]:
			issues = append(issues, Issue{Rule: "image-tag", Path: path, Message: "this tag is listed twice"})
		}
		seen[t] = true
	}
	return issues
}
