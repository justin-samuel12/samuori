# 0061. Deployment files for a project

Status: **Accepted** (2026-10-28, owner request; provisional, may change during acceptance testing). Extends
[ADR 0059](0059-deployment-files-for-built-images.md).

## Context
ADR 0059 gives each built image a starting `compose.yml` and Helm chart. A project often has several image
definitions, such as a web front end, an API and a background worker, that run together. Downloading one file per
image and joining them by hand loses what matters most: which images talk to each other, how many copies run and
where data is kept.

## Options
- **One Compose file and one Helm chart for the whole project, generated on request (chosen):** reuses ADR 0059's
  generator and reference rules; the person's choices travel in the request and nothing is stored.
- **Stored deployment profiles per project (rejected for now):** would need a new resource, migrations, revisions and
  permissions for something that is only a starting file.
- **An umbrella chart with one ADR 0059 subchart per image (rejected):** harder to read and edit, and links between
  subcharts need global values or naming conventions the user must learn.

## Decision
- **Scope.** As in ADR 0059: Samuori writes starting files; it never deploys, applies, orchestrates or reconciles
  anything. The files say so in their header.
- **API.** `GET /projects/{id}/deploy` lists every image definition (spec) of the project with its recent successful
  builds (newest ten), each build's successful, unretired publications, and the reference the files would use.
  `GET /projects/{id}/deploy/compose` returns `compose.yml` and `GET /projects/{id}/deploy/helm` a chart
  `<project>-<chart version>.tgz`. All three take an optional `plan` query parameter: a `ProjectDeploymentPlan` as
  JSON (at most 16 KiB) with, per spec, `artifactId`, `publicationId`, `replicas` (0–20, default 1), `links`
  (`envName`, `target` spec) and `volumes` (`name`, `mountPath`). GET with a query parameter keeps the downloads plain
  links, like ADR 0059's; nothing in the plan is stored.
- **Choice and references.** Per spec: the chosen build, else the newest successful one; then ADR 0059's rule
  unchanged: the chosen or newest successful publication by `<registry>/<repository>:<tag>@<digest>`, else the
  `docker load` name with `pull_policy: never` / `pullPolicy: Never`. A spec without a successful build is listed as
  unavailable with a note and left out of the files, which name it in a "Left out" header line; it is never faked. A
  build or publication that is not one of the spec's own is refused with 422.
- **Links.** `envName` must match `^[A-Z_][A-Z0-9_]{0,63}$` and be unique per image; the target must be another
  image in the files that serves HTTP on a port (`http-service` or `static-site` with `spec.application.port`). The
  variable is set to the target's in-network URL: `http://<service>:<port>` in Compose and
  `http://<release>-<component>:<service port>` in Helm.
- **Replicas.** Compose gets `deploy.replicas` when it is not 1; a serving image with more than one replica, or a
  second image on a port already taken, gets a Docker-chosen host port so containers do not collide. Helm gets
  `replicaCount` per component. A command-line task (a Job) ignores replicas.
- **Volumes.** A name (`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`, unique per image) and a clean absolute mount path
  other than `/` (letters, digits and `._@+-` only). Compose gets a named volume `<component>-<name>`; Helm a
  `ReadWriteOnce` PersistentVolumeClaim `<release>-<component>-<name>` whose `storageClass` and `size` are empty
  values. `helm install` stops with a message until the size is set, so no size is guessed.
- **Chart.** One chart named after the project; version from the newest chosen build's version (ADR 0059's rule). Per
  component a Deployment (or Job for a command-line task), a Service when it serves, a claim per volume, and values
  keyed by component: `image`, `replicaCount`, `containerPort`, `service`, `probes`, `env`, `existingSecret`,
  `resources`, `persistence`. Resources carry an `app.kubernetes.io/component` label. Probes come from each spec as
  in ADR 0059. No secret IDs or values, build argument values or internal paths are written. Output is deterministic.
- **Authorization and audit.** Anyone who may see the project and its images may use the endpoints (developer,
  maintainer, publisher, security reviewer, administrator, with project visibility), as in ADR 0059; other teams get
  404. Each file download is audited as `project.deployment_files` with the component references.
- **UI.** The project page shows "Deployment files" when any image has a successful build. The dialog offers per
  image a build and publication picker, replicas, links and volumes, a note on the reference used, and the two
  downloads.

## Consequences
- A multi-image application gets one coherent starting file with its internal URLs wired, rather than several files
  joined by hand.
- Choices are lost when the dialog closes; storing profiles can be added later without changing the files.
- Links assume HTTP; other protocols are left to the user, as are environment, secrets and resources (ADR 0059).
- Reversible: three read-only endpoints and one dialog; no stored state.
