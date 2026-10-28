# 0057. Name and brand

Status: **Accepted** (2026-10-27, owner decision). Not a trademark clearance.

## Context
The project needs a distinctive name and logo, and should say plainly why it exists: it unifies established
open-source build, scan and signing tools behind one guided, governed workflow.

## Decision
- **Name.** Samuori, a coined name from Sam (Samuel) and *ori*, Japanese for weaving. It is not an established
  Japanese word. Tagline: "Woven into one", shown small and muted.
- **Identifiers.** Binaries `samuori`, `samuori-server`, `samuori-worker`, `samuori-web`; environment prefix
  `SAMUORI_`; Go module `samuori.invalid/samuori`; API group `samuori.invalid/v1alpha1`; OpenAPI file
  `api/openapi/samuori.v1.yml` titled "Samuori API"; worker protocol package `samuori.worker.v1`; host paths
  `/etc/samuori` and `/var/lib/samuori*`; units `samuori-*.service`; image labels `io.samuori.*`; metrics
  `samuori_*`.
- **Logo.** The owner-approved vector files in `docs/brand/` are used unchanged: flat in app chrome and below 64 px,
  3D for large displays, the light or dark variant to match the surface. The UI loading indicators animate the mark's
  own six bands. The semantic colour tokens in `docs/brand/tokens.json` set the UI palette (ADR 0023).

## Provenance
The concept image is AI-generated; the vector files are a manual reconstruction of it. The wordmark's letters are
outlined from DejaVu Sans Bold, whose notice ships with the brand files and the UI. No exclusivity or copyright in the
logo is claimed beyond what the law gives. Preliminary searches found no software product with this exact name; they
are incomplete, and trademark, domain and package-name checks are the owner's to make before wider publication.
