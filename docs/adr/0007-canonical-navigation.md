# 0007. Canonical primary and administration navigation

Status: Accepted (2026-10-19) — owner decision

## Context
Two different administration section lists existed (e.g. "Databases" vs "Database", "Engines/Recipes/Base Images" vs
separate entries, "AWS & Registries" vs "Registries/ECR", "Artifact Storage/Cache" vs "Artifact Storage").

## Decision
Primary navigation: Home, Projects, New Image, Build Runs, Image Library, Approvals, Account. The administration
surface uses the grouped list in [`../product/administration.md`](../product/administration.md).

| Superseded names | Canonical section |
|---|---|
| Overview | Overview |
| Projects/Teams; Organization/Teams | Teams & Projects |
| Users/Identity | Users & Identity |
| Build Queue; Queue | Build Queue |
| Nodes/Workers; Workers | Nodes & Workers |
| Locations/Peering; Locations | Worker Pools & Locations |
| Engines/Recipes/Base Images; Build Engines; Recipes | Build Engines; Recipes & Base Images |
| Artifact Storage/Cache; Artifact Storage | Artifact Storage; Build Cache |
| AWS & Registries; Registries/ECR | Registries & ECR |
| Secrets; Credentials/Secrets | Credentials & Secrets |
| Certificates/TLS; Certificates | Certificates & TLS |
| Validation/Security Policies; Policies | Policies |
| Databases; Database | Database |
| Messaging | Messaging |
| Notifications | Notifications |
| Observability/Audits; Audit | Observability & Audit |
| Backup/Restore | Backup & Restore |
| Upgrades/Diagnostics; Upgrades | Upgrades & Diagnostics |
| (new, ADR 0045) | Servers |

## Consequences
Route paths, permission names and docs use the canonical names. Navigation visibility is a UX convenience only; the
API authorizes every request.
