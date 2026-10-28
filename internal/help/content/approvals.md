---
title: Approvals
section: Releasing
summary: Decide publication and exception requests that need a second person.
roles: [publisher, security-reviewer, administrator]
order: 80
---
**Approvals** lists requests that need someone other than the requester to decide: publications to destinations
that require approval, repository requests, secret binding requests, base image requests and network access requests. Output exception requests appear in their
own list for administrators.

## Decide

1. Open the request and read what it covers.
2. Choose approve or reject and give a reason of at least 10 characters. The reason is recorded with the decision
   in the audit log.

A network access request opens egress presets or public or internal destinations for one project and build method
only; the request shows the evidence Samuori recorded (a build readiness item or a blocked build). A request for an
**internal** destination is marked as internal network access: approving it lets the project's builds reach that host
inside your network, so approve only hosts the project really needs. Only organization
administrators decide it, and they can revoke the access later on the project page.

You cannot decide your own request. Decided requests move to **Decided**.

## Withdraw

While a repository, secret binding, base image or network access request is still waiting, the person who asked for
it, or a maintainer of the project, can **Withdraw** it here or where it is listed on the project page. It moves to
**Decided** as withdrawn and stays in the audit log. A decided or expired request cannot be withdrawn.
