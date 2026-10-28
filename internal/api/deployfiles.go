package api

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"

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

func writeAttachment(w http.ResponseWriter, contentType, filename string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
