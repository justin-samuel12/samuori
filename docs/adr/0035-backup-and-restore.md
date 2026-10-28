# 0035. Backup and restore

Status: Accepted (2026-10-20). Implements D27 within the approved scope.

## Context
D27 asks for administrator-managed, encrypted backups with validation, external adapters, consistency procedures,
documented RPO/RTO and disaster testing. The simple-install rule rules out a backup service, and the project
integrates mature tools instead of rebuilding them.

## Decision
- **Database copies use the database's own tools from one snapshot.** SQLite: `VACUUM INTO`, which is consistent
  while the controller runs. PostgreSQL: `pg_dump --format=custom` with a snapshot exported by a read-only
  repeatable-read transaction; the same transaction computes per-table row counts and checksums, so the checksums
  describe exactly what the dump holds. No own dump format.
- **Archive.** One tar stream holding the database copy, the internal CA directory, the secret store or cluster key
  (unless excluded) and blobs kept on the controller, with a manifest of SHA-256 digests as its last entry. Blobs
  held only by S3 or OCI storage are not copied: that storage keeps durable copies of its own (versioning,
  replication).
- **Encryption with age** (`filippo.io/age`, BSD-3-Clause) to X25519 recipients. Controllers hold only public keys;
  the identity that decrypts stays offline with the operator. There is no unencrypted mode.
- **Restore** only into an empty data directory and an empty database. Every entry is checked against the manifest,
  then every table against the backup's checksums, before any migration of a newer binary runs; a failure leaves the
  target empty (`pg_restore --single-transaction`). A SQLite backup restores into SQLite or PostgreSQL (through the
  verified transfer of ADR 0017); a PostgreSQL backup into PostgreSQL.
- **Administration.** `backup.dir` and `backup.recipientsFile` enable backups; `backup.interval` schedules them on
  the cluster leader and `backup.keep` prunes old archives. Administrators back up and re-check archive integrity
  from the Backup & Restore screen or the API; `samuori-server backup`, `verify-backup` and `restore` run on the host.
  Every backup and check is audited.
- **External adapters** are file-level: `backup.dir` may be a mounted share, and tools such as Kopia, restic or object
  storage sync copy the encrypted archives off the host. Point-in-time recovery for PostgreSQL uses its own WAL
  archiving (for example pgBackRest), outside Samuori.

## Consequences
- RPO is `backup.interval` unless PostgreSQL WAL archiving is added; RTO is the restore time plus a controller start
  and is measured in the rehearsal.
- PostgreSQL backups need `pg_dump` and `pg_restore` at least as new as the server.
- An archive that includes keys is as sensitive as the controller itself; its age identity must be protected
  accordingly, and an archive without keys cannot recover stored secrets.
