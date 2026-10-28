# Federation

Two Samuori installations can run each other's builds in agreed locations (ADR 0037). The installation that requests a
build (the originator) owns it; the installation that runs it (the executor) applies its own admission, policies and
workers. Specs and source files travel; build plans never do.

## Set up

On each installation, under **Administration > Federation**:

1. Note **This installation**: the organization ID and the CA pin. Exchange them with the other administrator out of
   band.
2. **Add peer** with the other installation's name, federation endpoint, CA pin and organization ID.

On the executor:

3. Start its federation listener: `federation.listen` (for example `0.0.0.0:8445`) and `federation.hostnames` (the
   names the originator uses). Set `federation.allowPrivatePeers: true` when the peers live on private networks;
   link-local and metadata addresses are always refused.
4. **Grant a peer**: a hosting project for the peer's builds, the locations of your own workers it may use and how
   many builds may run at once.

On the originator:

5. **Add a peer location**: a location name here that sends builds to a location of the peer. Restrict projects to it
   or let developers choose it like any location.

## What happens to a build

1. A build requested in a peer location passes this installation's admission, except for local worker capacity.
   Builds that use secrets are refused: secrets never leave their installation.
2. The leader sends the snapshot's files and the spec. The executor checks the files under its upload rules, records
   the spec in the hosting project, runs its own admission and builds on its own verified workers.
3. When the executor's build and validation pass, the originator fetches the image by digest, stores it, records the
   executor's runtime-test results as reported by that peer and runs its own validation. Only then does the build
   count as succeeded and become publishable here.
4. A build the executor refuses, blocks or fails fails here with the executor's reason.

## When a peer cannot be reached

The build stays **delegated** and the Federation page shows since when the peer is unreachable. It is never run
anywhere else, so it never has two executors. When the peer is back, the build continues.

If the peer will not come back, **Abandon** the delegation (with a reason). The build fails here at once, the peer is
told to stop when it can be reached, and any result it sends later is refused.

## Security

- Every call is mutual TLS with certificates from each installation's own CA, checked against the registered pin and
  organization; system roots are never used. **Disable** a peer to stop federation with it in both directions.
- A peer acts in its hosting project with a developer's rights there and nowhere else, under the executor's policies.
- After a CA rotation, the other administrator updates the pin; until then calls fail with a certificate error.
