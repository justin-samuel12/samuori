# Validation

Status: **Implemented for the `rootless-host` provider (WP20).** Lifecycle: [`build-lifecycle.md`](build-lifecycle.md)
(ADR 0003). Sensitive outputs: ADR 0031 and [`secrets.md`](secrets.md#sensitive-build-outputs). Threat rows: TM-45,
TM-84, TM-85.

A ValidationRun targets an image artifact digest. The first one is created in the same transaction that marks its
BuildRun succeeded; rescans create new runs. A run moves `pending → running → completed` (or `timed_out`,
`errored`) and ends with a verdict.

## Checks

| Check | Category | Runs on | Required | What it does |
|---|---|---|---|---|
| `runtime-<profile>` | `test` | worker, in the build environment | when the plan has a profile and `validation.requireRuntimeTest` (default on) | Starts the image and checks it (below) |
| `build-secret-canary` | `sensitive-content` | worker, before secret values are discarded | whenever the plan binds a secret | Searches every layer and the image config for the exact bytes of each delivered value (supplementary; cannot see transformed values) |
| `sensitive-content` | `sensitive-content` | controller, restricted subprocess | always | Path rules, content detectors and image metadata (below) |
| `sbom` | `sbom` | controller, Syft with no network | when the spec asks (`outputs.sbom`) or `validation.requireSbom` | SPDX JSON stored in internal storage; the check records the tool version and the document digest |

The plan decides the runtime profile: the spec's `validation.profile`, else one implied by the application type
(`http-service` → `http-startup`, `static-site` → `static-serve`, `worker` and `database` → `process-start`,
`cli-task` → `cli-smoke`). The port is `application.port` (or the image's first exposed TCP port); the HTTP path is
`application.healthPath` or `/`.

Worker results arrive with the attempt's completion report and are treated as untrusted data: only the canary and
runtime checks are accepted, with bounded fields. A check the plan implies but the worker did not report is
recorded as `errored`.

## Verdict

Only required checks count. `blocked` beats `failed`, which beats `incomplete` (a required check `skipped` or
`errored`), which beats `passed`. A scan that fails, times out or cannot finish within its limits is `errored`,
so the verdict is `incomplete`: nothing passes silently (SEC-OUT-06). Publication (WP21) reads the verdict at publish
time.

## Runtime tests

The worker exports the image as an OCI layout and runs a second, secret-free solve on the same fresh daemon:

```
FROM samuoriimage AS samuori-test        # the built image, from the OCI layout
USER 0
RUN --network=none --mount=type=bind,from=samuoriprobe,target=/.samuori-probe ["/.samuori-probe/samuori-probe", "runtime-probe", "<spec>"]
FROM scratch
COPY --from=samuori-test /.samuori-result/result.json /result.json
```

The probe is the static `samuori-worker` binary (`internal/runtimeprobe`). It starts as the step's first process,
with uid 0 in the user namespace, and then:

1. sets `no_new_privs`, makes itself undumpable and empties the capability bounding set;
2. checks that only loopback exists;
3. starts the image's entrypoint and command as the image user, or as uid 10001 when the image user is root or unset;
4. observes the application: `http-startup` (any response below 500), `static-serve` (200), `process-start`
   (still running after the settle time) or `cli-smoke` (exit status 0), within the timeout;
5. kills every other process in the namespace before writing its result, so the application cannot alter it.

The worker accepts a result only if it is well formed and, for a pass, confirms the constraints. Images for another
platform than the worker's are skipped with a reason. The probe must be statically linked (`make build` sets
`CGO_ENABLED=0`).

The step has no `/sys`: rootless BuildKit removes it, so applications that read their cgroup limits at start, such as
Paketo Node.js images, fail here although they start under Docker or Kubernetes. The owner decided (OD-29,
2026-10-26) that the test should see a read-only view of its own cgroup; a rootless BuildKit step cannot provide one
without `--security=insecure`, so this awaits an approved runtime path (`../spikes/runtime-test-cgroup-view.md`).

## Sensitive content

`internal/imagescan` reads the OCI archive as untrusted input. Blobs are read in place, and layer digests are
verified while streaming. Gzip layers are read as multi-member streams, as runtimes do. Limits cover layers,
entries, uncompressed bytes and findings; exceeding any of them makes the scan incomplete. On the controller the
scan runs in the discovery sandbox: a separate process with rlimits, no network, no process execution and
seccomp on Linux (SEC-OUT-07). The archive is passed as an inherited file descriptor.

| Class | Default | Rules (v1.2.0) |
|---|---|---|
| `build-secret` | `block` (cannot be lowered) | canary matches |
| `private-key` | `block` | PEM private keys with at least 32 bytes of key material, including keys in JSON or source strings with escaped newlines (placeholders such as npm's documented examples do not match); `.pvk`, `.pfx`, `.p12`, `.jks`, `.keystore`, `.bcfks` |
| `database-secrets` | `require-exception` | `var/opt/mssql/secrets/**`, PostgreSQL `pg_hba.conf` and `server.key`, `var/lib/mysql-keyring/**` |
| `database-data` | `warn` | MSSQL, PostgreSQL and MySQL data directories |
| `credential-file` | `require-exception` | `.env`, cloud credential files, registry auth, kubeconfig, SSH private keys, `.netrc`, `.git-credentials`, `.pypirc`, `.npmrc` with a token |
| `credential-content` | `require-exception` | AWS access key IDs and common access tokens |
| `metadata` | `require-exception` | secret-looking environment variables with values; secret-looking build arguments in history; image config labels (`metadata-label`) and manifest or index annotations (`metadata-annotation`) with a value under a secret-looking key or a value in a known credential format (`internal/secretscan`). OpenPGP fingerprints (40 or 64 hex digits, space-separated) under a `GPG_KEY`, `GPG_KEYS`, `PGP_KEY` or `PGP_KEYS` name, as official base images set them, are public and not flagged (rules 1.2.1, `TestMetadataEnvPublicKeyFingerprints`) |

The `sensitive-content` policy changes outcomes per class and adds path rules. Findings report rule, class, path,
layer, size and logical secret ID, never contents (SEC-OUT-04, SEC-OUT-05). Paths the spec declares under
`outputs.expectedSensitiveContent` are labeled with the declared reason; the outcome does not change. Without an
approved exception, a `require-exception` finding fails the check.

Labels and annotations are read by anyone who can pull or inspect the image, including ones inherited from a base
image; their findings name the key, never the value. Admission warns about secret-looking spec entries before the
build (`buildspec.md#image-labels-and-annotations`). Tested by `TestMetadataLabelsAndAnnotations`.

## Output exceptions (ADR 0031)

A maintainer or administrator of the project requests an exception (`POST /projects/{id}/output-exceptions`) with
image path patterns (`**` spans segments; empty, `*`, `**`, absolute and `..` patterns are refused), optional finding
classes, a justification of at least 20 characters and a lifetime of at most 90 days. An administrator other than
the requester approves or rejects it (`GET /output-exceptions`, `POST /output-exceptions/{id}/decision`); an
administrator can revoke it. Every step is audited.

While an exception is approved and unexpired, matching `require-exception` findings become `excepted` and carry the
exception ID. `block` findings, including build secrets and private keys, are never excepted (SEC-OUT-03). A run
that passes only through an exception is classified `restricted`; others are `standard`. A restricted image publishes
only to destinations that accept restricted images (SEC-OUT-08, `docs/architecture/publication.md`). Exceptions are
evaluated when a run executes, so a rescan or promotion after expiry or revocation fails again.

## Vulnerability scanning (ADR 0021, OD-10)

The `vulnerabilities` controller check runs a scanner that the operator installs and selects with
`validation.vulnerabilityScanner` (`grype` or `trivy`; `SAMUORI_VULNERABILITY_SCANNER`). There is no default until
OD-10 settles which vulnerability databases' terms are acceptable. The scanner runs on the OCI archive with a
scratch home, a timeout and output limits. Its database stays in `GRYPE_DB_CACHE_DIR` or `TRIVY_CACHE_DIR` when set,
otherwise in the scanner's usual cache under the service user's cache directory; set the variable for a system
service so the database is not downloaded again. The scanner's own update settings apply. The check records the tool, version and database description, and every
finding with its severity, package, version and fix.

The `validation` policy controls the outcome. `requireVulnerabilityScan` makes the check required.
`failOnSeverity` (`critical`, `high`, `medium`, `low`) fails the check for findings at or above that severity.
`ignoreVulnerabilities` lists IDs reported as ignored. With no scanner configured the check is `skipped`, which
fails closed when it is required.

## API and UI

`GET /artifacts/{id}`, `GET /artifacts/{id}/validations`, `POST /artifacts/{id}/validations` (rescan: repeats the
controller checks and carries worker results over, labeled "From the build-time validation"), and
`GET /validations/{id}`. Build runs include the artifact ID and their newest validation. The run page shows the
verdict, the classification, every check with its required flag, outcome and tool, vulnerabilities, findings with
exception IDs, and the stored build log. The project page lists exceptions and takes requests; administrators decide
them on the Approvals page.

## Evidence

> History: the tests and make targets named below ran at earlier commits and were removed (ADR 0039); restored
> focused tests are marked in `docs/developers/security-test-catalog.md`.

Unit tests cover the scanner (rules, canary across chunk boundaries and in the config, metadata, gzip bombs, entry
limits, corrupted digests, unknown compression, hidden gzip members), verdicts, worker-result sanitizing and the
sandboxed scan subprocess. Build-level tests cover creation in the success transaction, required flags, missing
and forged worker checks, scanner failures and rescans. `make test-buildkit` in the Linux dev VM covers real
runtime tests (pass and fail per profile, the in-container sandbox report, an application that tries to forge the
result or kill the probe), the canary on a build that copies its secret into the image, Syft SBOMs, and a live
Grype v0.120.1 scan (`provision-scanner.sh`) that finds lodash 4.17.15 advisories and records the database. Exception
tests cover requests, self-approval refusal, scope, classes, block findings that stay blocked, expiry, revocation
and the restricted classification; vulnerability tests cover thresholds, ignored IDs, a failing scanner and a
required scan with no scanner.

## Limitations

- Runtime tests use the worker's platform only; multi-platform images are tested for that platform.
- No default vulnerability scanner (OD-10). Grype is tested live in the dev VM; the Trivy adapter was tested only
  against a recorded report, in a test since removed (ADR 0039).
- Signatures and provenance are produced and verified at publication, not by a validation check
  (`docs/architecture/signing.md`).
- `vm-per-job` runs the same checks inside its VM once the provider exists (OD-12b).
