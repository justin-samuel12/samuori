---
title: Getting started
section: Getting started
summary: What Samuori does, how the screen is laid out, and where to begin for your role.
roles: [everyone]
order: 1
---
Samuori turns an application into a validated container image from the browser. You upload
your source, Samuori works out what it is without running it, you confirm how to build it, and a dedicated worker
builds, checks and publishes the image.

## Finding your way

- The **top bar** groups pages into workspaces: **Overview**, **Projects**, **Builds** (build runs, images, pipelines
  and approvals) and **Kubernetes**. Administrators and operators also see **Build supply** (registries, recipes,
  policies and providers), **Infrastructure** (workers, servers, capacity, storage, database and messaging),
  **Security** (secrets, certificates, egress, quarantines and audit) and **Organization** (health, people, sign-in,
  notification channels, federation, backups and upgrades). Each workspace lists its pages under the top bar; on a
  phone they fold into a menu. You only see pages your roles can use.
- **Search** (Ctrl+K, or ⌘K on a Mac) finds any page or guide by name.
- The **account menu** at the top right holds Account, Notifications, Help, the theme and Sign out; the bell opens
  Notifications. The guides you see depend on your roles; open [Roles and access](help:roles) to see what each
  role can do.
- A **?** next to a page title opens the guide for that page.

## Where to start

| If you… | Start here |
|---|---|
| build images for an application | [Upload your source](help:upload-source), then [Create an image](help:new-image) |
| release images to registries | [Publish and promote images](help:publishing) |
| run the build workers | [Workers and pools](help:workers) |
| review security | [Audit log and quarantines](help:security-review) |
| administer Samuori | [Teams, users and roles](help:people-and-access) |

## If you see no pages

You have no role yet. Ask an administrator to add you to a team. Your roles are listed on your
[Account](/account) page.
