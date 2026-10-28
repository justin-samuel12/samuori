# 0006. One canonical wizard with conditional steps

Status: Accepted (2026-10-19) — owner decision

## Context
The original directive described the wizard twice: a 10-step list and an 8-step list with different boundaries.

## Decision
The single definition is in [`../product/developer-journey.md`](../product/developer-journey.md): seven steps plus a
Run details page. Steps and individual questions are conditional on detected/selected application type, the number
of candidate apps, available recipes and permitted destinations, and the user's Guided/Detailed preference.

| Superseded 10-step draft | Superseded 8-step draft | Canonical |
|---|---|---|
| 1 Choose source | 1 Upload source | 1 Upload source |
| 2 Discover | 2 Discover | 2 Discover & choose app |
| 3 Confirm app behavior | 3 Describe application | 3 Describe your app |
| 6 Runtime and dependencies | (part of 3/4) | 3 (Detailed mode fields) |
| 4 Select recipe | 4 Choose build recipe | 4 Choose how to build |
| 5 Configure build | 4 (advanced mode) | 4 (Detailed mode fields) |
| 7 Validation & security | (part of 5) | 5 Checks |
| 8 Destination & tags | 6 Choose publication intent | 6 Destination & tags |
| 9 Review | 5 Validate plan | 7 Review & build |
| — | 7 Run build | Submit action of step 7 |
| 10 Build results | 8 Results | Run details page |

Step 4 was renamed "Choose how to build" when existing Dockerfiles became a first-class build method (ADR 0028).

## Consequences
Every requirement from both drafts is preserved in the canonical text. Skipped steps display "Not needed for this
app" rather than disappearing silently.
