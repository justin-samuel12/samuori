# 0016. Built-in observability; Grafana, Loki and Prometheus optional and external

Status: Accepted (2026-10-19) — owner decision

## Context
D18 requires useful built-in history, metrics and diagnostics without an external stack. Grafana and Loki are
AGPL-3.0-only (network copyleft). Prometheus is Apache-2.0.

## Decision
- Samuori instruments with OpenTelemetry (Apache-2.0) and exposes a Prometheus-format metrics endpoint
  (client_golang, Apache-2.0).
- The built-in UI provides build history, worker/location metrics, audit search and failure diagnostics from Samuori's
  own data.
- Grafana, Loki and Prometheus are **optional external integrations**: Samuori exports to them, may ship
  example dashboards as documentation, and never bundles, embeds, modifies or redistributes Grafana or Loki.
- Do not import Loki or Grafana Go packages except the Apache-2.0 subpackages listed in their `LICENSING.md`; prefer
  OTLP export instead.

## Implementation (WP26, 2026-10-20)
- Diagnostics, audit search, export and retention come from Samuori's own database and UI (Observability & Audit).
- Prometheus metrics use client_golang on a separate, optional listener, bearer-token protected off loopback, with
  no tenant names in labels. OpenTelemetry traces go out over OTLP/HTTP only when an endpoint is configured.
- Audit retention keeps the append-only guarantees. On PostgreSQL only a `SECURITY DEFINER` purge function can
  delete, never events younger than 30 days, and every purge is audited.
- Design and evidence: `docs/architecture/observability.md`.

## Consequences
No AGPL code in Samuori artifacts. Operators who want long-term metrics/log storage bring their own stack.
