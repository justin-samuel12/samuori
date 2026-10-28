---
title: Audit log and quarantines
section: Security review
summary: Read and export the audit log, review policies, and release quarantined images.
roles: [security-reviewer, administrator]
order: 120
---
## Audit log

**Security & Secrets > Observability & Audit** shows Samuori's operational state and its audit log, read from Samuori's own
database: sign-ins, role changes, grants, publications, approvals and administrative changes. Filter by action, for example
`publication.*`, and export events. Entries never contain secret values.

Retention is set in days: 0 keeps events forever; otherwise at least 30, and younger events are never purged.

## Quarantined images

Images built by a worker that was revoked as compromised are **quarantined**. They cannot be published until a
security reviewer or administrator reviews and releases them under **Security & Secrets > Quarantined images**.

