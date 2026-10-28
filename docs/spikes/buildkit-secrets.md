# Spike: BuildKit Secret Delivery (OD-15)

Date: 2026-10-19. Status: **Complete.** The results contradicted two assumptions in accepted ADRs. The owner approved OD-16 to OD-18
(2026-10-19) in response; see `docs/architecture/build-cache.md`. Raw logs (digests only, verified free of secret markers) are kept in the
maintainer's private records. Code: `tools/spikes/buildkit-secrets/`.

## Question
How does BuildKit's supported secrets API actually deliver, cache, retain and isolate build secrets? ADR 0029
deliberately did not assume per-instruction delivery.

## Environment

| Item | Value |
|---|---|
| Host | Personal Mac (owner-confirmed), disposable Lima `vz` VM with no host mounts, deleted afterwards |
| Guest | Ubuntu 24.04.4 LTS, kernel 6.8.0-134 |
| BuildKit | v0.34.0 (commit `f5b59a1f`), rootless under RootlessKit v3.3.0 (slirp4netns, host loopback disabled); both checksum-verified |
| Client | `secretprobe`, built on `github.com/moby/buildkit/client` v0.34.0. It registers `secretsprovider.NewSecretProvider` on the session and logs every `GetSecret` call with a timestamp and a SHA-256 prefix, never the value |
| Secrets | Synthetic: five text values and one 4 KiB binary value, each carrying a unique random marker for forensic search |
| Forensics | `grep` for markers across BuildKit state, runtime dir, `/tmp`, `/var/tmp` and home; OCI blobs (decompressed); local cache export; progress and daemon logs; `gcore` memory dump of `buildkitd` |

## Results

