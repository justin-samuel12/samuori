# 0072. Operator logo branding

Status: **Accepted** (2026-10-29, owner request to let an installation show its own logo; provisional, may change
during acceptance testing). Builds on the brand rules in [ADR 0057](0057-name-and-brand.md) and the single-organization
decision in [ADR 0071](0071-single-organization-per-installation.md).

## Context
An operator running Samuori for their own developers wants the portal to carry their logo, not only Samuori's. The
request is logo-only: the colour scheme and Samuori's own artwork rules stay. Two places show the logo, and they are
different shapes:
- the portal top bar (`ui/src/shell/Shell.tsx`), a wide ~3:1 mark at ~120 px;
- the login page (`ui/src/pages/LoginPage.tsx`), a larger, roughly square stacked mark.
Each renders a light and a dark variant, because the surface is light or dark with the theme. A single uploaded logo
can therefore look wrong in one slot or vanish on one surface, which the admin needs help avoiding.

## Options
- **What is themeable:** logo only (chosen: it meets the request and keeps the semantic colour tokens and Samuori's
  mark unchanged, ADR 0057); logo plus accent colour (rejected for now: colour changes risk contrast and
  accessibility regressions the owner does not want); arbitrary CSS (rejected: a security and support hole).
- **How many images:** one horizontal logo, with an optional dark-mode version requested only when a contrast check
  flags the first (chosen: one upload for most operators, a second only when needed); always two or four slots
  (rejected: more faithful to today's four files but more work for the admin for little gain).
- **Format:** PNG or SVG, SVG sanitised (chosen: SVG stays crisp at every size; sanitising removes scripts and
  external references); PNG only (rejected: soft when scaled); any image (rejected: unbounded).
- **Guidance:** the admin page inspects the file on drop and gives specific feedback — aspect ratio against the top
  bar's wide strip, transparent-background and contrast checks against the dark bar, size and format rules — with a
  live preview in both slots and both themes (chosen); a static "recommended size" note (rejected: operators still
  upload the wrong shape and cannot see it).

## Decision
- **Logo only.** An operator may replace the logo shown in the portal top bar and on the login page. Nothing else is
  branded. Samuori's colour tokens, artwork and the "flat vs 3d" usage rules (ADR 0057) are unchanged; the operator
  logo is a layer on top, and Samuori's mark is the fallback when none is set.
- **One brand record per installation**, keyed by `org_id` (ADR 0071): effectively per-installation. Stored
  server-side; logo images are stored as blobs/artifacts, not inline in the row.
- **Images.** A primary horizontal logo (target ~3:1 to 4:1), and an optional dark-mode variant. PNG or SVG,
  transparent background recommended, bounded in size (about 512 KB), with a minimum height for crispness. SVGs are
  sanitised before storage; untrusted markup is never served as uploaded.
- **Serving and authorization.** `GET /branding` is readable by any signed-in user, because the portal and login need
  it at load. Setting or clearing the logo, and uploading an image, are administrator-only and enforced on the
  server. The login page is pre-authentication, so the brand it needs is served through an unauthenticated,
  logo-only, no-secrets endpoint or embedded at load; no protected data is exposed there.
- **The intelligent uploader.** The admin page (Organization > Branding) reads the file on drop and reports: the
  detected dimensions and aspect ratio with the top bar's target range; whether the background is transparent; a
  contrast check against the dark top bar that offers the optional dark variant when the logo may not show; and the
  format and size rules. It previews the logo live in both slots and both themes. It validates the same rules again
  on the server.
- **Edit and remove.** The logo can be changed and cleared (reverting to Samuori's), administrator-only, following
  the UI rules in SKILLS.md. Clearing is not destructive beyond removing the stored image.
- **Attribution stays.** Operator branding never removes the Samuori attribution, the `NOTICE`, or the licence. The
  About dialog continues to identify Samuori.

## Consequences
- A new `branding` table (both dialects), a logo blob store, `GET/PUT /branding` and an upload operation, and the
  Organization > Branding page. The top bar, login page and theme image handling read the operator logo with the
  Samuori mark as fallback.
- The login page gains a small unauthenticated brand read; it exposes only the logo and must carry no other data.
- `make screenshots` keeps the Samuori artwork, because the tour installs no operator logo; a scene may show the
  Branding page with the default state.
- D22 (UI) and the traceability matrix gain the branding page; a security note covers the sanitiser and the
  unauthenticated read.
