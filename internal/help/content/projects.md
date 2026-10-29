---
title: Manage projects
section: Projects
summary: Create projects and request repositories and output exceptions.
roles: [maintainer, administrator]
order: 60
---
A **project** is one application and the images built from it. It belongs to a team, and team members' roles
apply to it.

## Create a project

On **Projects**, choose **New project**, pick the team and a name (lower-case letters, digits and dashes), and
whether approved images are published manually or automatically.

## Rename a project

A project's maintainers and administrators can **Rename** it on its page. The name follows the same rule and must be
unique in the team. Builds, images and history keep their IDs; repositories and tags already published are not
renamed. A project cannot move to another team.

When the organization has more than one location, the page shows **Build locations**. Administrators choose
**Change locations** to keep the project's builds in some locations only (data residency); none allows any location.

## Archive, restore or purge a project

A project's maintainers and administrators can **Archive project** on its page. An archived project is hidden from
**Projects** (turn on **Show archived projects** to see it) and accepts no new uploads, specs, builds, publications
or requests. Queued and running builds are cancelled. Its secret grants and bindings, egress allowances, destination
access and pipelines are revoked and pending requests are closed. **Restore** makes it usable again, but the grants
do not come back: an administrator must grant them again.

An administrator can **Purge** an archived project by typing its name. This deletes the uploads, images, logs,
reports and SBOMs it stored, keeps its name, records and audit trail, and cannot be undone. A purged project cannot
be restored. Images already pushed to external registries are not deleted.

## Deployment files for the whole project

Once an image of the project has built successfully, **Deployment files** on the project page gives one
`compose.yml` and one Helm chart that run all of the project's images together. For each image you can pick the
build and publication, the number of replicas, **links** (an environment variable set to another image's address,
such as `API_URL` pointing at `http://api:8080`) and **volumes** (a name and an absolute mount path). Images that
have not built successfully are listed and left out. A published image is named by tag and digest; an unpublished
one by the name `docker load` gives it, so download and load it first. Set each volume's size in the Helm values
before installing. Samuori does not deploy these files, and nothing you choose is saved.

## On the project page

- **Source uploads:** every upload, newest first.
- **Repository requests:** ask for a registry repository for this project's images, with a justification.
  An administrator creates it. You can **Withdraw** your own request while it is waiting; maintainers can withdraw
  any. The same applies to base image and network access requests.
- **Output exceptions:** request permission for the image to contain specific sensitive paths, with the reason and
  how many days it is needed. See [Validation results](help:validation).
