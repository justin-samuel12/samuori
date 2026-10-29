---
title: Build policies
section: Build supply
summary: Approve Dockerfile frontends, refuse risky build arguments, choose required checks and sensitive-content outcomes.
roles: [administrator]
order: 227
---
**Build supply > Build policies** holds four organization-wide policies. Base images are set under
**Build supply > Recipes & runtimes**, network access under **Security > Egress** and image downloads under
**Build supply > Registries & destinations**.

| Policy | What it decides |
|---|---|
| Dockerfile frontends | Which `# syntax=` frontends a Dockerfile may use. Each is pinned by digest, such as `docker.io/docker/dockerfile@sha256:` followed by 64 hex digits. Dockerfiles that name any other frontend are refused. |
| Build arguments | Rules that refuse a build argument whose name or value matches a pattern, with the reason developers see. |
| Validation | Whether a runtime test, an SBOM and a vulnerability scan are required, the severity that fails the scan, and vulnerability IDs to report as ignored. |
| Sensitive content | What happens when an image contains keys, credentials or database files: warn, require an exception or block. Add path rules for your own files. Secret bytes delivered to a build always block. |

## Change a policy

1. **Add**, **Edit** or **Remove** entries. Nothing is saved yet; the section says it has unsaved changes.
2. **Save** checks the change against every project's specs first. If it would block any of them, Samuori shows how
   many and asks for a reason of at least ten characters before applying it.
3. Each save becomes a new version. The section shows the current version, when it was saved and by whom, and the
   audit log keeps every change.

If someone else saved the policy since you opened the page, the save is refused; reload and make the change again.
