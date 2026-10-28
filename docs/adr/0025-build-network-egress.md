# 0025. Build network egress: policy-enforcing proxy with ecosystem presets

Status: **Accepted as direction** (2026-10-19, owner decision OD-11). Enforcement mechanism validated in the BuildKit isolation spike
(`docs/spikes/buildkit-isolation.md`, findings F4–F8). Threat rows: TM-42, TM-47 in
`docs/architecture/threat-model.md`.

## Context
Untrusted builds need default-deny egress. Legitimate builds still need package registries, base-image pulls and,
in some organizations, internal artifact repositories. Requiring administrators to allow every dependency host
by hand is unworkable, and developers must not need to understand networking to get a working build.

## Options

| Option | Security | Operability | Notes |
|---|---|---|---|
| A. Open egress with logging | Weak: exfiltration, SSRF, metadata access | Easiest | Not acceptable for untrusted builds |
| B. Per-host allowlists maintained by admins | Strong | Poor: endless manual entries | Rejected as the primary mechanism |
| C. **Egress proxy with curated ecosystem presets** | Strong at host level; no payload inspection | Good: presets are chosen automatically from the detected stack | Recommended |
| D. Internal mirrors only (no internet from builds) | Strongest | Needs mirror infrastructure per ecosystem | Supported as a stricter policy on top of C |

## Decision

### Enforcement
- Build steps and runtime-test containers have **no direct route** out of the worker. The only egress path is a
  Samuori-managed forward proxy reached through standard `HTTP(S)_PROXY` settings injected by the recipe.
- The worker host or VM firewall drops all other traffic from the build user's namespace, including direct DNS.
  Name resolution happens at the proxy. Cloud metadata, link-local and private ranges are always blocked unless an
  admin defines an internal destination.
- The proxy decides on CONNECT host and port only. It does not intercept TLS. Every allowed and denied request is
  logged against the build attempt.
- Base-image pulls are performed by the build engine, not by build steps, and go only through a **Samuori-controlled
  registry mirror** (pull-through cache or approved registry) configured in the engine, pinned by digest. In the
  spike, engine pulls through the egress proxy alone were unreliable (F6), while a pull-through mirror worked.
- The firewall matches the build user's UID and the **original** destination address, so services published
  through NAT are matched correctly (F7). Only the proxy and mirror ports are allowed.
- Dependency installation and compilation are separate recipe stages. Network access is granted only to the
  install stage. Later stages run with no network where the recipe supports it.
- Package managers that ignore proxy settings fail closed, with a clear message, rather than bypassing policy.

### Policy model (central, invisible to developers by default)
- **Ecosystem presets** maintained with Samuori releases and versioned like recipes: npm, PyPI, NuGet, Go module proxy,
  Maven Central, crates.io, Alpine/Debian/Ubuntu package mirrors, and similar. Each preset lists the hosts that
  ecosystem needs.
- The recipe declares which presets it needs. Discovery and recipe selection pick them automatically, so a
  developer never chooses network rules.
- Administrators control which presets are allowed per organization, team or pool, and can replace a preset with
  an internal mirror (option D) or add named **internal destinations** (host, port, optional credential reference).
- Projects can request an extra destination; it takes effect only after admin approval.

### Approved mirror: operational implications
Mirror-only base-image pulls are the current tested design (spike F6). Operating it means:

- **Authentication.** The mirror holds upstream registry credentials (for example, to avoid anonymous rate limits)
  as secret references. Workers authenticate to the mirror with job-scoped, read-only credentials and never see
  upstream credentials.
- **Availability.** If the mirror is down, builds needing uncached images fail closed with a clear "base image
  source unavailable" result. Preflight and the admin Overview show mirror health from real checks. HA deployments
  run the mirror redundantly or use a registry with its own HA (for example Harbor proxy cache).
- **Refresh.** Approved base images are pinned by digest, so cached content never changes underneath a build.
  Admins update base-image policy to new digests; the mirror fetches them on first use or by a scheduled pre-pull.
  A tag-only cache TTL applies to digest lookups only.
- **Retention and capacity.** Cached images not referenced by any approved policy are garbage-collected on a
  schedule. Mirror storage is a storage profile (D04) with quotas and monitoring.
- **Upstream scope.** CNCF Distribution in proxy mode serves one upstream per instance, so each upstream registry
  gets its own instance or a registry that supports several upstreams. Hosts outside approved upstreams are
  unreachable by design.
- **All-in-one.** The all-in-one install includes a local mirror (CNCF Distribution, Apache-2.0) preconfigured for
  the approved upstream registries, so a first-time user needs no extra service.

### Developer experience
A denied request produces a plain-language result on the run page: what the build tried to reach, which stage,
and the action to take (for example, "ask an administrator to allow this registry"). Developers never edit proxy
or firewall settings.

### Implementation choice (pending spike)
A small, independently written Go CONNECT proxy run by `samuori-worker` as a separate process, preferred over bundling
a general-purpose proxy (Squid and tinyproxy are GPL, and Envoy is far larger than this needs). The spike uses an
off-the-shelf proxy only to validate enforcement.

### Implementation (WP15, 2026-10-19)
Implemented as described in `docs/architecture/egress.md`. The proxy runs inside `samuori-worker` with a credential per
build attempt instead of a separate process; whether to split it out is OD-21.

## Consequences
- Presets need upkeep as ecosystems change hosts. They are versioned, tested and shipped like recipes.
- Host-level filtering cannot stop exfiltration to an allowed host, such as publishing a package to an allowed
  registry. That remains a residual risk.
