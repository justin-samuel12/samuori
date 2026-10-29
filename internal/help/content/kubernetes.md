---
title: Generate Kubernetes configuration
section: Kubernetes
summary: Turn a Compose file, source code or a Samuori image into manifests, a Helm chart or Kustomize, checked by Samuori, to review and apply yourself.
roles: [developer, maintainer, publisher, security-reviewer, administrator]
order: 75
---
Samuori can write a starting point for running your app on Kubernetes. It **generates and checks** the files; it never
applies them to a cluster. Someone who knows your cluster reviews them first.

## Start

**Kubernetes > Configurations** lists the conversions of every project you can see.

On a project page, or on an image, choose **Generate Kubernetes configuration**, then:

1. **Input.** A Compose file in one of the project's uploads (give its path, such as `compose.yml`), source code or
   a Dockerfile in an upload (with Move2Kube), or a built or published image. You can also replace one service's
   image with a Samuori build.
2. **Provider and format.** Kompose or, when an administrator has enabled it, Move2Kube; as plain manifests, a Helm
   chart or Kustomize.
3. **Name and namespace.**
4. **Ports and exposure.** Services come from the ports the Compose file publishes. Expose one service through a
   node port, a load balancer or an Ingress with a host and path.
5. **Replicas and resources.** Requests and limits go only to containers the Compose file gives none.
6. **Storage.** Volumes become persistent volume claims or empty directories. Host paths are never offered.
7. **Generate.** The conversion waits for a worker that has the approved provider version.

An administrator must first approve the provider under **Build supply > Build & conversion providers**; until then
the wizard shows it as unavailable.

## Move2Kube

Move2Kube also reads source code and Dockerfiles. It is **no longer maintained** (no release since March 2025), so
it is off unless your administrator enabled it; review what it writes with extra care.

- It reads only Compose files that start with `version: "3.8"` (or 1, 2, 3) and have no top-level `name:`. Samuori
  refuses other Compose files for it; use Kompose for those.
- It answers its own questions with **defaults**, which are often wrong for your app: a port it cannot find becomes
  8080, every port gets an Ingress, images point at `quay.io`. The conversion then says **needs-answers** and still
  shows the files it generated.
- Under **Questions**, check each answer (Samuori shows the default and the value used), then choose **Run again with
  these answers**. A new answer can bring new questions, for example about a port you changed; answer those and run
  again until none is open.
- When it writes its own Dockerfile for a service instead of using yours, a review item says so: the port and image
  in the files come from that Dockerfile. Choose a Samuori build for the service to use your image.
- It may not carry settings such as `privileged` over consistently; compare the review items with your Compose
  file.

## Review

The conversion page shows:

- **Objects:** every generated file.
- **Secrets & environment:** environment values that looked like passwords, tokens or keys. Samuori replaced each with a
  reference to a Kubernetes Secret, `<workload>-secrets`; create those keys in the cluster. The values are never in
  the files or the download.
- **Policy and validation:** errors for privileged containers, host networking, host paths, added capabilities,
  root users and objects that are not valid for the targeted Kubernetes version; review items such as images not
  pinned by digest, missing probes or missing resource limits.
- **Warnings & unsupported:** what the provider could not translate, for example a host bind mount that became a volume
  claim without its data, and what Samuori changed from your choices.

A readiness probe is added only when Samuori has evidence: the runtime check of the image's build named a port.
Otherwise the missing probe is listed for you to decide.

## Edit and download

Edit a file in the preview and **Save as new revision**: Samuori checks every file again and keeps the earlier
revision. **Download ZIP** gives the files with a `samuori-review.json` of the findings and where they came from. Run
the conversion again after changing the Compose file or the options; each run is a new revision in **History**.

## Rename or delete

**Rename** changes the conversion's name; files already generated keep the old name inside them until the next run or
edit. **Delete** removes the conversion with all its runs, revisions and files, so they can no longer be downloaded;
the audit log keeps a record. It is not possible while a run is in progress.

## Who can do what

People with a build role on the project generate, edit, rename and run again; the person who created a conversion
and the project's maintainers can delete it; everyone who can see the project can read and download. Other teams cannot see the conversion at all.
