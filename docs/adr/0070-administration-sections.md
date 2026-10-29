# 0070. Four administration sections and one worker status

Status: **Accepted** (2026-10-29, owner approval to restructure the administration navigation after a usability
review; provisional, may change during acceptance testing). Amends the portal navigation of
[ADR 0007](0007-canonical-navigation.md); the section list in
[`../product/administration.md`](../product/administration.md) stays the catalogue of what administration covers.

## Context
The portal spread administration over four top-bar workspaces (Registries & Recipes, Workers, Security & Secrets,
Administration with People, Platform and Operations groups). The Administration tab strip clipped at 1280–1440 px,
two pages were called Overview, and several pages repeated the controllers table. The pages also disagreed about
workers: the Administration overview counted eligible workers as ready while Build engines and Network egress counted
only connected ones, so one installation showed "1 / 1 ready" and "0 connected" at once.

## Decision
- **Four sections**, each shown only to roles that can use one of its pages:
  - **Build supply:** Registries & destinations, Recipes & runtimes, Build policies, Build & conversion providers.
  - **Infrastructure:** Workers & pools, Servers, Capacity, Build engines, Build queue, Storage, Database, Messaging.
  - **Security:** Secrets, Certificates, Egress, Quarantines, Audit & observability.
  - **Organization:** Health, Users, Teams, Identity & sign-in, Notification channels, Federation, Backups,
    Export & import, Upgrades & diagnostics.
- The former Administration overview is **Health** at `/admin/health`; `/admin` redirects there, so the top-bar
  **Overview** is the only overview. Page paths are otherwise unchanged; moved addresses are kept in one redirect
  table (`ui/src/shell/nav.ts`).
- An administration section's pages **wrap** under the top bar instead of scrolling, and fold into a menu at phone
  width. Developer workspaces keep their tabs.
- **One worker status.** The Worker resource reports `connected` (a live stream to a running controller). Every
  page computes the same four figures from it: **enrolled** (current certificate, not being removed), **in a pool**,
  **connected now** and **ready to build** (connected and eligible for uploaded source). Eligibility alone never
  reads as ready.
- The controllers table lives only on **Servers**; Database and Upgrades show a one-line summary that links to it.
- Destructive row actions use the danger style, and the top-bar health indicator is shown to administrators and
  operators with its label.

## Consequences
- Bookmarks and guides that used `/admin` keep working; other admin paths did not change.
- Guides and operator documents name pages as *Section > Page* with the new names.
- Server-side authorization is unchanged: navigation hides only what the server would refuse.
