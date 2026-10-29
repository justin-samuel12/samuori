# Operator Runbook

Where to look when something goes wrong. Each entry links to the guide that covers it.

| Situation | What to do | Guide |
|---|---|---|
| Install or upgrade one host | `install.sh`, then the setup code and enabling the `local` pool | [all-in-one.md](all-in-one.md) |
| A controller is down in a cluster | The others take over; check `GET /api/v1/system/health` and Organization > Upgrades & diagnostics | [high-availability.md](high-availability.md) |
| A controller lost the database or NATS | It fences itself and turns unready; restore connectivity, and it rejoins | [high-availability.md](high-availability.md) |
| A worker is quarantined | An environment failed to clean up. Inspect `--provider-dir`, fix the host, restart the worker; it destroys leftovers on start | [all-in-one.md](all-in-one.md), [vm-per-job.md](vm-per-job.md) |
| A worker's certificate expires or is revoked | Re-enroll with a new token | [worker-identity.md](worker-identity.md) |
| Rotate the internal CA | Planned rotation from Infrastructure > Workers & pools; workers acknowledge the new bundle | [worker-identity.md](worker-identity.md) |
| Move from SQLite to PostgreSQL | `samuori-server migrate-database`, offline | [database-migration.md](database-migration.md) |
| Rotate the database password | Change it at the database, then Database > Update password; recover a stopped controller with `samuori-server set-database-password` | [database.md](database.md) |
| Back up or restore | `samuori-server backup`, `verify-backup` and `restore`, or Organization > Backups | [backup-restore.md](backup-restore.md) |
| Upgrade controllers and workers | Version matrix, backup first, rolling restart | [upgrades.md](upgrades.md) |
| Federation peer unreachable | Delegated builds wait; an administrator can abandon them | [federation.md](federation.md) |
| Scripted operations | The `samuori` CLI | [cli.md](cli.md) |
| Offer a new runtime version (for example a new Node.js major) before a Samuori release ships it | Find the image digest with `docker buildx imagetools inspect`, then Build supply > Recipes & runtimes > Runtimes > Add runtime and Approve | [recipes-and-policies.md](../architecture/recipes-and-policies.md#organization-runtimes-adr-0051) |

None of these procedures has been rehearsed against the current tree since the tests were removed (ADR 0039).
