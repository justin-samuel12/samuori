---
title: Backup and restore
section: Administration
summary: Encrypted backups of Samuori's data, and how to restore them.
roles: [administrator]
order: 240
---
**Backup & Restore** makes encrypted backups of the database, the controller's keys and CA, and files kept on this
controller. **Check** verifies a backup without restoring it.

Backups are encrypted to your age recipients. Restoring needs the matching age identity, which Samuori never stores, so
keep it somewhere safe outside Samuori.

## Restore

Restore on the server, into an empty data directory and an empty database:

`samuori-server restore --identity-file KEY --data-dir DIR [--database DSN] ARCHIVE`

The restore checks every file and table against the checksums taken at backup time before anything else runs.
`samuori-server verify-backup` checks an archive without restoring it.
