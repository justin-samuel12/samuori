# Spike: Buildah as a Second Build Engine

Date: 2026-10-20. Status: **Complete; the adapter is built** (`internal/adapters/build/buildah`). The full self-test
and the runtime tests pass with Buildah. The authorizing mirror and secret delivery remain out of scope (OD-26). Raw output is kept in the maintainer's private records. Script:
`tools/spikes/buildah-isolation/run.sh`. Decision it informs: D05 (pluggable engines).

## Question
Can rootless Buildah run Samuori builds with the same isolation properties that BuildKit showed for `rootless-host`
([buildkit-isolation](buildkit-isolation.md), P1, P1+E and P1+L)? What would an adapter need?

## Environment

| Item | Value |
|---|---|
| VM | `samuori-dev` (Lima, `vz`, aarch64), personal device |
| Guest | Ubuntu 26.04.1 LTS, kernel 7.0.0-34-generic, cgroup v2 |
| Buildah | 1.42.1 (Ubuntu package `1.42.1+ds1-2build1`), installed without recommended packages |
| OCI runtime | runc 1.4.0 (Apache-2.0); crun (GPL) was not installed |
| Networking | netavark 1.16.1 (required even for `--network none`); slirp4netns 1.3.3 |
| Build user | `samuori-build` (uid 999, subordinate IDs 300000–365535), the `rootless-host` build user |
| Fixtures | the BuildKit spike's `probe/Dockerfile` and `limits/Dockerfile`; a fictitious secret value |

Each run uses a fresh storage root (`--root`, `--runroot`) under a private directory of the build user,
`registries.conf` pointing `docker.io` at the VM's pull-through mirror, and `--layers=false --pull=always`. Only
configuration properties were probed. No exploit code was run.

## Results

`✔` = holds, `✘` = does not hold, `–` = not tested. The BuildKit column repeats P1/P1+E from the BuildKit spike.

| Property | Buildah | BuildKit (P1, P1+E) |
|---|---|---|
| Step root maps to the build user; other IDs to subordinate IDs | ✔ | ✔ |
| Seccomp filter on build steps | ✔ (mode 2) | ✔ |
| Capabilities | default set `0x800405fb`, namespaced | the same |
| `NoNewPrivs` | 0 | 0 |
| Host kernel exposed to steps | ✘ (shared kernel) | ✘ |
| No Docker, containerd or BuildKit sockets in steps | ✔ | ✔ |
| No telemetry socket in steps | ✔ | ✘ (`/dev/otel-grpc.sock`, F3) |
| `--network none` blocks all traffic | ✔ | n/a |
| Strict egress: direct internet, DNS and worker-host services blocked | ✔ | ✔ |
| Host loopback blocked | ✔ (slirp4netns default) | ✔ |
| Base images pulled only through the allowlisted mirror | ✔ | ✔ |
| Allowlisted registry through the proxy | – (no proxy running in this VM) | ✔ |
| Secret mounted for the step | ✔ (`--secret`, `/run/secrets/<id>`) | ✔ |
| Secret absent from the image archive | ✔ (both builds) | ✔ |
| Secret absent from engine state | ✔ (storage root and run root) | ✔ |
| Process limit (`TasksMax=512`) | ✔ step fails cleanly; Buildah reports it | ✔ (F11) |
| Memory limit (`MemoryMax=1G`, no swap) | ✔ step killed inside the unit's cgroup; Buildah reports it | ✔, but the daemon was killed (F10) |
| No processes left after the build | ✔ | ✔ (daemon stopped by the provider) |

## Findings

- **B1** Ubuntu's `buildah` and `runc` packages ship AppArmor profiles that grant `userns` to those binaries, so they
  work with `kernel.apparmor_restrict_unprivileged_userns=1` (compare F1, where RootlessKit needed its own profile).
  The profiles are `flags=(unconfined)` apart from that grant.
- **B2** Buildah needs netavark even for `--network none`; the error without it is clear. Rootless slirp4netns
  networking also needs `/run/user/<uid>/libpod/tmp` to exist.
