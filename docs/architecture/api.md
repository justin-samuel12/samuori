# API Design (`/api/v1`)

Status: **M0 skeleton** (2026-10-19). Contract: `api/openapi/samuori.v1.yml` (OpenAPI 3.0.3, ADR 0010). Checked by
`tools/apicheck` (see "Automated checks"). The running server publishes it (ADR 0066); a guide for callers is
[developers/api.md](../developers/api.md).

## Principles

- **Separate resources, never one build object.** Each of these is its own resource with its own lifecycle and
  authorization:
  - specs and revisions;
  - source snapshots;
  - build runs and attempts;
  - artifacts;
  - validation runs;
  - publications;
  - approvals and exceptions;
  - workers and pools;
  - secret providers and bindings;
  - registries, credential profiles, destinations, image sources and mirrors;
  - policies;
  - audit events.
- **The BuildSpec document** is carried as an opaque object and validated server-side against
  `api/schemas/buildspec.v1alpha1.json`, which OpenAPI 3.0 cannot express fully.
- **Developers see logical secrets, admins see bindings.** Developers see `SecretReadiness` (ID, required, type,
  mount target, ready / missing / not-permitted). Only administrators see `SecretBinding`, including provider
  references. Nobody ever sees values.
- **No credentials in responses.**
  - Credential inputs are `writeOnly`: the login password, local secret values and provider bootstrap credentials.
  - Registry credentials are secret *references*.
  - Two deliberate show-once exceptions, marked `x-samuori-reveal-once`: `ApiTokenIssued.token` and
    `EnrollmentTokenIssued.token`. Both are returned once at creation and stored only as hashes.
- **Enrollment is not trust.**
  - `Worker` reports `enrollmentState`, `operationalState` and `eligibility.untrustedBuilds` with reasons.
  - A worker is eligible for uploaded source only when its pool's isolation profile is approved and the worker
    passed its own recent self-test for that profile and its current configuration (OD-22).
  - `daemonSlots` exposes one-build-per-daemon capacity (OD-16).
- **The worker execution protocol** (leases, heartbeats, plan fetch, secret release, artifact commit) is a separate
  mTLS channel and is **not** part of this API. Worker identities are rejected by every endpoint here (ADR 0024).
  Its transport is OD-20.
- **Authorization is per operation.** `x-samuori-roles` lists the roles that may call it. The server also checks
  project, team and organization scope on every call (D30). Hidden navigation is not authorization.
