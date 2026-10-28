# Architecture Decision Records

Format: `NNNN-kebab-title.md` with sections Status, Context, Decision, Consequences. Statuses: **Proposed**,
**Accepted**, **Deferred**, **Superseded by NNNN**, **Rejected**. An accepted ADR is changed only by a new ADR that
supersedes it. Product decisions D01–D31 ([`../product/decisions.md`](../product/decisions.md)) change only with
owner approval.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-documentation-structure-and-authority.md) | Documentation structure and authority | Accepted |
| [0003](0003-separate-build-validation-publication-lifecycles.md) | Separate BuildRun, ValidationRun and Publication lifecycles | Accepted |
| [0004](0004-source-snapshot-is-buildrun-input.md) | Source snapshot is a BuildRun input, not part of BuildSpec | Accepted |
| [0005](0005-editor-modes-guided-detailed-yaml.md) | Editor modes: Guided / Detailed / YAML over one model | Accepted |
| [0006](0006-canonical-wizard.md) | One canonical wizard with conditional steps | Accepted |
| [0007](0007-canonical-navigation.md) | Canonical primary and administration navigation | Accepted |
| [0008](0008-api-group-placeholder.md) | Provisional API group and Go module path | Accepted (permanent names deferred) |
| [0009](0009-go-repository-layout.md) | Idiomatic Go repository layout | Accepted |
| [0010](0010-openapi-toolchain-and-generated-clients.md) | OpenAPI toolchain and generated client locations | Accepted |
| [0011](0011-toolchain-pins-and-package-manager.md) | Toolchain pins, package manager and task runner | Accepted |
| [0012](0012-node-roles-and-process-model.md) | Node roles, process model and configuration precedence | Accepted |
| [0013](0013-buildkit-default-engine-and-dev-environment.md) | BuildKit default engine; standalone requirements; macOS development | Accepted; isolation profiles approved (OD-4); Windows deferred (OD-7) |
| [0014](0014-build-plan-signing.md) | Build-plan integrity and the necessity of plan signing | Deferred |
| [0015](0015-secrets-providers-openbao-preferred.md) | Secrets providers: OpenBao preferred, Vault optional compatibility | Accepted |
| [0016](0016-observability-optional-external-stack.md) | Built-in observability; Grafana/Loki/Prometheus optional and external | Accepted |
| [0017](0017-persistence-and-messaging.md) | Persistence, migrations and messaging | Accepted |
| 0018 | Legal-status tracking and private information handling | Replaced by 0042 |
| [0019](0019-git-workflow-and-ci.md) | Git workflow, commits, pull requests and CI | Accepted |
| [0020](0020-code-comment-standard.md) | Code comment standard | Accepted |
| [0021](0021-image-signing-and-scanner-providers.md) | Image signing and vulnerability scanner providers | Accepted (signing default tentative) |
| [0022](0022-dco-and-no-ai-attribution.md) | DCO sign-off and no AI attribution in Git metadata | Accepted |
| [0023](0023-ui-component-library.md) | UI component library: React Aria Components with an original design system | Accepted |
| [0024](0024-untrusted-content-placement.md) | Placement of untrusted-content processing, messaging and publication | Accepted as direction |
| [0025](0025-build-network-egress.md) | Build network egress: policy-enforcing proxy with ecosystem presets | Accepted as direction |
| [0026](0026-worker-identity-lifecycle.md) | Worker and controller identity lifecycle | Accepted as direction |
| [0027](0027-isolation-providers-and-vm-lifecycle.md) | Isolation providers and build VM lifecycle | Accepted as direction |
| [0028](0028-build-methods-and-dockerfile-support.md) | Build methods: generated recipes and first-class Dockerfiles | Accepted as direction |
| [0029](0029-build-secrets-and-bindings.md) | Build secrets: logical IDs, project-scoped bindings and session-scoped delivery | Accepted as direction, with conditions |
| [0030](0030-registry-authentication.md) | Registry authentication: build-time pulls and controller-only publication | Accepted as direction |
| [0031](0031-sensitive-build-outputs.md) | Sensitive build outputs | Accepted as direction, with conditions |
| [0032](0032-all-in-one-install-shapes.md) | All-in-one install shapes: native systemd and Compose with a host worker | Accepted with conditions |
| [0033](0033-git-pipelines-and-webhooks.md) | Git-triggered image pipelines and inbound webhooks | Accepted as direction |
| [0034](0034-controller-high-availability.md) | Controller high availability | Accepted |
| [0035](0035-backup-and-restore.md) | Backup and restore | Accepted |
| [0036](0036-version-matrix-and-upgrades.md) | Version matrix and upgrades | Accepted |
| [0037](0037-controller-federation.md) | Controller federation | Accepted |
| [0038](0038-vm-per-job-cloud-hypervisor.md) | vm-per-job with Cloud Hypervisor | Accepted; not validated |
| [0039](0039-test-code-removed.md) | Test code removed by owner directive | Accepted |
| [0040](0040-uploader-deletes-uploads.md) | The uploader may delete an upload | Accepted |
| [0041](0041-custom-recipes.md) | Custom recipes | Accepted |
| [0042](0042-no-legal-status-tracking.md) | No legal-status tracking; private information handling | Accepted |
| [0043](0043-database-password-handover.md) | Database password handover from the UI | Accepted |
| [0044](0044-self-update.md) | Self-update from the portal | Accepted |
| [0045](0045-cluster-lifecycle.md) | Cluster lifecycle: first node, joining, updates and removal | Accepted |
| [0046](0046-topology-plan-and-conversion.md) | Topology plan, resilience tier and conversion | Accepted |
| [0047](0047-build-strategies-railpack-and-buildpacks.md) | Build strategies: Railpack and Cloud Native Buildpacks | Accepted |
| [0048](0048-kubernetes-artifact-generation.md) | Kubernetes artifact generation: Kompose and Move2Kube | Accepted |
| [0049](0049-base-image-tag-pinning.md) | Base-image tag pinning at admission | Accepted |
| [0050](0050-build-readiness-prediction.md) | Build readiness prediction | Accepted |
| [0051](0051-organization-runtimes.md) | Organization runtimes for built-in recipes | Accepted |
| [0052](0052-project-egress-allowances.md) | Project egress allowances | Accepted (provisional) |
| [0053](0053-image-download-and-build-deletion.md) | Downloading images and deleting builds | Accepted (provisional) |
| [0054](0054-default-image-tags.md) | Default image tags: the build date and a daily counter | Accepted (provisional) |
| [0055](0055-frontend-and-backend-in-one-image.md) | A frontend and a backend in one image | Accepted (provisional) |
| [0056](0056-apache-2-license.md) | Samuori is licensed under Apache-2.0 | Accepted |
| [0057](0057-samuori-name-and-brand.md) | The product is named Samuori | Accepted |
| [0058](0058-archiving-restoring-and-purging-projects.md) | Archiving, restoring and purging projects | Accepted (provisional) |
| [0059](0059-deployment-files-for-built-images.md) | Deployment files for built images | Accepted (provisional) |
| [0060](0060-deprecating-and-withdrawing-built-in-runtimes.md) | Deprecating and withdrawing built-in runtimes | Accepted (provisional) |
| [0061](0061-deployment-files-for-a-project.md) | Deployment files for a project | Accepted (provisional) |
| [0062](0062-editing-withdrawing-and-deleting.md) | Renaming, withdrawing requests and deleting pipelines and conversions | Accepted (provisional) |
| [0064](0064-editing-secret-grants.md) | Editing secret grants | Accepted (provisional) |