- **B3** Build-step identity, seccomp and capabilities match BuildKit's rootless steps. Buildah mounts no telemetry or
  engine socket into steps, which removes the channel noted in F3.
- **B4** Under the uid-scoped nftables rules used for BuildKit, everything except the allowlisted host and ports was
  blocked, and base images came through the mirror configured in `registries.conf`. `--network none` blocked all
  traffic, which suits builds that need no downloads.
- **B5** `--secret id=…,src=…` mounts the file for the step. The value was absent from both OCI archives and from
  Buildah's storage and run roots after the build.
- **B6** With the whole build in a transient systemd unit, both limits held and Buildah stayed alive to report the
  failure. There is no long-lived daemon to lose, so the F10 failure mode does not arise.
- **B7** Buildah writes an OCI archive directly (`-t oci-archive:…`) and needs no daemon. One process per build, with a
  fresh storage root, gives the "one build per engine, nothing reused" property by construction.
- **B8** Licenses: Buildah, runc, netavark and containers-common are Apache-2.0; slirp4netns is GPL-2.0 and is already
  run (exec only) for BuildKit through RootlessKit. Nothing would be linked.

## Adapter findings

- **B9** Buildah bind-mounts a generated marker file, `/run/.containerenv`, into each step, next to `/etc/hostname`,
  `/etc/hosts` and `/etc/resolv.conf`. Like those, it is created per build from a temporary file (under the build's
  own directory, through `TMPDIR`) and holds no host path. The SEC-ISO-05 host-mount check therefore allows it
  alongside the other three.
- **B10** Buildah reads the build context from disk as the build user. The provider's directories are private, so the
  adapter copies the context, the Dockerfile directory and the runtime-test inputs into the build's group-shared
  directory instead of changing permissions on the worker's own files.
- **B14** Because the build user can write in the group-shared directory, it can replace any path there with a
  link. The adapter therefore opens that directory once as an `os.Root` and does every read, write, permission change
  and removal through it, refuses outputs that are not regular files or that the worker owns, and copies only regular
  files out of test results (SEC-ISO-10).
- **B11** The build's systemd user scope ends when Buildah exits, and systemd reports `Result=success` even after an
  out-of-memory kill in the delegated subtree. The wrapper shell therefore waits for Buildah and copies the scope's
  hierarchical `memory.events` into the build directory before the scope ends; the adapter reads that copy. The kernel
  log confirmed that the kill was constrained to the build's scope.
- **B12** With rootless networking, Buildah logs `failed to move the rootless netns slirp4netns process to the
  systemd user.slice`, because the build has no user bus. This is harmless and intended: slirp4netns stays inside the
  build's scope and limits.
- **B13** The `oci-layout://` named build context is not supported by Buildah 1.42. The runtime test loads the image
  from a single-manifest layout (`oci:<dir>:samuoriimage`) into the build's storage and passes it as
  `container-image://<id>`.

## Not covered

- **Authorizing mirror.** BuildKit fetches build-scoped mirror tokens through its session (`mirror-auth` spike).
  Buildah would need a per-build auth file holding a short-lived token. This is untested, and private base images
  through the mirror are not yet possible with Buildah.
- **Proxy-mediated downloads.** No HTTP proxy ran in this VM, so allowlisted downloads through the proxy were not
  exercised with Buildah.
- **Cache import and export, SBOM attestations from the engine, and multi-platform builds** (Buildah needs QEMU
  binfmt or native workers per platform).

## Conclusion

Rootless Buildah meets the `rootless-host` isolation properties that BuildKit met in this environment, and slightly
improves on two (no telemetry socket, no daemon to kill under memory pressure). The adapter is built within the
existing isolation model, and a Buildah worker passes the same per-worker self-test before it may build uploaded
source. Secret-bearing builds and private base images stay on BuildKit: OD-26 (owner, 2026-10-20) keeps them disabled on Buildah until the conditions recorded there are verified.
