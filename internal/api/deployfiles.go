package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/deployfiles"
	"samuori.invalid/samuori/internal/publication"
)

func (a *API) deployFiles(w http.ResponseWriter, r *http.Request, id string, publicationID *string) (publication.DeployFiles, bool) {
	var chosen string
	if publicationID != nil {
		chosen = *publicationID
	}
	d, err := a.publications.DeployFiles(r.Context(), principal(r), operationRoles(r.Context()), id, chosen)
	if err != nil {
		a.publicationError(w, r, err)
		return d, false
	}
	return d, true
}

// GetDeploymentFiles says which image reference the deployment files use (ADR 0059).
func (a *API) GetDeploymentFiles(w http.ResponseWriter, r *http.Request, id apigen.ArtifactId, params apigen.GetDeploymentFilesParams) {
	d, ok := a.deployFiles(w, r, id, params.PublicationId)
	if !ok {
		return
	}
	out := apigen.DeploymentFiles{ArtifactId: d.ArtifactID, Reference: d.Input.Image.Reference(), Published: d.Input.Image.Published,
		ComposeFilename: "compose.yml", ChartFilename: deployfiles.ChartFilename(d.Input), Publications: []apigen.DeploymentTarget{}}
	if d.PublicationID != "" {
		out.PublicationId = ptr(d.PublicationID)
	}
	for _, t := range d.Publications {
		out.Publications = append(out.Publications, apigen.DeploymentTarget{PublicationId: t.PublicationID, DestinationName: ptr(t.DestinationName), Reference: t.Image.Reference(), PublishedAt: t.PublishedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// DownloadDeploymentCompose returns a starting compose.yml for the image (ADR 0059).
func (a *API) DownloadDeploymentCompose(w http.ResponseWriter, r *http.Request, id apigen.ArtifactId, params apigen.DownloadDeploymentComposeParams) {
	d, ok := a.deployFiles(w, r, id, params.PublicationId)
	if !ok {
		return
	}
	if err := a.publications.AuditDeployFiles(r.Context(), principal(r), d, "compose", requestID(w)); err != nil {
		a.internalError(w, r, err)
		return
	}
	body := deployfiles.Compose(d.Input)
	writeAttachment(w, "application/yaml", "compose.yml", body)
}

// DownloadDeploymentHelmChart returns a starting Helm chart for the image (ADR 0059).
func (a *API) DownloadDeploymentHelmChart(w http.ResponseWriter, r *http.Request, id apigen.ArtifactId, params apigen.DownloadDeploymentHelmChartParams) {
	d, ok := a.deployFiles(w, r, id, params.PublicationId)
	if !ok {
		return
	}
	var buf bytes.Buffer
	if err := deployfiles.WriteChart(&buf, d.Input); err != nil {
		a.internalError(w, r, err)
		return
	}
	if err := a.publications.AuditDeployFiles(r.Context(), principal(r), d, "helm", requestID(w)); err != nil {
		a.internalError(w, r, err)
		return
	}
	writeAttachment(w, "application/gzip", deployfiles.ChartFilename(d.Input), buf.Bytes())
}

type deployPlan struct {
	Components []struct {
		SpecID        string `json:"specId"`
		ArtifactID    string `json:"artifactId"`
		PublicationID string `json:"publicationId"`
		Replicas      *int   `json:"replicas"`
		Links         []struct {
			EnvName string `json:"envName"`
			Target  string `json:"target"`
		} `json:"links"`
		Volumes []apigen.ProjectDeploymentVolume `json:"volumes"`
	} `json:"components"`
}

func (a *API) projectDeploy(w http.ResponseWriter, r *http.Request, id string, plan *string) (publication.ProjectDeploy, bool) {
	var choices []publication.DeployChoice
	if plan != nil && *plan != "" {
		var p deployPlan
		dec := json.NewDecoder(strings.NewReader(*plan))
		dec.DisallowUnknownFields()
		if len(*plan) > 16384 || dec.Decode(&p) != nil || len(p.Components) > 50 {
			apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", "plan must be a ProjectDeploymentPlan as JSON")
			return publication.ProjectDeploy{}, false
		}
		for _, c := range p.Components {
			ch := publication.DeployChoice{SpecID: c.SpecID, ArtifactID: c.ArtifactID, PublicationID: c.PublicationID, Replicas: c.Replicas}
			for _, l := range c.Links {
				ch.Links = append(ch.Links, deployfiles.Link{Env: l.EnvName, Target: l.Target})
			}
			for _, v := range c.Volumes {
				ch.Volumes = append(ch.Volumes, deployfiles.Volume{Name: v.Name, MountPath: v.MountPath})
			}
			choices = append(choices, ch)
		}
	}
	d, err := a.publications.ProjectDeployFiles(r.Context(), principal(r), operationRoles(r.Context()), id, choices)
	if err != nil {
		a.publicationError(w, r, err)
		return d, false
	}
	return d, true
}

// GetProjectDeploymentFiles lists each image definition of the project and the reference its files use (ADR 0061).
func (a *API) GetProjectDeploymentFiles(w http.ResponseWriter, r *http.Request, id apigen.ProjectId, params apigen.GetProjectDeploymentFilesParams) {
	d, ok := a.projectDeploy(w, r, id, params.Plan)
	if !ok {
		return
	}
	out := apigen.ProjectDeploymentFiles{ProjectId: d.ProjectID, ComposeFilename: "compose.yml", ChartFilename: deployfiles.ProjectChartFilename(d.Files),
		Components: []apigen.ProjectDeploymentComponent{}}
	specOf := map[string]string{}
	for _, c := range d.Components {
		if c.Available {
			specOf[c.Component.Name] = c.SpecID
		}
	}
	ports := map[string]int{}
	for _, c := range d.Files.Components {
		if c.App != nil {
			ports[c.Name] = c.App.Port
		}
	}
	for _, c := range d.Components {
		item := apigen.ProjectDeploymentComponent{SpecId: c.SpecID, SpecName: c.SpecName, Available: c.Available, Note: c.Note,
			Builds: []apigen.ProjectDeploymentBuild{}, Links: []apigen.ProjectDeploymentLink{}, Volumes: []apigen.ProjectDeploymentVolume{}}
		for _, b := range c.Builds {
			ab := apigen.ProjectDeploymentBuild{ArtifactId: b.ArtifactID, BuildRunId: b.BuildRunID, BuiltAt: b.BuiltAt, Publications: []apigen.DeploymentTarget{}}
			if b.Version != "" {
				ab.Version = ptr(b.Version)
			}
			for _, t := range b.Publications {
				ab.Publications = append(ab.Publications, apigen.DeploymentTarget{PublicationId: t.PublicationID, DestinationName: ptr(t.DestinationName), Reference: t.Image.Reference(), PublishedAt: t.PublishedAt})
			}
			item.Builds = append(item.Builds, ab)
		}
		if c.Available {
			comp := c.Component
			item.Name, item.ArtifactId, item.Linkable = ptr(comp.Name), ptr(c.ArtifactID), ptr(comp.Linkable())
			item.Reference, item.Published, item.Replicas = ptr(comp.Image.Reference()), ptr(comp.Image.Published), comp.Replicas
			if c.PublicationID != "" {
				item.PublicationId = ptr(c.PublicationID)
			}
			if comp.App != nil {
				if comp.App.Type != "" {
					item.Type = ptr(comp.App.Type)
				}
				if comp.App.Port > 0 {
					item.Port = ptr(comp.App.Port)
				}
			}
			for _, l := range comp.Links {
				item.Links = append(item.Links, apigen.ProjectDeploymentLink{EnvName: l.Env, Target: specOf[l.Target], Url: fmt.Sprintf("http://%s:%d", l.Target, ports[l.Target])})
			}
			for _, v := range comp.Volumes {
				item.Volumes = append(item.Volumes, apigen.ProjectDeploymentVolume{Name: v.Name, MountPath: v.MountPath})
			}
		}
		out.Components = append(out.Components, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// projectDeployDownload resolves the files, refuses a project with nothing to run and audits the download.
func (a *API) projectDeployDownload(w http.ResponseWriter, r *http.Request, id string, plan *string, kind string, render func(publication.ProjectDeploy) ([]byte, error)) ([]byte, publication.ProjectDeploy, bool) {
	d, ok := a.projectDeploy(w, r, id, plan)
	if !ok {
		return nil, d, false
	}
	if len(d.Files.Components) == 0 {
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", "no image of this project has a successful build yet")
		return nil, d, false
	}
	body, err := render(d)
	if err == nil {
		err = a.publications.AuditProjectDeployFiles(r.Context(), principal(r), d, kind, requestID(w))
	}
	if err != nil {
		a.internalError(w, r, err)
		return nil, d, false
	}
	return body, d, true
}

// DownloadProjectDeploymentCompose returns a starting compose.yml for every image of the project (ADR 0061).
func (a *API) DownloadProjectDeploymentCompose(w http.ResponseWriter, r *http.Request, id apigen.ProjectId, params apigen.DownloadProjectDeploymentComposeParams) {
	body, _, ok := a.projectDeployDownload(w, r, id, params.Plan, "compose", func(d publication.ProjectDeploy) ([]byte, error) {
		return deployfiles.ProjectCompose(d.Files), nil
	})
	if ok {
		writeAttachment(w, "application/yaml", "compose.yml", body)
	}
}

// DownloadProjectDeploymentHelmChart returns a starting Helm chart for every image of the project (ADR 0061).
func (a *API) DownloadProjectDeploymentHelmChart(w http.ResponseWriter, r *http.Request, id apigen.ProjectId, params apigen.DownloadProjectDeploymentHelmChartParams) {
	body, d, ok := a.projectDeployDownload(w, r, id, params.Plan, "helm", func(d publication.ProjectDeploy) ([]byte, error) {
		var buf bytes.Buffer
		err := deployfiles.WriteProjectChart(&buf, d.Files)
		return buf.Bytes(), err
	})
	if ok {
		writeAttachment(w, "application/gzip", deployfiles.ProjectChartFilename(d.Files), body)
	}
}

func writeAttachment(w http.ResponseWriter, contentType, filename string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
