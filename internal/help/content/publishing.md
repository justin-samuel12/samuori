---
title: Publish and promote images
section: Releasing
summary: Push validated images to registries, then promote, roll back, deprecate or retire them.
roles: [maintainer, publisher]
order: 70
---
A built image is **ready**, not published. Publishing pushes it to a **destination**: a registry location an
administrator set up and opened to your project.

## Publish

1. Open **Images**, choose the project, then the image. Images are named by app and version, such as
   `shop · 2026-10-29.1`, with the short digest beside them. A finished build's **Next step** card offers the same
   **Publish image**.
2. **Publish image**: pick a destination and the tags.
3. Samuori checks again before pushing: your permission, the destination, the validation verdict, approvals, signing
   and, for restricted images, that the destination accepts them. The publication records the image digest.

If the destination needs an approval, the publication waits until someone else decides it. See
[Approvals](help:approvals).

## After publishing

Each publication offers:

- **Promote:** copy the image to another destination by digest, without rebuilding. Samuori revalidates it against
  current policy first.
- **Roll back tags:** move tags back to this publication.
- **Deprecate:** mark it as not recommended; it stays available.
- **Retire:** withdraw it, with a reason.

## Versions and default tags

Every successful build gets a version: the UTC date it succeeded and a counter for that day across the project, such
as `2026-10-27.3`. The build page and the Images page show it. When you publish without typing tags, Samuori uses the
image definition's tags, set under **Image details** in the New image wizard, which default to the version alone.
The permanent `samuori-` tag is always added. If a tag you list there was already published, saving warns you; publishing
again moves it, unless the destination protects it or keeps tags immutable.
