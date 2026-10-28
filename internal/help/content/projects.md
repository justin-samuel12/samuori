---
title: Manage projects
section: Building images
summary: Create projects and request repositories and output exceptions.
roles: [maintainer, administrator]
order: 60
---
A **project** is one application and the images built from it. It belongs to a team, and team members' roles
apply to it.

## Create a project

On **Projects**, choose **New project**, pick the team and a name (lower-case letters, digits and dashes), and
whether approved images are published manually or automatically.

## On the project page

- **Source uploads:** every upload, newest first.
- **Repository requests:** ask for a registry repository for this project's images, with a justification.
  An administrator creates it.
- **Output exceptions:** request permission for the image to contain specific sensitive paths, with the reason and
  how many days it is needed. See [Validation results](help:validation).