- **The UI hides what the roles do not permit, from the same declarations.** `make generate` runs `tools/genroles`,
  which writes `ui/src/api/generated/roles.ts` (operationId → `"public"`, `"any"` or the role list) from
  `x-samuori-roles` and `x-samuori-public`; the UI keeps no role lists of its own. `useCan()` / `can(op, scope)`
  (`ui/src/session/can.ts`) mirrors the server's coarse check (`declaredRoles` and `authz.HasAnyRole` in
  `internal/api/middleware.go`: one of the roles at any scope) and, where a page knows the project and its team,
  narrows it as `authz.Can` does (an organization binding, or a team or project binding on that team or project).
  Navigation entries list their main operations and appear when one is allowed; New image, Upload source, Build,
  Build again, Cancel, Publish, Promote, Deprecate, Retire, request and approval actions appear only when allowed;
  project-page sections whose list call answers 403 render nothing. Administration groups keep their
  organization-scope rule. This only avoids offering what would be refused: **the server still authorizes every
  call**, and scope checks the UI cannot know (for example a build in another team's project) still answer 403 or
  404. Tested by `TestRender` and `TestDeclaredSpec` (`tools/genroles`) and `ui/src/session/can.test.ts`.
- **Publication is re-authorized when it runs** (`gate_check`). `PublicationRequest` carries no credentials.

## Conventions

| Concern | Rule |
|---|---|
| IDs | ULIDs (`Id` schema) |
| Pagination | `cursor` and `limit` (1–200, default 50); responses carry `nextCursor`; lists paginated on 2026-10-28 return every item without `limit` or `cursor`; bounded lists are exempt (`TestContractConsistency`) |
| Errors | `Problem` (`code`, `message`, `details[]`, `requestId`) for every documented non-2xx; `429` declares `Retry-After`; cross-scope reads return 404 |
| Concurrency | `ETag` on specs and policies; `If-Match` is required to change them (412 on mismatch) |
| Idempotency | `Idempotency-Key` header, required on build, cancel, publication, approval decision, upload commit, enrollment token, secret binding and worker action operations; optional on every other create, where the shared operations record answers a repeat with `409 idempotent_replay` (`replayCreates`) |
| Async work | `202` with an `Operation` resource |
| Browser auth | `samuori_session` cookie, plus `X-CSRF-Token` on unsafe methods |
| API clients | Bearer tokens (scoped, expiring API tokens or OIDC) |
| Streaming | Build events over server-sent events with `Last-Event-ID` resume |
| Contract | Served at `/openapi.yml` and `/openapi.json` to any signed-in caller, with `ETag` and private caching (ADR 0066) |
| Deprecation | `deprecated: true` with `x-samuori-deprecated` and optional `x-samuori-sunset` dates; the guard adds `Deprecation` and `Sunset` headers and refuses to start on a missing date or under 30 days' notice (ADR 0066, `docs/developers/api-changelog.md`) |

## Automated checks (`tools/apicheck`)

1. OpenAPI 3.0.3 validity, including validation of every example (kin-openapi).
2. Every operation has an `operationId`.
3. Non-public operations declare `x-samuori-roles` and never disable authentication. Public operations
   (`getHealth`, `login`) must say so explicitly.
4. Unsafe methods declare `X-CSRF-Token`.
5. The listed state-changing operations declare `Idempotency-Key`.
6. Credential-like fields are `writeOnly`, except approved reveal-once fields in approved schemas.
7. No worker-protocol paths (lease, heartbeat, secret release, plan fetch).
8. `x-samuori-signed` (public, but authenticated by a signature, so `401` means unverified) appears only on approved
   operations (`receivePipelineHook`).

The checker's own negative tests prove each rule fires. See `docs/developers/testing.md`.

`TestContractConsistency` (`internal/api`) walks the contract and fails when a list lacks `limit`, `cursor` or
`nextCursor`, a non-2xx response does not use `Problem`, a `429` lacks `Retry-After`, or a create lacks
`Idempotency-Key`. Its allowlists name each exception with a reason; stale entries fail too.

## Not in this skeleton yet

These domains from `domain-model.md` are deferred to the milestone that builds them. They follow the same
conventions:
- organizations (single organization first);
- recipes and base-image policies (M3);
- the image library listing `/images` (M6);
- locations and controller peers (M7, M11);
- storage profiles (M5);
- backups and upgrades (M11).

Promotion is a `Publication` with `kind: promote`, not a separate resource.

## Endpoints

| Area | Method | Path | Operation | Roles |
|---|---|---|---|---|
| system | GET | `/system/health` | `getHealth` | public |
| system | GET | `/openapi.yml` | `getApiContract` | any (ADR 0066) |
| system | GET | `/openapi.json` | `getApiContractJson` | any (ADR 0066) |
| system | GET | `/system/diagnostics` | `getDiagnostics` | administrator, operator |
| system | GET | `/build-caches` | `listBuildCaches` | administrator, operator |
| system | POST | `/build-caches/{scope}/purge` | `purgeBuildCache` | administrator |
| system | GET | `/operations/{operationId}` | `getOperation` | any |
| identity | POST | `/auth/login` | `login` | public |
| identity | POST | `/auth/logout` | `logout` | any |
| identity | GET | `/auth/session` | `getSession` | any, browser session only |
| identity | POST | `/auth/session/extend` | `extendSession` | any, browser session only |
| identity | POST | `/auth/session/renewal` | `renewSession` | any person, browser session only; re-authenticates the session's own user into a new session (ADR 0068) |
| identity | GET | `/auth/providers` | `listSignInProviders` | public |
| identity | GET | `/auth/oidc/{providerId}/start` | `startOidcSignIn` | public |
| identity | GET | `/auth/oidc/callback` | `finishOidcSignIn` | public |
| identity | GET | `/identity-providers` | `listIdentityProviders` | administrator |
| identity | POST | `/identity-providers` | `createIdentityProvider` | administrator |
| identity | POST | `/identity-providers/{providerId}/state` | `setIdentityProviderState` | administrator |
| identity | DELETE | `/identity-providers/{providerId}` | `deleteIdentityProvider` | administrator (409 `provider_in_use` once anyone signed in through it) |
| identity | POST | `/identity-providers/{providerId}/test` | `testIdentityProvider` | administrator |
| identity | GET | `/identity-providers/{providerId}/group-mappings` | `listGroupMappings` | administrator |
| identity | POST | `/identity-providers/{providerId}/group-mappings` | `createGroupMapping` | administrator |
| identity | DELETE | `/identity-providers/{providerId}/group-mappings/{mappingId}` | `deleteGroupMapping` | administrator |
| identity | GET | `/identity-settings` | `getIdentitySettings` | administrator |
| identity | PUT | `/identity-settings` | `setIdentitySettings` | administrator |
| identity | GET | `/service-identities` | `listServiceIdentities` | administrator |
| identity | POST | `/service-identities` | `createServiceIdentity` | administrator |
| identity | POST | `/service-identities/{identityId}/state` | `setServiceIdentityState` | administrator |
| identity | PATCH | `/service-identities/{identityId}` | `updateServiceIdentity` | administrator (display name only) |
| identity | POST | `/service-identities/{identityId}/api-tokens` | `createServiceToken` | administrator |
| identity | DELETE | `/service-identities/{identityId}/api-tokens/{tokenId}` | `revokeServiceToken` | administrator |
| identity | GET | `/me` | `getMe` | any |
| identity | PATCH | `/me` | `updateMe` | any person (own display name) |
| identity | POST | `/me/password` | `changePassword` | any person with a local password, browser session only (ADR 0065) |
| identity | GET | `/me/api-tokens` | `listApiTokens` | any |
| identity | POST | `/me/api-tokens` | `createApiToken` | any |
| tenancy | GET | `/teams` | `listTeams` | any |
| tenancy | PATCH | `/teams/{teamId}` | `updateTeam` | administrator (organization) |
| tenancy | DELETE | `/teams/{teamId}` | `deleteTeam` | administrator (organization; 409 `team_in_use`) |
| identity | PATCH | `/users/{userId}` | `updateUser` | administrator (organization) |
| identity | POST | `/users/{userId}/state` | `setUserState` | administrator (organization; ADR 0065) |
| identity | POST | `/users/{userId}/password-reset` | `resetUserPassword` | administrator (organization; local users; ADR 0065) |
| identity | PATCH | `/role-bindings/{bindingId}` | `changeRoleBinding` | administrator (organization) |
| tenancy | GET | `/projects` | `listProjects` | developer, maintainer, publisher, security-reviewer, administrator |
| tenancy | POST | `/projects` | `createProject` | maintainer, administrator |
| tenancy | GET | `/projects/{projectId}` | `getProject` | developer, maintainer, publisher, security-reviewer, administrator |
| tenancy | PATCH | `/projects/{projectId}` | `updateProject` (name, publication mode; never the team) | maintainer, administrator (of the project's team; audited) |
| tenancy | POST | `/projects/{projectId}/archive` | `archiveProject` | maintainer, administrator (of the project; ADR 0058) |
| tenancy | POST | `/projects/{projectId}/restore` | `restoreProject` | maintainer, administrator (of the project; ADR 0058) |
| tenancy | POST | `/projects/{projectId}/purge` | `purgeProject` | administrator (organization; `X-Samuori-Confirm` = project name; ADR 0058) |
| sources | POST | `/projects/{projectId}/uploads` | `createUploadSession` | developer, maintainer |
| sources | PUT | `/uploads/{uploadId}/chunks/{index}` | `putUploadChunk` | developer, maintainer |
| sources | POST | `/uploads/{uploadId}/commit` | `commitUpload` | developer, maintainer |
| sources | GET | `/projects/{projectId}/snapshots` | `listSnapshots` | developer, maintainer, security-reviewer, administrator |
| sources | GET | `/snapshots/{snapshotId}` | `getSnapshot` | developer, maintainer, security-reviewer, administrator |
| discovery | POST | `/snapshots/{snapshotId}/discoveries` | `startDiscovery` | developer, maintainer |
| discovery | GET | `/discoveries/{discoveryId}` | `getDiscovery` | developer, maintainer |
| discovery | GET | `/snapshots/{snapshotId}/spec-suggestions` | `listSpecSuggestions` | developer, maintainer ([suggested specs](buildspec.md#suggested-specs)) |
| specs | GET | `/projects/{projectId}/specs` | `listSpecs` | developer, maintainer, publisher, administrator |
| specs | POST | `/projects/{projectId}/specs` | `createSpec` | developer, maintainer |
| specs | GET | `/specs/{specId}` | `getSpec` | developer, maintainer, publisher, administrator |
| specs | GET | `/specs/{specId}/revisions` | `listSpecRevisions` | developer, maintainer, publisher, administrator |
| specs | POST | `/specs/{specId}/revisions` | `createSpecRevision` | developer, maintainer |
| specs | GET | `/specs/{specId}/revisions/{revision}` | `getSpecRevision` | developer, maintainer, publisher, administrator |
| specs | POST | `/specs/{specId}/revisions/{revision}/admission-checks` | `checkAdmission` | developer, maintainer |
| recipes | GET | `/recipes` | `listRecipes` | developer, maintainer, publisher, administrator (withdrawn versions and runtimes: administrators only) |
| recipes | PUT | `/recipes/{recipeId}/versions/{version}/status` | `setRecipeStatus` | administrator (organization) |
| recipes | PUT | `/recipes/{recipeId}/versions/{version}/shipped-runtimes/{label}/status` | `setShippedRuntimeStatus` | administrator (organization; ADR 0060) |
| builds | GET | `/builds` | `listBuilds` | developer, maintainer, publisher, operator, security-reviewer, administrator |
| builds | POST | `/builds` | `createBuild` | developer, maintainer |
| builds | GET | `/builds/{buildId}` | `getBuild` | developer, maintainer, publisher, operator, security-reviewer, administrator |
| builds | POST | `/builds/{buildId}/cancel` | `cancelBuild` | developer, maintainer, operator, administrator |
| builds | GET | `/builds/{buildId}/attempts` | `listBuildAttempts` | developer, maintainer, operator, security-reviewer, administrator |
| builds | GET | `/builds/{buildId}/events` | `streamBuildEvents` | developer, maintainer, operator, security-reviewer, administrator |
| builds | GET | `/builds/{buildId}/logs` | `getBuildLogs` | developer, maintainer, operator, security-reviewer, administrator |
| artifacts | GET | `/artifacts/{artifactId}` | `getArtifact` | developer, maintainer, publisher, security-reviewer, administrator |
| artifacts | GET | `/artifacts/{artifactId}/deploy` | `getDeploymentFiles` | developer, maintainer, publisher, security-reviewer, administrator (project visibility applies; ADR 0059) |
| artifacts | GET | `/artifacts/{artifactId}/deploy/compose` | `downloadDeploymentCompose` | developer, maintainer, publisher, security-reviewer, administrator (project visibility applies; audited) |
| artifacts | GET | `/artifacts/{artifactId}/deploy/helm` | `downloadDeploymentHelmChart` | developer, maintainer, publisher, security-reviewer, administrator (project visibility applies; audited) |
| artifacts | GET | `/projects/{projectId}/deploy` | `getProjectDeploymentFiles` | developer, maintainer, publisher, security-reviewer, administrator (project visibility applies; ADR 0061) |
| artifacts | GET | `/projects/{projectId}/deploy/compose` | `downloadProjectDeploymentCompose` | developer, maintainer, publisher, security-reviewer, administrator (project visibility applies; audited) |
| artifacts | GET | `/projects/{projectId}/deploy/helm` | `downloadProjectDeploymentHelmChart` | developer, maintainer, publisher, security-reviewer, administrator (project visibility applies; audited) |
| validation | GET | `/artifacts/{artifactId}/validations` | `listValidations` | developer, maintainer, publisher, security-reviewer, administrator |
| validation | POST | `/artifacts/{artifactId}/validations` | `startRevalidation` | maintainer, security-reviewer, administrator |
| validation | GET | `/validations/{validationId}` | `getValidation` | developer, maintainer, publisher, security-reviewer, administrator |
| publication | GET | `/projects/{projectId}/destinations` | `listProjectDestinations` | developer, maintainer, publisher |
| publication | GET | `/publications` | `listPublications` | developer, maintainer, publisher, security-reviewer, administrator |
| publication | POST | `/publications` | `requestPublication` (publish, promote or rollback) | publisher, maintainer |
| publication | GET | `/publications/{publicationId}` | `getPublication` | developer, maintainer, publisher, security-reviewer, administrator |
| publication | POST | `/publications/{publicationId}/deprecation` | `setPublicationDeprecation` | publisher, maintainer |
| publication | POST | `/publications/{publicationId}/retire` | `retirePublication` | publisher, maintainer |
| approvals | GET | `/approvals` | `listApprovals` | publisher, security-reviewer, administrator |
| approvals | POST | `/approvals/{approvalId}/decision` | `decideApproval` | publisher, security-reviewer, administrator |
| approvals | POST | `/approvals/{approvalId}/withdrawal` | `withdrawApproval` | the requester (developer, maintainer, administrator), or a maintainer or administrator of the project; pending repository, base-image, egress-access and secret-binding requests only (ADR 0062) |
| validation | GET | `/projects/{projectId}/output-exceptions` | `listOutputExceptions` | developer, maintainer, publisher, security-reviewer, administrator |
| validation | POST | `/projects/{projectId}/output-exceptions` | `requestOutputException` | maintainer, administrator |
| validation | GET | `/output-exceptions` | `listPendingOutputExceptions` | administrator |
| validation | POST | `/output-exceptions/{exceptionId}/decision` | `decideOutputException` | administrator (not the requester) |
| validation | POST | `/output-exceptions/{exceptionId}/revoke` | `revokeOutputException` | administrator |
| publication | GET | `/signing-keys` | `listSigningKeys` | every role |
| publication | POST | `/signing-keys` | `createSigningKey` | administrator |
| publication | POST | `/signing-keys/{keyId}/retire` | `retireSigningKey` | administrator |
| workers | GET | `/workers` | `listWorkers` | operator, administrator |
| workers | GET | `/workers/{workerId}` | `getWorker` | operator, administrator |
| workers | POST | `/workers/{workerId}/actions` | `applyWorkerAction` | operator, administrator |
| workers | GET | `/worker-pools` | `listWorkerPools` | operator, administrator |
| workers | POST | `/worker-pools` | `createWorkerPool` | administrator |
| workers | POST | `/worker-pools/{poolId}/self-tests` | `runPoolSelfTest` | operator, administrator |
| workers | POST | `/workers/{workerId}/self-tests` | `runWorkerSelfTest` | operator, administrator |
| workers | PATCH | `/worker-pools/{poolId}` | `updateWorkerPool` | administrator |
| workers | DELETE | `/worker-pools/{poolId}` | `deleteWorkerPool` | administrator |
| workers | GET | `/locations` | `listLocations` | every role |
| workers | POST | `/locations` | `createLocation` | administrator |
| workers | DELETE | `/locations/{locationName}` | `deleteLocation` | administrator |
| system | DELETE | `/federation/routes/{routeId}` | `deleteFederationRoute` | administrator |
| workers | GET | `/projects/{projectId}/locations` | `getProjectLocations` | every role (project visibility applies) |
| workers | PUT | `/projects/{projectId}/locations` | `setProjectLocations` | administrator |
| workers | GET | `/pki` | `getTrustBundle` | operator, administrator |
| workers | POST | `/pki/rotations` | `startCaRotation` | administrator |
| workers | POST | `/pki/rotations/switch` | `switchCaRotation` | administrator |
| artifacts | GET | `/artifact-quarantines` | `listArtifactQuarantines` | security-reviewer, administrator |
| artifacts | POST | `/artifact-quarantines/{quarantineId}/release` | `releaseArtifactQuarantine` | security-reviewer, administrator |
| workers | POST | `/enrollment-tokens` | `createEnrollmentToken` | administrator |
| workers | GET | `/enrollment-tokens` | `listEnrollmentTokens` | administrator |
| workers | DELETE | `/enrollment-tokens/{tokenId}` | `revokeEnrollmentToken` | administrator |
| secrets | GET | `/secret-providers` | `listSecretProviders` | administrator |
| secrets | POST | `/secret-providers` | `createSecretProvider` | administrator |
| secrets | POST | `/secret-providers/{providerId}/test` | `testSecretProvider` | administrator |
| secrets | POST | `/secret-providers/{providerId}/local-secrets` | `storeLocalSecret` | administrator |
| secrets | GET | `/projects/{projectId}/secret-bindings` | `listSecretBindings` | administrator |
| secrets | POST | `/projects/{projectId}/secret-bindings` | `createSecretBinding` | administrator |
| secrets | POST | `/secret-bindings/{bindingId}/actions` | `applySecretBindingAction` | administrator |
| secrets | POST | `/projects/{projectId}/secret-requests` | `requestSecretBinding` | developer, maintainer |
| secrets | PUT | `/projects/{projectId}/secret-grants/{grantId}` | `updateSecretGrant` | administrator (bindings the narrower grant no longer covers are disabled; ADR 0064) |
| secrets | PUT | `/allowed-secrets/{allowedSecretId}` | `updateAllowedSecret` | administrator |
| registries | GET | `/registries` | `listRegistries` | administrator |
| registries | POST | `/registries` | `createRegistry` | administrator |
| registries | GET | `/registry-credential-profiles` | `listRegistryCredentialProfiles` | administrator |
| registries | POST | `/registry-credential-profiles` | `createRegistryCredentialProfile` | administrator |
| registries | POST | `/registry-credential-profiles/{profileId}/test` | `testRegistryCredentialProfile` | administrator |
| registries | GET | `/destinations` | `listDestinations` | administrator |
| registries | POST | `/destinations` | `createDestination` | administrator |
| registries | GET | `/registries/{registryId}/repositories` | `listEcrRepositories` | administrator |
| registries | POST | `/registries/{registryId}/repositories` | `createEcrRepository` | administrator |
| registries | GET | `/image-sources` | `listImageSources` | developer, maintainer, publisher, security-reviewer, administrator |
| registries | POST | `/image-sources` | `createImageSource` | administrator |
| registries | PUT | `/image-sources/{sourceId}` | `updateImageSource` | administrator |
| registries | DELETE | `/image-sources/{sourceId}` | `deleteImageSource` | administrator (409 while an unfinished build of its teams plans to pull from its upstream) |
| registries | POST | `/image-sources/{sourceId}/state` | `setImageSourceState` | administrator |
| registries | GET | `/image-sources/{sourceId}/mirror-config` | `getImageSourceMirrorConfig` | administrator |
| registries | POST | `/mirror-token-keys/rotations` | `rotateMirrorTokenKey` | administrator |
| policies | GET | `/policies` | `listPolicies` | administrator, security-reviewer |
| policies | PUT | `/policies/{policyId}` | `replacePolicy` | administrator |
| policies | POST | `/policies/{policyId}/dry-run` | `dryRunPolicy` | administrator |
| policies | GET | `/projects/{projectId}/base-image-requests` | `listBaseImageRequests` | developer, maintainer, publisher, administrator |
| policies | POST | `/projects/{projectId}/base-image-requests` | `requestBaseImage` | developer, maintainer |
| policies | GET | `/projects/{projectId}/egress-access-requests` | `listEgressAccessRequests` | developer, maintainer, publisher, administrator |
| policies | POST | `/projects/{projectId}/egress-access-requests` | `requestEgressAccess` | developer, maintainer |
| policies | GET | `/projects/{projectId}/egress-allowances` | `listEgressAllowances` | developer, maintainer, publisher, administrator |
| policies | POST | `/egress-allowances/{allowanceId}/revocation` | `revokeEgressAllowance` | administrator (organization) |
| recipes | PUT | `/custom-recipes/{recipeId}/versions/{version}` | `replaceCustomRecipeDraft` | administrator (organization; drafts only, otherwise 409) |
| recipes | DELETE | `/custom-recipes/{recipeId}/versions/{version}` | `discardCustomRecipeDraft` | administrator (organization; drafts only, otherwise 409; audited `recipe.discard`) |
| registries | GET | `/projects/{projectId}/repository-requests` | `listRepositoryRequests` | developer, maintainer, publisher, administrator |
| registries | POST | `/projects/{projectId}/repository-requests` | `requestRepository` | developer, maintainer |
| registries | POST | `/approvals/{approvalId}/fulfillment` | `fulfillRepositoryRequest` | administrator |
| pipelines | GET | `/pipelines` | `listPipelines` | any role that can see the project |
| pipelines | POST | `/pipelines` | `createPipeline` | administrator |
| pipelines | GET | `/pipelines/{pipelineId}` | `getPipeline` | any role that can see the project |
| pipelines | PATCH | `/pipelines/{pipelineId}` | `updatePipeline` (token write-only; webhook URL and secret kept) | administrator |
| pipelines | DELETE | `/pipelines/{pipelineId}` | `deletePipeline` (409 while a run is queued or fetching; runs kept; ADR 0062) | administrator |
| pipelines | POST | `/pipelines/{pipelineId}/state` | `setPipelineState` | administrator |
| pipelines | POST | `/pipelines/{pipelineId}/hook-secret` | `rotatePipelineHookSecret` | administrator |
| pipelines | GET | `/pipelines/{pipelineId}/runs` | `listPipelineRuns` | any role that can see the project |
| pipelines | POST | `/pipelines/{pipelineId}/runs` | `startPipelineRun` | developer, maintainer |
| pipelines | POST | `/hooks/pipelines/{pipelineId}` | `receivePipelineHook` | public, signed |
| kubernetes | PATCH | `/kube-conversions/{conversionId}` | `updateKubeConversion` (rename) | developer, maintainer, administrator |
| kubernetes | DELETE | `/kube-conversions/{conversionId}` | `deleteKubeConversion` (409 while a run is active) | its creator, or a maintainer or administrator of the project |
| audit | GET | `/audit-events` | `listAuditEvents` | administrator, security-reviewer |
| audit | GET | `/audit-events/export` | `exportAuditEvents` | administrator, security-reviewer |
| audit | GET | `/audit-policy` | `getAuditPolicy` | administrator, security-reviewer |
| audit | PUT | `/audit-policy` | `setAuditPolicy` | administrator |
| configuration | GET | `/configuration/export` | `exportConfiguration` | administrator; YAML or JSON, no secrets, audited (ADR 0069) |
| configuration | POST | `/configuration/import` | `importConfiguration` | administrator; `dryRun` (default true) answers the plan, `dryRun=false` with `Idempotency-Key` applies it (ADR 0069) |
| notifications | GET | `/notifications` | `listNotifications` | any (own inbox) |
| notifications | POST | `/notifications/read` | `markNotificationsRead` | any (own inbox) |
| notifications | GET | `/notification-channels` | `listNotificationChannels` | administrator |
| notifications | POST | `/notification-channels` | `createNotificationChannel` | administrator |
| notifications | POST | `/notification-channels/{channelId}/state` | `setNotificationChannelState` | administrator |
| notifications | DELETE | `/notification-channels/{channelId}` | `deleteNotificationChannel` | administrator |
| notifications | POST | `/notification-channels/{channelId}/test` | `testNotificationChannel` | administrator |
| notifications | GET | `/notification-rules` | `listNotificationRules` | administrator |
| notifications | POST | `/notification-rules` | `createNotificationRule` | administrator |
| notifications | DELETE | `/notification-rules/{ruleId}` | `deleteNotificationRule` | administrator |
| notifications | GET | `/notification-deliveries` | `listNotificationDeliveries` | administrator |
| notifications | POST | `/notification-deliveries/{deliveryId}/retry` | `retryNotificationDelivery` | administrator |
