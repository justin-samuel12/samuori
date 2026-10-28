---
title: Upgrades and diagnostics
section: Operating Samuori
summary: Controller and worker versions, the database schema and self-update.
roles: [operator, administrator]
order: 110
---
**Upgrades & Diagnostics** shows each controller's release, state and last heartbeat, the database schema version,
and which releases the workers run, checked against the supported version matrix.

## Upgrade

Back up first, upgrade controllers one at a time, then drain and upgrade workers. The page reports any version a
controller or worker runs that the others do not accept.

## Self-update (administrators)

Under **Self-update**, choose the three files `make release-artifacts` wrote (the archive, `SHA256SUMS` and
`SHA256SUMS.sig`) and **Upload**. Samuori keeps the release only if a key in the controller's release signers signed it.
**Update everything** asks for a reason, then:

1. Updates each controller, one at a time; the others keep serving.
2. Drains each remote worker (running builds finish), updates it, and runs its self-test. It builds uploaded source
   again only once that passes.

With **Stop after the first worker so I can check it** (on by default), the rollout pauses after the first worker.
Check that worker, for example with a build, then choose **Continue with the other workers** or **Stop here**.

Every host checks the signature itself and goes back to its previous release if installing fails. A failure stops
the rollout at that host and shows why; **Stop after the current host** ends it early. Hosts must be installed with
`--release-signers` (see the upgrade guide).

## Add and remove controllers (administrators)

Adding a controller, accepting its join request and removing a controller are under [Servers](help:servers).
