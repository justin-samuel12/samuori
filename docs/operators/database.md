# Database

Status: **implemented** (ADR 0043). Exercised on 2026-10-22 against PostgreSQL 16.15 on one controller, and by the
two-controller handover test (SEC-DB-03). Not yet exercised on RDS or across separate controller hosts.

**Infrastructure > Database** shows where Samuori keeps its records and whether each live controller can sign in.
**Test connection** signs in with a new connection from the controller that handles the request.

## Rotate the password

Samuori never changes the database password.

1. Change the password at the database, for example in RDS.
2. In **Database > Update password**, enter it twice with a reason. The controller signs in with it first; if that
   fails, nothing changes.
3. The other controllers verify it within seconds and switch. The **Controllers** table shows `verified`, or `failed`
   with the reason (that controller keeps its previous password and retries every minute).

Each controller keeps the password in `<dataDir>/database-password` (mode 0600), which replaces the password in
`/etc/samuori/env` at start. Leave `/etc/samuori/env` as it is. Between steps 1 and 2, new connections fail; open ones keep
working. SQLite has no password.

## Lost connections

A PostgreSQL restart, an RDS failover or a terminated backend leaves dead connections in each controller's pool.
Samuori discards them without failing the request when a retry is safe: a `BEGIN`, or a `SELECT` outside a
transaction, that finds its connection gone runs again on another one. A write outside a transaction, or a
statement inside one, is never retried and fails that request once, because Samuori cannot tell whether the server
applied it. Connections idle for more than a second are checked before reuse, idle ones close after five minutes,
and every connection is replaced after 30 minutes so a failover that leaves old connections open does not keep them.

## Recover a controller

A controller that was stopped during a rotation cannot sign in when it starts. On that host:

```
read -rs PW && printf '%s' "$PW" | sudo samuori-server set-database-password --config /etc/samuori/server.yml --env-file /etc/samuori/env
sudo systemctl restart samuori-controller
```

It signs in with the password before storing it, and stores it only for that controller.
