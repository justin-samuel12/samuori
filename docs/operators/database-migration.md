# Moving from SQLite to PostgreSQL

`samuori-server migrate-database` copies a standalone controller's SQLite database into a new PostgreSQL database (D07,
ADR 0017). It is needed before a controller can join a cluster ([high availability](high-availability.md)) and
whenever PostgreSQL is preferred for a single controller.

## Procedure

1. Stop the controller and its workers (`systemctl stop samuori-server samuori-worker`). The command refuses to run while a
   controller's heartbeat in the SQLite database is recent.
2. Copy the SQLite file aside (`cp /var/lib/samuori/samuori.db /var/lib/samuori/samuori.db.before-postgres`).
3. Create an empty PostgreSQL 16–18 database, owned by a role that may create tables, and the application role
   `samuori_app` if you separate them (`database.migrationDsn` and `database.dsn`).
4. Run, as the owner role:

   ```
   samuori-server migrate-database --from sqlite:///var/lib/samuori/samuori.db --to postgres://owner@db.example/samuori?sslmode=verify-full
   ```

   The command brings both databases to the current schema, copies every table in one transaction, then reads every
   table back from both and compares row counts and checksums. Any difference, or any value PostgreSQL cannot hold,
   rolls the copy back and leaves the target empty. It prints each table's row count and checksum.
5. Set `database.dsn` (and `database.migrationDsn`) to PostgreSQL and start the controller. Sessions, API tokens,
   enrolled workers, secrets and blobs carry over: the data directory, its keys and its blobs stay where they are.

## Rolling back

Until the new installation has taken changes you need, stop the controller, point `database.dsn` back at the SQLite
file and start it again. Changes made on PostgreSQL after the switch are not copied back.

## Notes

- PostgreSQL keeps timestamps to the microsecond; finer fractions in SQLite are truncated.
- A target that already holds any row is refused, so the command never merges installations.
- Evidence (an earlier commit; the test was removed, ADR 0039): `internal/api/migrate_integration_test.go` filled an
  installation through the API plus 20 000 audit events, migrated it and checked that the PostgreSQL-backed API returned the same data.
