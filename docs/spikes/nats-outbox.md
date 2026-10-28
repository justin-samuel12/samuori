# Spike: NATS JetStream Restart, Outbox and Reconciliation

Date: 2026-10-19. Status: **Complete; no contradictions with ADR 0017 or ADR 0024.** Code:
`tools/spikes/nats-outbox/`. Raw output is kept in the maintainer's private records.

## Setup
- Embedded `nats-server` v2.15.1 (JetStream, file storage, 2-minute duplicate window), `nats.go` v1.54.0, SQLite via
  `modernc.org/sqlite` v1.60.1, on macOS arm64.
- Every step runs as a **separate OS process**, so crash points are real process exits (`os.Exit` mid-operation).
  The embedded server restarts with each process.
- The database is authoritative. Each job insert writes an outbox row (`msg_id` unique) in the same transaction. The
  relay publishes with `Nats-Msg-Id = msg_id` and marks rows published afterwards. The consumer applies a
  compare-and-swap state transition (`queued → assigned`) and acknowledges only after commit.

## Results

| Scenario | Outcome |
|---|---|
| S1 crash after DB commit, before publish | Unpublished outbox rows were published by the next relay; 5/5 jobs transitioned exactly once |
| S2 crash after publish, before outbox rows marked | The retry republished 3 messages; JetStream reported all 3 as duplicates and dropped them; 3/3 transitioned once |
| S3 consumer crash after DB commit, before ack | The message was redelivered after `AckWait`; the guarded update made it a no-op; 2/2 transitioned once; nothing left pending |
| S4 NATS restart between every step | Stream and durable consumer survived; a second consume found nothing pending; 4/4 transitioned once |
| S5 JetStream store deleted (simulated message loss) | The consumer found nothing; DB reconciliation re-enqueued 3 queued jobs under new message IDs; 3/3 recovered once |

In every scenario, no job had more than one transition.

## Conclusions for the implementation
- Outbox plus `Nats-Msg-Id` deduplication plus compare-and-swap consumers gives at-least-once delivery with
  exactly-once state transitions, as designed.
- Deduplication only covers the stream's duplicate window. The compare-and-swap transition is what guarantees
  idempotency; the window only reduces noise.
- Reconciliation from the database is required and sufficient after message-store loss. Re-enqueue uses fresh
  message IDs so the duplicate window cannot suppress it.
- Not covered: clustered NATS (M11), very large backlogs (performance suite), and lease expiry and fencing. Fencing
  is scheduler logic and is tested as CHA-LSE-01 in M5.
