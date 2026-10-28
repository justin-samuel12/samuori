package publication

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/deployfiles"
	"samuori.invalid/samuori/internal/tenancy"
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
	names, err := s.destinationNames(ctx, art.OrgID)
	if err != nil {
		return DeployFiles{}, err
	}
	pubs, err := s.deployTargets(ctx, art.OrgID, art.ID, names)
	if err != nil {
		return DeployFiles{}, err
	}
	n := s.imageNames(ctx, art.BuildRunID, art.Digest)
	name := n.app
	if name == "" {
		name = n.project
	}
	out := DeployFiles{ArtifactID: art.ID, Publications: pubs,
		Input: deployfiles.Input{Name: name, Version: n.version, App: specApplication(n.spec), ModTime: art.CreatedAt}}
	out.PublicationID, out.Input.Image, err = chooseImage(pubs, publicationID, n.repoTag)
	if err != nil {
		return DeployFiles{}, err
	}
	if out.Input.Version == "" {
		out.Input.Version = out.Input.Image.Tag
	}
	return out, nil
}

func specApplication(document string) *buildspec.Application {
	var doc buildspec.Document
	_ = json.Unmarshal([]byte(document), &doc)
	return doc.Spec.Application
}

func chooseImage(pubs []DeployTarget, publicationID, repoTag string) (string, deployfiles.Image, error) {
	switch {
	case publicationID != "":
		for _, t := range pubs {
			if t.PublicationID == publicationID {
				return t.PublicationID, t.Image, nil
			}
		}
		return "", deployfiles.Image{}, &Invalid{"that publication is not a successful, unretired publication of this image"}
	case len(pubs) > 0:
		return pubs[0].PublicationID, pubs[0].Image, nil
	}
	repo, tag, ok := strings.Cut(repoTag, ":")
	if !ok {
		return "", deployfiles.Image{}, &Invalid{"the image has no name to load it under"}
	}
	return "", deployfiles.Image{Repository: repo, Tag: tag}, nil
}

func (s *Service) destinationNames(ctx context.Context, orgID string) (map[string]string, error) {
	all, err := s.destinations(ctx, orgID)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, d := range all {
		names[d.ID] = d.Name
	}
	return names, nil
}

// deployTargets are an image's successful, unretired publications, newest first.
func (s *Service) deployTargets(ctx context.Context, orgID, artifactID string, names map[string]string) ([]DeployTarget, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectPublication+` WHERE artifact_id = ? AND org_id = ? AND state = ? AND retired_at IS NULL ORDER BY updated_at DESC, id DESC LIMIT 50`), artifactID, orgID, Published)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeployTarget{}
	for rows.Next() {
		pub, err := scanPublication(rows)
		if err != nil {
			return nil, err
		}
		repo, _, _ := strings.Cut(pub.DestinationRef, "@")
		if repo == "" || pub.ImageDigest == "" {
			continue
		}
		tag := BuildTag(pub.ImageDigest)
		if len(pub.Tags) > 0 {
			tag = pub.Tags[0]
		}
		out = append(out, DeployTarget{PublicationID: pub.ID, DestinationName: names[pub.DestinationID], PublishedAt: pub.UpdatedAt,
			Image: deployfiles.Image{Repository: repo, Tag: tag, Digest: pub.ImageDigest, Published: true}})
	}
	return out, rows.Err()
}

// AuditDeployFiles records that someone downloaded a deployment file and which image reference it named.
func (s *Service) AuditDeployFiles(ctx context.Context, p authz.Principal, d DeployFiles, kind, requestID string) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "image.deployment_files", ResourceType: "artifact", ResourceID: d.ArtifactID, Outcome: "succeeded",
			Reason: kind + " " + d.Input.Image.Reference(), RequestID: requestID})
	})
}

// DeployBuild is a successful build of an image definition that project deployment files can use.
type DeployBuild struct {
	ArtifactID, BuildRunID, Version string
	BuiltAt                         time.Time
	Publications                    []DeployTarget
	digest                          string
}

