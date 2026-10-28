---
title: Create an image
section: Building images
summary: The New image steps, the form and YAML views, and starting a build.
roles: [developer, maintainer]
order: 20
---
**New image** walks you through a build specification in seven steps:

1. **Upload source**: pick an upload, or upload new files.
2. **Choose app**: what to build. If the upload has a Dockerfile, **Build my Dockerfile** builds it exactly as
   written; otherwise choose one of the applications Samuori recognised.
3. **Describe your app**: what kind it is (web service, background worker, command-line tool or static website),
   the commands that install, build, start and test it, and the port it listens on.
4. **Choose how to build**: **Automatic** picks for you from what Samuori found in your files and your organization's
   policy, and says why. Or choose **Railpack** (plans and builds from your project files on a build worker, when an
   administrator enabled it), **Samuori recipe** (an approved recipe; your organization's own recipes appear in the
   **Recipe** list too, with their few settings; for a built-in recipe choose the **Runtime version** here, where
   versions your organization added are marked), or **Custom Dockerfile** if your project has one (already chosen if
   you picked it in step 2). **Paketo Buildpacks** detects and builds your project with the builder your
   administrator approved, on a build worker; set buildpack options such as `BP_NODE_VERSION` under
   `spec.build.buildpacks.env` and give each build secret a binding under `spec.build.buildpacks.bindings` (for an npm
   token, type `npmrc` with key `.npmrc`). A choice your administrator has not allowed is greyed out with the reason.
   Samuori reads a Dockerfile but never runs it to plan the build; Railpack and buildpacks run only on a build worker, and
   the plan or the buildpacks used are shown on the build page.
5. **Checks**: how Samuori proves the image works, for example that it starts and answers HTTP requests, plus an
   optional test command.
6. **Destination & tags**: where the image may be published, and its name and tags.
7. **Review & build**: confirm and press **Build**.

## Form and YAML

The same specification can be edited two ways: the **Form** asks every question in the step where it belongs, and
**YAML** is the file itself. Switching keeps your changes; Samuori warns you before one view overwrites the other.

Install, build and test commands are lists: add a line per command and order them with the arrows. They run in
order and the build stops at the first that fails. The start command is a single line, because a container runs one
main process; to start several, add a start script to your project and run that.

With a Samuori recipe you can also list **system packages**: operating system packages the build needs (such as
`build-essential` or `git`) and packages your app needs at run time (such as a shared library). Samuori installs them
with the base image's package manager and allows the matching package archive in the build's network rules.

## When it cannot build yet

If no verified worker can build for your project, or a required secret has no key, the build is created as
**Blocked** with the reason. Nothing is lost: fix the cause and use **Build again**. See
[Builds and logs](help:builds).
