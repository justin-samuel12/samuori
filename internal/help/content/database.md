---
title: Database
section: Infrastructure
summary: Check the database connection and hand every controller a new password.
roles: [administrator]
order: 235
---
**Infrastructure > Database** shows where Samuori keeps its records, how this controller reaches the database, and whether each live
controller can sign in.

**Test connection** signs in with a new connection from the controller that handles the request.

## Change the database password

Samuori never changes the database password. To rotate it:

1. Change the password at the database, for example in RDS.
2. Open **Database > Update password**, enter the new password twice and a reason.
3. This controller signs in with it first. If that fails, nothing changes.
4. The other controllers verify it within a few seconds and switch. The **Controllers** table shows each result;
   a controller that cannot sign in keeps its old password and shows why.

Connections already open keep working while the password changes. SQLite installations have no password.

A controller that was stopped during the change cannot sign in when it starts. Recover it on that host:

```
read -rs PW && printf '%s' "$PW" | sudo samuori-server set-database-password --config /etc/samuori/server.yml --env-file /etc/samuori/env
sudo systemctl restart samuori-controller
```