// DeployChoice is what a person picked for one image definition; empty fields take the defaults.
type DeployChoice struct {
	SpecID, ArtifactID, PublicationID string
	Replicas                          *int
	// Links name the target image definition by spec ID.
	Links   []deployfiles.Link
	Volumes []deployfiles.Volume
}

// ProjectDeployComponent is one image definition of the project and what the files use for it.
type ProjectDeployComponent struct {
	SpecID, SpecName string
	// Available is false when the definition has no successful build; it is then left out of the files.
	Available bool
	Note      string
	// Builds are the definition's recent successful builds, newest first.
	Builds                    []DeployBuild
	ArtifactID, PublicationID string
	Component                 deployfiles.Component
}

// ProjectDeploy is what a project's deployment files are generated from (ADR 0061).
type ProjectDeploy struct {
	ProjectID  string
	Components []ProjectDeployComponent
	Files      deployfiles.Project
}

const deployBuildsPerSpec = 10

// ProjectDeployFiles resolves deployment files for every image definition of a project: per definition the chosen
// or newest successful build, and the chosen or newest publication of it, else its docker load name. Visibility is
// the project's, as for one image's files (ADR 0059); other teams get not found.
func (s *Service) ProjectDeployFiles(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string, choices []DeployChoice) (ProjectDeploy, error) {
	proj, err := s.tenancy.GetProject(ctx, p, roles, projectID)
	if errors.Is(err, tenancy.ErrNotFound) || errors.Is(err, tenancy.ErrForbidden) {
		return ProjectDeploy{}, ErrNotFound
	}
	if err != nil {
		return ProjectDeploy{}, err
	}
	names, err := s.destinationNames(ctx, proj.OrgID)
	if err != nil {
		return ProjectDeploy{}, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, name FROM build_specs WHERE project_id = ? AND org_id = ? ORDER BY name, id`), proj.ID, proj.OrgID)
	if err != nil {
		return ProjectDeploy{}, err
	}
	out := ProjectDeploy{ProjectID: proj.ID, Files: deployfiles.Project{Name: proj.Name}}
	for rows.Next() {
		var c ProjectDeployComponent
		if err := rows.Scan(&c.SpecID, &c.SpecName); err != nil {
			rows.Close()
			return ProjectDeploy{}, err
		}
		out.Components = append(out.Components, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ProjectDeploy{}, err
	}

	bySpec := map[string]*DeployChoice{}
	for i := range choices {
		c := &choices[i]
		if bySpec[c.SpecID] != nil {
			return ProjectDeploy{}, &Invalid{"each image definition may be chosen once"}
		}
		bySpec[c.SpecID] = c
	}
	known := map[string]bool{}
	for _, c := range out.Components {
		known[c.SpecID] = true
	}
	for id := range bySpec {
		if !known[id] {
			return ProjectDeploy{}, &Invalid{"a chosen image definition is not part of this project"}
		}
	}

	slugs := map[string]bool{}
	nameOf := map[string]string{}
	var newest time.Time
	for i := range out.Components {
		c := &out.Components[i]
		if c.Builds, err = s.deployBuilds(ctx, proj.OrgID, c.SpecID, names); err != nil {
			return ProjectDeploy{}, err
		}
		choice := bySpec[c.SpecID]
		if choice == nil {
			choice = &DeployChoice{}
		}
		if len(c.Builds) == 0 {
			if choice.ArtifactID != "" || choice.PublicationID != "" {
				return ProjectDeploy{}, &Invalid{c.SpecName + " has no successful build to choose"}
			}
			c.Note = "No successful build yet; left out of the files."
			out.Files.Skipped = append(out.Files.Skipped, c.SpecName+" has no successful build.")
			continue
		}
		b := c.Builds[0]
		if choice.ArtifactID != "" {
			found := false
			for _, cand := range c.Builds {
				if cand.ArtifactID == choice.ArtifactID {
					b, found = cand, true
				}
			}
			if !found {
				return ProjectDeploy{}, &Invalid{"the chosen image of " + c.SpecName + " is not one of its recent successful builds"}
			}
		}
		n := s.imageNames(ctx, b.BuildRunID, b.digest)
		c.Available, c.ArtifactID = true, b.ArtifactID
		c.Component = deployfiles.Component{Name: uniqueSlug(deployfiles.Slug(c.SpecName), slugs), App: specApplication(n.spec), Replicas: 1, Volumes: choice.Volumes}
		if choice.Replicas != nil {
			c.Component.Replicas = *choice.Replicas
		}
		if c.PublicationID, c.Component.Image, err = chooseImage(b.Publications, choice.PublicationID, n.repoTag); err != nil {
			return ProjectDeploy{}, err
		}
		if c.Component.Image.Published {
			c.Note = "Uses the published image, by tag and digest."
		} else {
			c.Note = "Not published: uses the name docker load gives the downloaded image; load it first."
		}
		nameOf[c.SpecID] = c.Component.Name
		if b.BuiltAt.After(newest) {
			newest, out.Files.Version = b.BuiltAt, b.Version
		}
	}
	for i := range out.Components {
		c := &out.Components[i]
		choice := bySpec[c.SpecID]
		if !c.Available || choice == nil {
			continue
		}
		for _, l := range choice.Links {
			target, ok := nameOf[l.Target]
			if !ok {
				return ProjectDeploy{}, &Invalid{c.SpecName + ": " + l.Env + " must point at an image of this project with a successful build"}
			}
			c.Component.Links = append(c.Component.Links, deployfiles.Link{Env: l.Env, Target: target})
		}
	}
	for _, c := range out.Components {
		if c.Available {
			out.Files.Components = append(out.Files.Components, c.Component)
		}
	}
	if err := out.Files.Validate(); err != nil {
		return ProjectDeploy{}, &Invalid{err.Error()}
	}
	out.Files.ModTime = newest
	return out, nil
}

func uniqueSlug(slug string, used map[string]bool) string {
	name := slug
	for i := 2; used[name]; i++ {
		name = slug + "-" + strconv.Itoa(i)
	}
	used[name] = true
	return name
}

// deployBuilds are a definition's newest successful builds with an image, newest first.
func (s *Service) deployBuilds(ctx context.Context, orgID, specID string, names map[string]string) ([]DeployBuild, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT a.id, r.id, COALESCE(r.version, ''), a.digest, a.created_at FROM build_runs r
		JOIN build_artifacts a ON a.run_id = r.id AND a.kind = 'image'
		WHERE r.spec_id = ? AND r.org_id = ? AND r.state = 'succeeded' AND r.id NOT IN (SELECT run_id FROM build_run_deletions)
		ORDER BY a.created_at DESC, a.id DESC LIMIT ?`), specID, orgID, deployBuildsPerSpec)
	if err != nil {
		return nil, err
	}
	var out []DeployBuild
	for rows.Next() {
		var b DeployBuild
		if err := rows.Scan(&b.ArtifactID, &b.BuildRunID, &b.Version, &b.digest, &b.BuiltAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Publications, err = s.deployTargets(ctx, orgID, out[i].ArtifactID, names); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AuditProjectDeployFiles records that someone downloaded a project's deployment file and the references it named.
func (s *Service) AuditProjectDeployFiles(ctx context.Context, p authz.Principal, d ProjectDeploy, kind, requestID string) error {
	refs := make([]string, 0, len(d.Files.Components))
	for _, c := range d.Files.Components {
		refs = append(refs, c.Name+"="+c.Image.Reference())
	}
	sort.Strings(refs)
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "project.deployment_files", ResourceType: "project", ResourceID: d.ProjectID, Outcome: "succeeded",
			Reason: kind + " " + strings.Join(refs, " "), RequestID: requestID})
	})
}
