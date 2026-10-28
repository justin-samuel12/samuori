# Vision, Scope and Boundaries

Status: **Authoritative.**

## Product

Samuori (service and identifier prefix `samuori`, ADR 0057) is a focused, self-hosted container image factory.
Its defining experience: **a developer who knows their programming language but has no
container knowledge can upload a project and produce a validated, governed OCI image entirely in a web browser.**

A JavaScript developer can select a directory in a browser, see the detected stack with explainable
evidence/confidence, answer a few familiar questions (install/build/start scripts, HTTP port if needed),
preview/edit a portable image build specification, run an isolated build, review tests/scans/logs, and publish to a
repository an administrator permits. Dockerfiles and container terminology are optional, never prerequisites.
Python, PowerShell, .NET, Go, Java and further stack plugins follow the same workflow.

Design for background workers and CLIs as well as HTTP services. Do not assume every source is a web app or
Linux-compatible, that every Node.js app serves HTTP, or that PowerShell targets Linux.

## Roles

Each role is scoped and least-privilege; RBAC is evaluated server-side.

| Role | Responsibility |
|---|---|
| Developer | Projects and builds |
| Maintainer | Project policies |
| Publisher | Authorized release operations (publish/promote) |
| Operator | Workers, locations, queues |
| Security Reviewer | Findings and exceptions |
| Administrator | Global integrations and policies |

The UI progressively discloses expert controls and shows actionable, human-readable failures.

## In scope

Every feature must directly support one of: application discovery, image configuration, building, testing,
security, storage, publication, promotion, or administration of those functions.

## Out of scope (non-goals)

- A general-purpose developer portal, or full Backstage. Backstage is **not** a core dependency.
- A source-hosting / source-control product (snapshots and an optional future browser workspace only).
- An arbitrary CI engine or generic workflow engine (Git-driven *image* pipelines only: build/test/scan/sign/publish/promote).
- General application deployment, orchestration, or hosting of applications. Generating and validating Kubernetes
  deployment artifacts is in scope (ADR 0048), as are starting Compose files and Helm charts for a built image
  (ADR 0059) or for all of a project's images (ADR 0061); applying them, reconciling or provisioning clusters is not.
- An IDE, ITSM, or enterprise service catalog.
- Tag rollback is not execution rollback of a deployed workload.

Expanding scope requires a new written owner decision (ADR).

## Product principles

1. **Simple installation first.** All-in-one works without Kubernetes, Harbor, Vault/OpenBao, Keycloak, ECR, an
   external database or an external message server. Optional services enhance it; never install the whole
   ecosystem for a first-time user. (BuildKit *is* a required component of the build worker — see ADR 0013.)
2. **Integrate mature open source** behind capability-checked adapters rather than recreating functionality. No
   single external project's data format becomes the product's permanent public specification.
3. **No code-generation-as-validation.** User-created specs are type-checked, schema-validated, policy-evaluated,
   and test-built where appropriate. An image building is not proof the application works.
4. **Uploaded source is untrusted.** Never execute uploaded code or package scripts during discovery; build
   execution is isolated (see [`../architecture/security.md`](../architecture/security.md)).
5. **Honest UI.** Navigation never implies a capability that is only mocked. Unfinished functionality is labeled.
   Health is shown only after real connectivity tests. Skipped and blocked checks are never shown as passed.
6. **Accessibility and responsiveness are requirements**, not polish (WCAG 2.2 AA practical target).
7. **Replaceable components.** Samuori must remain valuable as a narrow image factory, not a wrapper requiring an
   enterprise platform.

## Independence and provenance

Samuori is a new, independent project. It is not a fork, port, translation or derivative of any prior project
(ADR 0042). In short:

- Use only public specifications, original designs, properly licensed open source, and independently created work.
- Do not copy, translate, import, port or derive code, UI components, internal specifications, configuration,
  tests, assets, schemas or documentation from prior related projects or any employer.
- Do not use employer datasets, credentials, cloud accounts, devices or infrastructure.
- Historical discussions may inform high-level functional goals but are not source material.
- Do not inspect prior codebases while implementing.

## Licensing intent

Apache-2.0 is the candidate license for original code, subject to the dependency license review.
Maintain SPDX identifiers, LICENSE, NOTICE where appropriate, dependency SBOM, attribution, contribution rules,
security disclosure policy and automated license compliance. A tool's website or "open source" label does not
establish compatibility: verify the pinned version's license and its transitives. See
[`../dependencies.md`](../dependencies.md).

## Release governance (for the first publication)

Public project metadata: Apache-2.0 candidate, NOTICE, SPDX SBOM, OSS dependency inventory, security disclosure
procedure, signed releases, changelog, semantic versioning, CODEOWNERS, PR rules, issue/feature-request templates,
documented support matrix and upgrade policy. Publication waits for the third-party license review.
