# Notifications

Status: **Implemented (WP28)** for the in-app inbox, signed webhooks, Slack, Teams and email. Slack and Teams delivery
has been tested only against local receivers; delivery to real workspaces is an outstanding validation
(`docs/roadmap.md`). Product decision: D21. Screens: **Notifications** (every user) and Administration →
**Notifications**.

## Events

Notifications come from the audit log. Every state change already writes an audit record in the same transaction,
so an event is never announced for a change that rolled back, and a committed change is never missed.

`notify.Dispatcher` reads new audit records in `(at, id)` order behind a cursor. It stays 10 seconds (`Lag`) behind
the clock so that a transaction that commits late with an earlier timestamp is still seen. On its first run the
cursor starts at the current time; past records are history and are not announced.

| Event | Audit record | Inbox recipients |
|---|---|---|
| `build.succeeded`, `build.failed` | `build.finished` (failed, timed out, expired in the queue) | the requester |
| `validation.passed`, `validation.failed`, `validation.blocked` | `validation.complete` | the build's requester |
| `publication.published`, `publication.failed`, `publication.blocked` | the same action | the requester |
| `approval.requested` | `approval.request` | organization administrators |
| `approval.decided` | `approval.decide` | the requester |
| `exception.requested` | `exception.request` | organization administrators |
| `worker.revoked` | `worker.revoke` | organization administrators |
| `artifact.quarantined` | `artifact.quarantine` | organization administrators |

Cancelled builds are not announced, because the person who cancelled already knows. Builds blocked at admission are
not announced either; the requester sees the refusal when they start the build.

## Message content (SEC-NOT-01)

A message has a title, one line of text, a link, the time, the resource type and ID, and the project. It is built
only from structured fields: states, the project name, the destination reference of a published image, and machine
labels such as a failure class (`build_error` becomes "build error"; anything that is not a lower-case slug is
dropped). Messages start with "Project: <name>." and, for builds and checks, "App: <name>." (the BuildSpec `metadata.name`,
also in the `app` field), since a project can hold several images. Build messages add who requested the build (display name, with control characters and chat markup such
as `<`, `>`, `*`, `_` and backticks removed; nothing for pipelines), how long it took from the request and the image
size; failed or blocked checks list up to six check IDs. Free text such as failure details, log lines, reasons or scanner output is never read, so messages cannot
carry secrets or log excerpts. Links point back into Samuori, where server-side authorization decides what the reader
sees. Links are absolute when `externalUrl` is set.

## Inbox

Every user sees their own entries at `GET /notifications` and marks them read with `POST /notifications/read`
(one entry or all). Users read only their own inbox; there is no administrator view of other users' inboxes.

## Channels and rules

Organization administrators manage channels and rules. Every change is audited.

| Kind | Target | Secret |
|---|---|---|
| `webhook` | HTTPS URL (HTTP only on loopback) | an HMAC key that Samuori generates and shows once |
| `slack` | Slack incoming webhook | the URL itself, stored in the secret store for the controller only |
| `teams` | Teams workflow webhook (Adaptive Card) | the URL itself, as for Slack |
| `email` | 1 to 50 addresses | none; uses the controller's SMTP settings |

A rule routes a list of event types to one channel, for the whole organization, one team or one project. A
disabled channel receives nothing. **Send test** sends a sample message and audits the outcome.

**Edit** (`PUT /notification-channels/{id}`, `PUT /notification-rules/{id}`) applies the checks of creation. A
channel's kind is fixed; a webhook keeps its signing key; a Slack or Teams channel keeps its stored URL unless a new
one is given, which becomes a new controller-only secret (the old one is then listed as no longer used under
Security & Secrets → Secrets) and is never returned. Edits are audited without secrets.

### Deleting a channel

`DELETE /notification-channels/{id}` (administrator) deletes the channel and its delivery log in one transaction,
audited as `notification-channel.delete` with the number of deliveries removed. It is refused with 409
`channel_in_use` while any rule routes events to the channel; the refusal lists those rules, and the UI offers to
disable the channel instead. Pending deliveries of the channel are dropped. A webhook signing key or Slack or Teams
URL the channel held stays as a controller-only secret that nothing uses, listed under Security & Secrets → Secrets
where an administrator may delete it.

### Webhook signatures

Each webhook request carries:

- `X-Samuori-Event`: the event type;
- `X-Samuori-Delivery`: the event ID, the same across retries, for de-duplication;
- `X-Samuori-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256>` over `<t>.<raw body>` with the channel key.

Receivers recompute the HMAC with a constant-time comparison, reject timestamps more than five minutes old, and
discard repeated delivery IDs.

### Network rules

Webhook, Slack and Teams requests go through the address guard (SEC-NET-03). Link-local and metadata addresses are
always refused. Loopback and private ranges are refused unless the channel allows private networks. The guard checks
every resolved address at connect time, so DNS answers cannot bypass it. Redirects are never followed, and proxy
environment variables are ignored. Errors never include a Slack or Teams URL.

Email needs `smtp.host`. STARTTLS is required unless the relay is on loopback. Header values are stripped of line
breaks, so a title cannot add headers.

## Delivery, retries and dead letters

The dispatcher writes one delivery per matching channel and event. `UNIQUE (channel_id, event_id)` and
`UNIQUE (user_id, event_id)` make dispatch exactly-once even if several controllers run it.

`RunDelivery` attempts due deliveries every 10 seconds. A delivery is claimed with a conditional update that hides
it for five minutes, so two controllers never send it at the same time. After a failure it waits 30 seconds, then
2 minutes, 10 minutes, 1 hour and 6 hours. After the sixth failed attempt it becomes **dead**, and the dead letter is
audited (`notification.dead-letter`). An administrator can retry a dead delivery, which is audited
(`notification.retry`). The delivery log shows state, attempts, the next attempt and the last error.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `smtp.host` | empty | SMTP server; email channels are refused while empty |
| `smtp.port` | 587 | |
| `smtp.from` | empty | sender address |
| `smtp.username` | empty | |
| `smtp.passwordFile` | empty | file holding the SMTP password (mode 0600) |

## Tests

> History: the tests named below ran at earlier commits and were removed (ADR 0039); restored focused tests are
> marked in `docs/developers/security-test-catalog.md`.

- `internal/notify/notify_test.go`: routing by event and scope, inbox recipients, SEC-NOT-01, exactly-once dispatch,
  backoff, dead-lettering, retry and its audit, no replay of history, and channel validation.
- `internal/adapters/notifysend/notifysend_test.go`: webhook signature, the address guard, no redirects, no secret
  URL in errors, Slack and Teams payloads, and SMTP header injection.
- `internal/api/notifications_test.go`: role checks, the reveal-once signing key, and validation.
- `ui/e2e/05-admin-infra.spec.ts`: an administrator adds a webhook channel and a rule; every user can open the inbox.
