---
title: Builds and logs
section: Builds
summary: What each build status means, and how to read, cancel and repeat a build.
roles: [developer, maintainer, operator]
order: 40
---
**Builds > Build runs** lists every build you can see. Open one to see its status, what to do next, validation, attempts
and logs. The page updates as the build moves. Digests, worker IDs and other raw values are under **Technical
details** at the bottom; the **Timeline** appears once the page receives state changes while you watch.

## Statuses

| Status | Meaning |
|---|---|
| **Checking** | Samuori is checking permissions, secrets and policy before queueing |
| **Waiting for a worker** | Queued until a verified worker is free |
| **Assigned**, **Preparing**, **Building**, **Saving the image** | Running on a worker |
| **Built** | The image exists. It is not published yet |
| **Blocked** | It cannot run yet; the reason says why (for example no verified worker, or a missing secret key) |
| **Failed** | It ran and failed. The message says whether the application build or the build infrastructure failed |
| **Timed out**, **Cancelled**, **Expired** | Stopped before it finished |

A build may use several **attempts**: if a worker is lost, Samuori retries on another one. Each attempt shows its
state (such as **Completed** or **Failed**) and its build environment (**Cleaned up** once it was destroyed).

## Next step

- A **Built** image that passed validation offers **Publish image** if you may publish in this project. Otherwise
  the card says who can (a publisher or maintainer of the project's team). **Open image**, **Download image** (when
  allowed) and **Deployment files** are there too.
- While the checks run, the card says publishing opens when every required check passes; an image that did not pass
  cannot be published.
- A **Failed** or **Timed out** build says why and links to the log and the attempts; a **Blocked** build says what
  blocks it. Both offer **Build again**.

## Actions

- **Cancel build** stops a running build. It ends as **Cancelled** with your reason, never as failed or built.
- **Build again** repeats the same build settings version (shown as **Build settings v1**, v2 and so on) and
  upload.
- **Logs** show each attempt. Secret values are masked before logs are stored.

## Blocked network access

When the build's network proxy refused a destination, the build page lists it with what would open it. For a
destination that only an organization administrator can open, a developer or maintainer can press **Request
access**, choose **Public internet** or **Internal network** for each host (private addresses and internal names can
only be internal), and say why it is needed. Samuori attaches the blocked build as evidence. Once an organization administrator
approves it in **Approvals**, the destination is open for this project only; run the build again. The same button
appears on **Will fail** items in the build readiness list. The project page lists the access opened for it.

## Download or delete a build

- **Download image** on the build page, or **Download** on the Images page, saves the image as a file you can load
  with `docker load -i <file>` or `podman load -i <file>`. It is offered to project maintainers, publishers and
  administrators once the image's checks have passed. Every download is recorded.
- **Delete build** removes the build's image, logs and cache to free space; the build stays in the history, marked
  as deleted. Maintainers and administrators can delete a finished build unless its image is published, held or
  still being checked. Deleting cannot be undone.
