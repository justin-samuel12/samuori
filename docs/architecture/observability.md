# Observability and audit

Status: **Implemented (WP26).** Decision: ADR 0016 (built-in observability; Grafana, Loki and Prometheus are optional
and external). Product decision: D18. Screen: Administration → Observability & Audit.

Samuori answers its operational questions from its own database. Diagnostics, audit search, export and retention need no
external monitoring system. Prometheus metrics and OpenTelemetry traces are optional outputs for operators who
already run those tools. Both are off by default.

## Diagnostics

`GET /system/diagnostics` (administrators and operators of the organization) reads the current state:

- **Health:** the same real checks as `/system/health`.
- **Builds:** runs by state, the age of the oldest queued run, runs finished in the last 24 hours, and the top five
  failure classes.
- **Validations and publications:** by state, plus verdicts and outcomes in the last 24 hours.
- **Workers:** by enrollment state, how many hold a live channel, and how many enrolled workers have sent no
  heartbeat for more than two minutes (stale). Each pool shows its profile, whether the profile is approved, and
  enrolled, connected and stale workers.
- **Registries:** each push or pull profile's last connection test (`ok`, `failed` or `untested`).
- **Outbox:** pending messages and the age of the oldest.
- **Telemetry:** whether metrics, tracing and the scheduled audit export directory are configured.

Everything is scoped to the caller's organization and measured when requested. Nothing is cached or estimated.
`internal/observability` builds the snapshot; it only reads.

## Metrics

Prometheus text format on a **separate listener**, off unless `observability.metricsListen` is set:

```yaml
observability:
  metricsListen: 127.0.0.1:9464
  metricsTokenFile: /etc/samuori/metrics-token   # required off loopback; mode 0600, at least 16 characters
```

Scrapes off loopback must send `Authorization: Bearer <token>`. The API listener never serves metrics.

| Metric | Labels | Meaning |
|---|---|---|
| `samuori_http_requests_total` | `operation`, `method`, `code` | API requests by OpenAPI operation ID |
| `samuori_http_request_duration_seconds` | `operation` | API latency histogram |
| `samuori_builds` / `samuori_builds_finished_24h` | `state` | Build runs now, and terminal in 24 hours |
| `samuori_build_queue_oldest_seconds` | | Age of the oldest queued run |
| `samuori_build_failures_24h` | `class` | Failure classes, top five |
| `samuori_validations` / `samuori_validations_finished_24h` | `state` / `verdict` | Validation runs |
| `samuori_publications` / `samuori_publications_finished_24h` | `state` | Publications |
| `samuori_workers` | `enrollment` | Workers by enrollment state |
| `samuori_workers_connected`, `samuori_workers_stale` | | Live channels; enrolled workers without a recent heartbeat |
| `samuori_pool_workers` | `pool_id`, `state` | Per pool: enrolled, connected, stale |
| `samuori_pool_profile_approved` | `pool_id`, `profile` | Whether the pool's isolation profile is approved |
| `samuori_registry_profiles` | `status` | Profiles by last connection test |
| `samuori_outbox_pending`, `samuori_outbox_oldest_seconds` | | Transactional outbox backlog |
| `samuori_health_component_up`, `samuori_health_ok` | `component` | Health checks |
| `samuori_metrics_snapshot_errors_total` | | Scrapes whose database snapshot failed |
| `go_*`, `process_*` | | Go runtime and process |

Metrics cover the whole installation. **Labels never carry organization, team, project, user, pool or registry
names** (SEC-OBS-02), so a scrape reveals no tenant's names. Pools appear by ID only. Gauges come from one database
snapshot, cached for ten seconds so frequent scrapes do not load the database.

## Tracing

OpenTelemetry traces over OTLP/HTTP, off unless an endpoint is set:

```yaml
observability:
  tracing:
    endpoint: http://collector.example:4318/v1/traces   # the full traces URL
    sampleRatio: 0.25                                     # of new traces; incoming sampled parents are honoured
```

- **API requests** get a server span named after the OpenAPI operation. It records the method, operation and
  status code, never the query string, headers or body (SEC-OBS-01). Incoming W3C `traceparent` headers are
  honoured.
- **Background work:** `scheduler.dispatch`, `scheduler.assign`, `validation.process` and `publication.process` get
  spans with the record ID. Domain packages use the small `telemetry.Span` wrapper, never the OpenTelemetry API
  directly.
- **Workers:** each assignment carries the W3C `traceparent` of its `scheduler.assign` span. A worker started with
  `--tracing-endpoint` (and optionally `--tracing-sample-ratio`) continues that trace with `worker.attempt` and the
  child spans `worker.fetch-source`, `worker.build` and `worker.upload`. Worker spans carry build and attempt IDs and
  an outcome class such as `failed-application`, never error text, which can contain build output.
- **Logs:** each request log line carries `operation` and, when the request is traced, `trace_id`.

## Audit search

`GET /audit-events` (organization administrators and security reviewers) returns the organization's events, newest
first, with cursor paging. The filters are:

- actor ID, resource type and ID, outcome, request ID;
- action, exact or as a prefix (`publication.*`);
- text in the details, matched literally (`%` and `_` are not wildcards);
- a `since`/`until` time range.

Events never hold secret values (TM-90), so search cannot reveal them. Another organization's events are never
returned.

## Export

