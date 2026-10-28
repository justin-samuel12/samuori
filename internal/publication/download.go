package publication

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/validation"
)

var downloadRoles = []authz.Role{authz.Maintainer, authz.Publisher, authz.Administrator}

// Download is an image a person may take out of Samuori as an OCI archive that docker load also reads.
type Download struct {
	Digest   string
	Size     int64
	Filename string
	// RepoTag is the name docker load gives the image: project/app and the build's version, or the digest.
	RepoTag string
}

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

// UseDownloadPolicy reports whether an organization lets developers download validated images (publication policy).
func (s *Service) UseDownloadPolicy(f func(ctx context.Context, orgID string) (bool, error)) {
	s.developerDownloads = f
}

// Download checks that a built image may leave Samuori without a publication and audits it (ADR 0053): project
// maintainers, publishers and administrators, and developers when the publication policy allows it; and only an image whose newest validation passed, that is not
// restricted and that no compromised worker built. The caller streams the archive.
func (s *Service) Download(ctx context.Context, p authz.Principal, roles []authz.Role, artifactID, requestID string) (Download, error) {
	art, err := s.validations.Artifact(ctx, p, roles, artifactID)
	if err != nil {
		return Download{}, ErrNotFound
	}
	team, err := s.team(ctx, art.ProjectID)
	if err != nil {
		return Download{}, err
	}
	res := authz.Resource{OrgID: art.OrgID, TeamID: team, ProjectID: art.ProjectID}
	if !authz.Can(p, downloadRoles, res) {
		if !authz.Can(p, []authz.Role{authz.Developer}, res) || s.developerDownloads == nil {
			return Download{}, ErrForbidden
		}
		allowed, err := s.developerDownloads(ctx, art.OrgID)
		if err != nil {
			return Download{}, err
		}
		if !allowed {
			return Download{}, ErrForbidden
		}
	}
	v, ok, err := s.validationOf(ctx, Publication{ArtifactID: art.ID})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Download{}, err
	}
	if !ok || v.State != validation.Completed || v.Verdict != "passed" {
		return Download{}, &Invalid{"only an image whose checks passed can be downloaded"}
	}
	if v.Classification == "restricted" {
		return Download{}, &Invalid{"this image passed only through an approved exception and is restricted; it can leave Samuori only through a destination that accepts restricted images"}
	}
	var worker string
	switch err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT worker_id FROM artifact_quarantines WHERE org_id = ? AND digest = ? AND released_at IS NULL LIMIT 1`), art.OrgID, art.Digest).Scan(&worker); {
	case err == nil:
		return Download{}, &Invalid{"this image is quarantined because its worker was revoked as compromised"}
	case !errors.Is(err, sql.ErrNoRows):
		return Download{}, err
	}
	f, err := s.blobs.File(art.Digest)
	if err != nil {
		return Download{}, &Invalid{"the image is no longer in internal storage"}
	}
	f.Close()
	var project, app, ver string
	_ = s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT p.name, COALESCE(sp.name, ''), COALESCE(r.version, '') FROM build_runs r JOIN projects p ON p.id = r.project_id LEFT JOIN build_specs sp ON sp.id = r.spec_id WHERE r.id = ?`), art.BuildRunID).Scan(&project, &app, &ver)
	short := strings.TrimPrefix(art.Digest, "sha256:")[:12]
	name := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(strings.Join([]string{project, app, short}, "-")), "-"), "-")
	if ver == "" {
		ver = "samuori-" + short
	}
	d := Download{Digest: art.Digest, Size: art.Size, Filename: name + ".tar", RepoTag: dockerRepoTag(project, app, ver)}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "image.download", ResourceType: "artifact", ResourceID: art.ID, Outcome: "succeeded", Reason: art.Digest, RequestID: requestID})
	})
	return d, err
}

// OpenDownload opens a download's archive for streaming with Docker's manifest.json added; the caller closes it.
func (s *Service) OpenDownload(d Download) (*DockerLoadable, func() error, error) {
	f, err := s.blobs.File(d.Digest)
	if err != nil {
		return nil, nil, err
	}
	l, err := NewDockerLoadable(f, d.RepoTag)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return l, f.Close, nil
}
