package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"samuori.invalid/samuori/internal/buildspec"
)

type specWarning struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// tagWarnings names the spec's fixed default tags that the project has already published, so moving or reusing them
// is a choice, not a surprise (ADR 0054). It only warns: the publish-time check against the registry decides.
func (a *API) tagWarnings(ctx context.Context, projectID string, document json.RawMessage) []specWarning {
	var d buildspec.Document
	if err := json.Unmarshal(document, &d); err != nil || d.Spec.Outputs == nil || d.Spec.Outputs.Image == nil {
		return nil
	}
	rows, err := a.db.SQL.QueryContext(ctx, a.db.Rebind(`SELECT p.tags, d.name, COALESCE(r.version, '') FROM publications p JOIN destinations d ON d.id = p.destination_id
		JOIN build_artifacts ba ON ba.id = p.artifact_id JOIN build_runs r ON r.id = ba.run_id
		WHERE p.project_id = ? AND p.state = 'published' ORDER BY p.created_at DESC LIMIT 500`), projectID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type use struct{ destination, version string }
	used := map[string]use{}
	for rows.Next() {
		var raw, dest, version string
		if rows.Scan(&raw, &dest, &version) != nil {
			continue
		}
		var tags []string
		_ = json.Unmarshal([]byte(raw), &tags)
		for _, t := range tags {
			if _, seen := used[t]; !seen {
				used[t] = use{dest, version}
			}
		}
	}
	var out []specWarning
	for i, t := range d.Spec.Outputs.Image.Tags {
		u, ok := used[t]
		if t == buildspec.VersionTag || !ok {
			continue
		}
		of := ""
		if u.version != "" {
			of = " (version " + u.version + ")"
		}
		out = append(out, specWarning{Path: fmt.Sprintf("/spec/outputs/image/tags/%d", i),
			Message: fmt.Sprintf("%s already names an image%s published to %s; publishing again moves the tag, or is refused if the destination protects it or keeps tags immutable", t, of, u.destination)})
	}
	return slices.Clip(out)
}