| # | Scenario | Observed |
|---|---|---|
| S1 | Timing | BuildKit called `GetSecret` when each `RUN` that mounts the secret **started executing** (each request 2 s after the previous step's 2 s sleep), once per mount per execution. A second `RUN` mounting `alpha` triggered a **second request**; there was no per-session caching. Two mounts in one `RUN` triggered two requests at the same instant |
| S1 | Unused | A secret provided but never mounted (`zeta`) was never requested. A secret mounted only in a stage outside the target graph (`delta`) was never requested |
| S1 | Optional / missing | `required=false` with no value: requested, "not found", step ran without the file. **A mount with no `required` option behaved the same**, so BuildKit treats secret mounts as optional by default |
| S1 | Mount targets | Values were mounted at `/run/secrets/<id>` and at a custom target (`/certificates/gamma.bin`). The binary value arrived byte-exact (matching digest). Later steps saw neither `/run/secrets` nor `/certificates`, and exported layers had no entries for either path |
| S2 | Cached rebuild | Every step was `CACHED` and **no secret was requested** |
| S3 | Revocation | The provider denied `alpha`'s second request. The step failed and the solve ended with "release denied". A provider error, unlike "not found", fails the build even for optional mounts |
| S4 | Client cancel | Canceling 3 s after release ended the solve at once. No processes with secret mounts and no `sleep 60` process remained (checked immediately and after 10 s) |
| S5 | Client disconnect | `SIGKILL` of the client 3 s after release gave the same result as S4 |
| S6 | Failing step | The build failed. No secret marker anywhere on disk afterwards |
| **S7** | **Concurrent builds, same daemon, same Dockerfile, different values for the same ID, `no-cache`** | **BuildKit merged the identical `RUN` vertex across the two sessions and executed it once**. Only build B's provider was asked. Build A never requested its secret and **received output computed with build B's secret** (`alpha.hash` = B's digest) |
| **S8** | **Cache across sessions** | Build A (value `alpha`) reused a cached step computed earlier with `alpha2` and got `alpha2`'s result. **Neither build requested its secret** |
| S9 | `env=` mount | Value available as an environment variable in the step. Not present in any exported blob (config, history, layers) |
| S10 | Build script copies a secret into the image | Marker found in 1 OCI layer blob and in the BuildKit snapshot store. Exact-byte detection works for verbatim copies |
| Logs | All progress logs and the daemon log | No marker in any log |
| Outputs | OCI layers, image config and history, provenance attestation (`mode=max`, present in the export), local cache export | No marker except S10's deliberate copy |
| Disk | BuildKit state, runtime dir, temp dirs | No marker except S10's copy (in the snapshot store) |
| **Memory** | `gcore` of a running `buildkitd` | After S1, `alpha` appeared once and `gamma` twice. After all scenarios, **`alpha` appeared 13 times**. Values persist in daemon memory after builds complete |

## Observed versus documented

| Behavior | Documented by BuildKit or Docker? | Source |
|---|---|---|
| Secret values are not part of the cache key; changing a value does not invalidate the cache; IDs and mount paths are | **Yes**: Docker build cache documentation | [Build cache invalidation](https://docs.docker.com/build/cache/invalidation) |
| Secret mounts are not persisted to image layers | Stated in Docker's build-secrets guide (not re-verified in this session); also observed | This spike, S1 and S9 |
| Request timing per `RUN` execution and per mount; no request on cache hit | Observed only (consistent with the cache documentation) | This spike |
| Mounts optional unless `required=true` | Observed. The Dockerfile reference documents `required` (default false) | This spike |
| Identical concurrent vertices from different sessions merged, executed once, and the result shared | **Not found in documentation**; observed | This spike, S7 |
| Values retained in daemon memory after use | Not documented; observed. No secure deletion is claimed | This spike |

## Security implications

1. **A shared daemon breaks the session boundary in ADR 0029 (S7).** Concurrent builds in one `buildkitd` can share
   execution of identical steps. One build's secret then shapes another build's output, and the other build's own
   secret is never consulted. Different projects or tenants on one daemon are affected.
2. **Cache reuse ignores secret values (S8, documented).** A cached result computed with one project's secret, or an
   old rotated secret, can be reused by another build in the same cache scope without a request. This is a
   confidentiality issue (derived data crosses builds) and an integrity issue (outputs don't reflect current
   secrets).
3. **Daemon memory retains values (memory scan).** A long-lived daemon accumulates secrets from every build it ran.
   Compromising that daemon later exposes them. Memory-only handling is not secure deletion.
4. **Mounts default to optional.** A missing value silently yields a step without the file unless Samuori enforces
   presence. The Samuori provider must return a hard error, not "not found", for anything except spec-optional,
   unbound secrets.
5. **Good news:**
   - per-execution requests make release and revocation between steps effective;
   - cancellation and disconnect leave no running step or mount behind;
   - no values were found in logs, layers, config, history, provenance or cache export;
   - custom mount targets leave no residue in layers;
   - the exact-byte canary is feasible for verbatim copies (S10).

## Architecture changes (approved 2026-10-19 as OD-16 to OD-18)

| OD | Contradicts or extends | Recommendation |
|---|---|---|
| OD-16 | ADR 0029 (session boundary); ADRs 0013 and 0027 already require one build per daemon, but only for resource limits | Make **one build per `buildkitd` at a time** a mandatory security requirement in every isolation profile, including `rootless-host`. Concurrency comes from more daemons, never from sharing one |
| OD-17 | ADR 0029 and ADR 0013 (cache per trust domain is not enough) | Scope cache import and export by **project plus the set of secret binding versions** used by the build. Don't key on secret values: hashing values into args or layers would expose low-entropy secrets to offline guessing. Steps with secret mounts never reuse cache across scopes |
| OD-18 | ADR 0027 (`rootless-dedicated-vm` keeps a VM across jobs) | Start a fresh `buildkitd` (and its state directory, where practical) for every build that uses secrets, even when the VM persists. `vm-per-job` satisfies this already |

Also, without needing a decision, the ADR 0029 design is refined to match what was observed:
- the provider answers "not found" only for spec-optional unbound IDs, and returns an error for everything else;
- release happens per BuildKit request, and requests follow per-`RUN` execution, so revocation applies between steps.

## Limitations

- One BuildKit version (v0.34.0), rootless OCI worker only; the containerd worker and Kata were not tested.
- Memory evidence covers the `buildkitd` process only, not RootlessKit or slirp4netns.
- Disk forensics searched likely locations, not the raw block device.
- S7 used identical Dockerfiles and contexts. Builds that differ anywhere before the secret step would not merge.
  This was not separately measured.
