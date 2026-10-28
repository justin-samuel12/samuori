# 0056. Samuori is licensed under Apache-2.0

Status: **Accepted** (2026-10-27, owner decision).

## Context
Samuori had no outbound license (`docs/product/acceptance-criteria.md`, ADR 0022). The owner, Justin Samuel, is the sole
creator, architect and maintainer, runs the project without profit, and wants the attribution to stay visible
wherever Samuori is used.

Every line of Samuori is original: nothing is copied or ported from other projects, and the owner's earlier projects were
read only as references (ADR 0042). Linked dependencies are Apache-2.0, MIT, BSD, ISC and 0BSD, with BSD-only packages
of one mixed MPL-2.0/BSD module (`make license-check`); GPL tools are development-only or run as separate programs;
release archives bundle only Samuori's own binaries and UI.

## Options
- **Apache-2.0 (chosen).** Permissive; an explicit patent grant from contributors; redistributors must keep the
  `NOTICE` file, which carries the owner's attribution. Common in the container ecosystem and approved routinely by
  organizations.
- **MIT.** Simplest, but attribution is only the copyright line, and there is no patent grant.
- **AGPL-3.0 or GPL-3.0.** Keeps derivatives open, but many organizations forbid AGPL, which would hinder adoption,
  and with the DCO and no contributor agreement the license could not later change without every contributor.
- **MPL-2.0.** File-level copyleft; less familiar to adopters and no attribution mechanism beyond Apache's.
- **Non-commercial licenses.** Not open source; they would block organizational use.

## Decision
- Samuori is licensed under the Apache License, Version 2.0: `LICENSE` holds the unmodified license text, and `NOTICE`
  reads "Copyright 2026 Justin Samuel <jsamuel12@gmail.com>. Created, architected and maintained by Justin Samuel."
  Both ship in every release archive, beside `THIRD_PARTY_NOTICES.txt`, which the UI also serves and shows under
  **About Samuori → View third-party notices** (with the wordmark's font notice).
- Attribution also appears in the README, in the UI (account menu, **About Samuori**) and in the version output of
  `samuori`, `samuori-server`, `samuori-worker` and `samuori-web` (`internal/version.Credit`).
- Source files carry no per-file license headers (ADR 0020); `LICENSE` and `NOTICE` at the root cover them.
- Contributions are accepted under Apache-2.0 with the DCO (ADR 0022).

## Consequences
- Anyone may use, change and redistribute Samuori, including commercially, if they keep `LICENSE` and `NOTICE`. The
  license cannot require downstream copies to keep the UI credit; only the `NOTICE` text is mandatory.
- Ownership of code written as part of employment depends on that employment agreement, which the owner confirms
  before publishing.
