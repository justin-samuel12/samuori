---
title: Servers
section: Operating Samuori
summary: Adding, accepting and removing controllers and workers.
roles: [administrator]
order: 95
---
**Servers** lists every controller and worker of this installation in one place.

## Add a controller

Choose **Create a join code** under **Add a controller**. Samuori shows one code, valid once and for at most an hour, and
the command to run from the release folder on the new host:

```
sudo ./samuori-join controller <code>
```

The host uses its own DNS name (add `--host NAME` to choose another) and prints a verification code. Its request
appears under **Join requests**. **Accept** only if the code matches what the host shows; **Reject** anything you do
not expect. Only an accepted host receives the cluster's configuration and keys.

## Add a worker

Create a pool first under [Workers and pools](help:workers) if none exists. Choose **Add a worker**. Samuori shows one
code, valid once and for an hour, and the command for the worker VM:

```
sudo ./samuori-join worker <code>
```

It checks that it is talking to this installation before installing anything, then installs and enrolls the worker.
The worker appears as **Unassigned**: it gets no builds, secrets or self-test until you choose **Assign to pool**.
The CA pin and token below the command are for a host already installed with `install.sh --worker-only`
(`sudo samuori-enroll-worker <CA pin>`, then paste the token).

A code is safe to paste on the command line: it works once, expires within the hour and grants nothing until you
accept the controller or assign the worker. An unused code stays listed as unused; a worker you did not expect stays
unassigned and can be removed.

## Remove a server

**Remove** asks for a reason. A worker finishes its running builds, its host deletes Samuori, and it is revoked. A
controller hands over leadership first; removing the last controller, or one that would leave messaging without a
majority, is refused. A host that does not answer is revoked and shown as not confirmed wiped: run
`sudo samuori-uninstall` on it or terminate it. Coming back means installing and joining again.