- **On demand:** `GET /audit-events/export` with the same filters returns JSON Lines, oldest first, up to 100,000
  events. `X-Samuori-Truncated: true` means more matched. Every export is itself audited (`audit.export`), with the
  count.
- **Scheduled:** when the organization's policy enables it, the controller writes new events every hour to
  `<auditExportDir>/<organization ID>/audit-<from>-<until>.jsonl`, with a `.sha256` sidecar. The default directory is
  `audit-export` in the data directory. Files are written atomically. Windows are half-open (`from ≤ at < until`), so
  consecutive files neither repeat nor skip an event. Each window ends five minutes before now, so transactions
  still committing are not missed. Shipping the files elsewhere (object storage, a SIEM) is the operator's choice.
  The last export time and any error appear on the policy.

## Retention

Each organization has a policy (`GET`/`PUT /audit-policy`; only administrators change it, and changes are audited):

- **Retention days:** `0` keeps events forever (the default). Otherwise 30 to 36,500.
- **Purge only exported events** (on by default): events are purged only after scheduled export has written them,
  so this needs scheduled export.

The purge runs hourly. It deletes the organization's events older than the retention period, and with "purge only
exported events", only those before the export checkpoint. Every purge is recorded as an `audit.purge` event with
the count and cutoff.

**Tamper protection is kept (TM-91, INT-AUD-02, INT-AUD-03).**

- **PostgreSQL:** the application role still has no `UPDATE`, `DELETE` or `TRUNCATE` on `audit_events`, and the
  trigger still rejects updates, truncation and deletes. The only way to delete is `samuori_audit_purge(org, cutoff)`,
  a `SECURITY DEFINER` function owned by the schema owner. It refuses any cutoff younger than **30 days**, and only
  it sets the transaction-local flag the trigger accepts. Setting that flag yourself does not help the application
  role, which has no delete privilege.
- **SQLite** has no roles. The delete trigger allows a delete only while an open purge record exists for that
  organization and only before its cutoff. This protects against application bugs, not against someone with the
  database file (documented limitation, as before).

**Residual risk:** a compromised controller can still purge events older than 30 days of its own organization. To
keep a copy the controller cannot alter, ship scheduled exports off the host.

## UI

Administration → **Observability & Audit**:

- **Diagnostics:** shown to administrators and operators, with a refresh action.
- **Audit log:** shown to administrators and security reviewers. It has filters, Load more and an export link for
  the current filters.
- **Audit retention and export:** security reviewers see the policy; administrators can change it.

## Evidence

> History: the tests named below ran at earlier commits and were removed (ADR 0039); restored focused tests are
> marked in `docs/developers/security-test-catalog.md`.

- `internal/audit`: search filters, literal text matching, paging, invalid cursors, policy validation, deletes refused
  outside a purge, exports with matching checksums, the minimum retention, purges audited, and windowed exports
  continuing from the checkpoint. On real PostgreSQL (`make test-integration`), `TestPurgePostgres` covers INT-AUD-03:
  - the application role cannot delete, even after setting the purge flag;
  - the owner is blocked by the trigger outside a purge;
  - the function refuses a cutoff younger than 30 days;
  - the application role's purge works;
  - `TRUNCATE` stays blocked.
- `internal/observability`: snapshot counts, queue age, failure classes, connected and stale workers, organization
  scoping, and metric labels free of tenant names (SEC-OBS-02). Every query also runs on PostgreSQL as the
  application role.
- `internal/telemetry`: the metrics bearer token, request and gauge metrics, snapshot caching, operation-named server
  spans continuing an incoming trace, child spans, and no query, header or body secrets in spans (SEC-OBS-01).
- `internal/telemetry/interop_test.go` (dev VM, `make test-buildkit`): Prometheus 3.15.0 scrapes the metrics listener
  with the bearer token and is refused without it; the OpenTelemetry Collector 0.162.0 receives Samuori's spans over
  OTLP/HTTP, including a worker span that continues a propagated trace.
- `internal/builds` (`TestTracePropagation`): one build's assignment and worker spans share a trace, and worker spans
  hold no error text.
- `internal/api`: role checks, search and paging through the API, the JSON Lines export and its audit event, policy
  validation, and the diagnostics contract.
- **Live upgrade on the clean VM:** migrations 00020 and 00021 applied to an existing install. The earlier
  publication and its audit history survived, and diagnostics report its builds, worker and pool. With
  `SAMUORI_METRICS_LISTEN=127.0.0.1:9464`, the metrics listener served the gauges above. The API port served no
  metrics, and the listener was gone once the setting was removed.
- `ui/e2e/09-supply-chain.spec.ts`: an administrator opens Observability & Audit, filters the audit log by a prefix,
  is refused a seven-day retention, and saves a policy, with axe checks.

## Limitations

- Trace context does not reach workers, and worker-side spans and metrics are not collected.
- No log shipping: the controller logs JSON to stderr for the operator's collector.
- Scheduled export writes files only. Object storage and syslog targets are not built in.
- Metrics and diagnostics are point-in-time, with no built-in history beyond the 24-hour counts. Long-term metric
  storage belongs to the operator's Prometheus (ADR 0016).
- Not yet scraped by a Prometheus server or sent to an OpenTelemetry collector. The exposition was read live with
  curl, and spans are tested with the SDK's in-memory exporter.
