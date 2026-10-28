package publication

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/deployfiles"
)

// DeployTarget is a successful publication deployment files can reference.
type DeployTarget struct {
	PublicationID, DestinationName string
	Image                          deployfiles.Image
	PublishedAt                    time.Time
}

// DeployFiles is what a built image's deployment files are generated from (ADR 0059).
type DeployFiles struct {
	ArtifactID, PublicationID string
	Input                     deployfiles.Input
	// Publications are the image's successful, unretired publications, newest first.
	Publications []DeployTarget
}

// DeployFiles chooses the image reference for deployment files: the chosen publication, else the newest successful
// one, else the name docker load gives the downloaded archive. Anyone who may see the image may see these
// references, as with its publications; the files never hold secret values.
func (s *Service) DeployFiles(ctx context.Context, p authz.Principal, roles []authz.Role, artifactID, publicationID string) (DeployFiles, error) {
	art, err := s.validations.Artifact(ctx, p, roles, artifactID)
	if err != nil || art.OrgID != p.OrgID {
		return DeployFiles{}, ErrNotFound
	}
	names := map[string]string{}
	all, err := s.destinations(ctx, art.OrgID)
	if err != nil {
		return DeployFiles{}, err
	}
	for _, d := range all {
		names[d.ID] = d.Name
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectPublication+` WHERE artifact_id = ? AND org_id = ? AND state = ? AND retired_at IS NULL ORDER BY updated_at DESC, id DESC LIMIT 50`), art.ID, art.OrgID, Published)
	if err != nil {
		return DeployFiles{}, err
	}
	out := DeployFiles{ArtifactID: art.ID, Publications: []DeployTarget{}}
	for rows.Next() {
		pub, err := scanPublication(rows)
		if err != nil {
			rows.Close()
			return DeployFiles{}, err
		}
		repo, _, _ := strings.Cut(pub.DestinationRef, "@")
		if repo == "" || pub.ImageDigest == "" {
			continue
		}
		tag := BuildTag(pub.ImageDigest)
		if len(pub.Tags) > 0 {
			tag = pub.Tags[0]
		}
		out.Publications = append(out.Publications, DeployTarget{PublicationID: pub.ID, DestinationName: names[pub.DestinationID], PublishedAt: pub.UpdatedAt,
			Image: deployfiles.Image{Repository: repo, Tag: tag, Digest: pub.ImageDigest, Published: true}})
	}
	rows.Close()

	n := s.imageNames(ctx, art.BuildRunID, art.Digest)
	var doc buildspec.Document
	_ = json.Unmarshal([]byte(n.spec), &doc)
	name := n.app
	if name == "" {
		name = n.project
	}
	out.Input = deployfiles.Input{Name: name, Version: n.version, App: doc.Spec.Application, ModTime: art.CreatedAt}
	switch {
	case publicationID != "":
		for _, t := range out.Publications {
			if t.PublicationID == publicationID {
				out.PublicationID, out.Input.Image = t.PublicationID, t.Image
			}
		}
		if out.PublicationID == "" {
			return DeployFiles{}, &Invalid{"that publication is not a successful, unretired publication of this image"}
		}
	case len(out.Publications) > 0:
		out.PublicationID, out.Input.Image = out.Publications[0].PublicationID, out.Publications[0].Image
	default:
		repo, tag, ok := strings.Cut(n.repoTag, ":")
		if !ok {
			return DeployFiles{}, &Invalid{"the image has no name to load it under"}
		}
		out.Input.Image = deployfiles.Image{Repository: repo, Tag: tag}
	}
	if out.Input.Version == "" {
		out.Input.Version = out.Input.Image.Tag
	}
	return out, nil
}

// AuditDeployFiles records that someone downloaded a deployment file and which image reference it named.
func (s *Service) AuditDeployFiles(ctx context.Context, p authz.Principal, d DeployFiles, kind, requestID string) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "image.deployment_files", ResourceType: "artifact", ResourceID: d.ArtifactID, Outcome: "succeeded",
			Reason: kind + " " + d.Input.Image.Reference(), RequestID: requestID})
	})
}
