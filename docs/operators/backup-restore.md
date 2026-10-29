# Backup and Restore

Backups are encrypted with [age](https://age-encryption.org) to public keys you choose (ADR 0035). Samuori never holds
the private key, so only the operator can restore.

## Set up

1. Create a key pair on an offline or well-protected machine: `age-keygen -o samuori-backup.key` (or any age tool).
   Keep `samuori-backup.key` (mode 0600) away from the controller; it is needed only to verify or restore.
2. Put the public key line (`age1…`) in a recipients file on the controller, one key per line.
3. Configure the controller:

   ```yaml
   backup:
     dir: /var/backups/samuori            # a mounted share works too
     recipientsFile: /etc/samuori/backup-recipients.txt
     interval: 24h                    # 0: only on request
     keep: 7
   ```

4. Copy the archives off the host with your usual tooling (Kopia, restic, object storage sync). They are encrypted.

## What a backup holds

| Part | How it is copied |
|---|---|
| Database | SQLite `VACUUM INTO`, or `pg_dump` from an exported snapshot; per-table row counts and checksums from the same snapshot |
| Internal CA and grant key | The `pki` directory |
| Secret store key (or cluster key) | Included, so stored secrets can be read after a restore |
| Blobs | Those kept on this controller; blobs held by S3 or OCI storage stay there |

## PostgreSQL in containers

The default controller image carries no PostgreSQL tools. Controllers that back up PostgreSQL use the image built with
`--target controller-pgtools` from `deploy/compose/Dockerfile`, which carries PostgreSQL 18's `pg_dump` and
`pg_restore` (they dump servers 16 to 18). The `backup-tools` health check reports a missing or older tool.

## Verify and restore

- On Organization > Backups, **Check** confirms the archive is unchanged since it was written.
- `samuori-server verify-backup --identity-file samuori-backup.key ARCHIVE` decrypts it and checks every file and, for
  SQLite backups, every table.
- To restore, stop the controller and run, into an empty data directory and an empty database:

  ```
  samuori-server restore --identity-file samuori-backup.key --data-dir /var/lib/samuori-restored \
    [--database postgres://owner@db.example/samuori_restored] ARCHIVE
  ```

  The restore checks every file and every table against the backup before migrating to this version's schema. Point
  `dataDir` and `database.dsn` at the restored copy and start the controller.

## RPO and RTO

- **RPO** is the backup interval. For a smaller RPO on PostgreSQL, add WAL archiving (for example pgBackRest) to the
  database itself.
- **RTO** is the restore time plus a controller start. A rehearsal at an earlier commit (`internal/api/backup_integration_test.go`, since
  removed, ADR 0039) restored a test installation (73 tables, about 20 000 rows) in 0.07 s into SQLite and about 0.5 s into PostgreSQL on a
  developer laptop. These are test-scale numbers; rehearse with your own data and record the result.

## Rehearse

At least quarterly: restore the newest archive into a scratch data directory and database, start a controller on it
with no workers, sign in, and compare the project, build and audit pages with production.
