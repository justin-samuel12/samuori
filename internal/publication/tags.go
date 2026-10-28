package publication

import (
	"context"
	"encoding/json"

	"samuori.invalid/samuori/internal/buildspec"
)

// DefaultTags are the tags a publication of a build's image applies when the publisher gives none: the spec's
// outputs.image.tags with {version} resolved, or the build's version (ADR 0054).
func (s *Service) DefaultTags(ctx context.Context, runID string) ([]string, error) {
	var version, doc string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COALESCE(r.version, ''), sr.document FROM build_runs r JOIN spec_revisions sr ON sr.spec_id = r.spec_id AND sr.revision = r.spec_revision WHERE r.id = ?`), runID).Scan(&version, &doc)
	if err != nil {
		return nil, err
	}
	var d buildspec.Document
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		return nil, err
	}
	return d.DefaultTags(version), nil
}
