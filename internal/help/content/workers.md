---
title: Workers and pools
section: Operating Samuori
summary: Pools, isolation profiles, enrollment, self-tests and taking workers out of service.
roles: [operator, administrator]
order: 100
---
Builds of uploaded source run only on **workers**: dedicated machines in **pools**. Open
**Nodes & workers**.

## Pools and trust

- Each pool has an **isolation profile**. Creating a pool approves it in your name; changing the profile or trust
  domain later keeps it approved by whoever saved the change. **Disable pool** stops its workers building uploaded
  source until **Enable pool**. The all-in-one `local` pool starts disabled.
- Enrollment never makes a worker trusted. A new worker is **Unassigned**: it gets no builds, secrets or self-test
  until an administrator assigns it to a pool.
- A worker builds uploaded source only after it passes a **self-test** for its pool with its current configuration
  in the last 24 hours. Self-tests run when a worker is assigned to an enabled pool, when it connects, after its
  configuration changes, and every 20 hours.
- The **Can build uploaded source** column says why a worker is not eligible, for example a stale self-test.

## Add a worker (administrators)

1. Choose **Enroll a worker** at the top of the page, or **Add a worker** under [Servers](help:servers). Samuori shows
   one join code and the command `sudo ./samuori-join worker <code>`.
2. Run it from the release folder on the worker's own VM. It installs and enrolls the worker in one step. On a host
   already installed with `install.sh --worker-only`, run `sudo samuori-enroll-worker <CA pin>` and paste the token
   instead.
3. The worker appears as **Unassigned**. Choose **Assign to pool** and pick its pool.

**Move to pool** reassigns a worker. Its trust does not move with it: it must pass a new self-test for the new pool.
A worker running builds cannot move; drain it first.

Locations stay hidden while there is only one (`default`). To add another, choose **Put this pool in another
location** when creating or editing a pool.

## Take a worker out of service

Choose an action on the worker:

| Action | Effect |
|---|---|
| **Cordon** | Takes no new builds. **Uncordon** reverses it |
| **Drain** | Finishes running builds, then stops |
| **Quarantine** | Stops scheduling for investigation |
| **Remove** (administrators) | Finishes running builds, has the host delete Samuori (services, files, keys, users and the folder it was installed from), then revokes the worker. Coming back means installing again. If the host was not connected, it is revoked and shown as not confirmed wiped: run `sudo samuori-uninstall` on it, or terminate it |
| **Revoke** | Ends the worker's identity now; it must enroll again. Images it built can be quarantined |

A worker that could not clean up after a build quarantines itself; check its logs and restart it.

The **Build queue** (operators) shows builds waiting for or running on workers.
