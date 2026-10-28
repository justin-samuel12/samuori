# 0024. Placement of untrusted-content processing, messaging and publication

Status: **Accepted as direction** (2026-10-19, owner decision OD-11). Publication stays exclusively on the controller.

## Context
The threat model (`docs/architecture/threat-model.md`) depends on three placement choices that earlier documents
left open:

1. Discovery must read untrusted manifests "in a read-only sandbox", but no document said which process does it.
   Parsing hostile input inside the API process puts the controller at risk from parser bugs.
2. The scheduler documents say workers use an outbound channel to the controller, but they do not say whether
   workers also connect to NATS. A worker-reachable NATS widens the attack surface and requires a second
   authentication scheme.
3. Which component holds destination registry credentials. A worker holding them could publish forged images.

## Decision
- **Discovery** runs in a short-lived subprocess spawned by the controller per snapshot: no network, a read-only
  view of one snapshot, CPU/memory/time limits, and no access to the DB or credentials. It returns findings as
  structured data, which the controller validates before storing.
- **Workers never connect to NATS.** NATS is internal to controllers (outbox relay, wake-ups, controller-to-
  controller coordination). Controllers deliver work to workers over the authenticated worker channel (TB5).
  Redelivery, fencing and idempotency apply to that channel exactly as to NATS consumers.

- **Publication runs only in the controller.** Workers commit built images to internal artifact storage. The
  controller copies them to destination registries. Workers never receive destination registry credentials.

## Consequences
- Publication traffic flows from internal storage through the controller (or a controller-run publisher), so
  publisher capacity is sized for image copies.
- In all-in-one, embedded NATS listens only on loopback or a unix socket.
- External clustered NATS (HA) authenticates controllers only.
- Discovery adds subprocess start-up cost per snapshot. This is acceptable because discovery runs once per upload.
