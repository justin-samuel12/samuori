# 0017. Persistence, migrations and messaging

Status: Accepted (2026-10-19)

## Context
D07 and D08: SQLite for a single controller, PostgreSQL for HA, embedded NATS JetStream by default, DB as the
source of truth.

## Decision
- **SQLite:** `modernc.org/sqlite` (pure Go, BSD-3-Clause; no cgo, simple cross-compilation). WAL mode, foreign keys
  on, busy timeout, a single writer connection.
- **PostgreSQL:** `jackc/pgx/v5`. Supported servers: 16, 17, 18 (14 reaches EOL 2026-11-12; 15 in 2027).
  CI tests 16 and 18.
- **Identifiers:** ULIDs for every resource, stored as text in SQLite and PostgreSQL alike for portability.
- **No ORM.** Hand-written repository layer on `database/sql`-style interfaces; SQL is kept per dialect where it
  differs.
- **Migrations:** `pressly/goose` (MIT), separate `migrations/sqlite/` and `migrations/postgres/` with the same
  version numbers and logical schema; a test asserts both produce equivalent schemas.
- **Outbox:** state change + outbox row in one transaction; relay publishes to JetStream with message
  deduplication IDs; consumers are idempotent.
- **Messaging:** embedded `nats-server` v2.15 (Apache-2.0) for all-in-one; external clustered NATS for HA. NATS is a
  transport, never authoritative.
- **SQLite → PostgreSQL migration** is an offline, verified copy (row counts + checksums per table) with a rollback
  path. It is tested on nontrivial fixture data.

## Consequences
Two migration sets must be kept in step; CI enforces it. Pure-Go SQLite is slower than cgo SQLite; this is acceptable
for single-controller scale and is measured in performance tests.
