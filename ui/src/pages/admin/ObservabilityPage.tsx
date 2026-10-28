import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Checkbox, TextField } from "../../components/Field";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";
import { isOrgAdmin, useMe, type Me } from "../../session/session";

type Diagnostics = Schemas["Diagnostics"];
type AuditEvent = Schemas["AuditEvent"];

function orgRole(me: Me, role: string): boolean {
  return me.roles.some(
    (r) => r.role === role && r.scopeType === "organization",
  );
}

export function ObservabilityPage() {
  const me = useMe();
  const admin = isOrgAdmin(me);
  const diagnostics = admin || orgRole(me, "operator");
  const audit = admin || orgRole(me, "security-reviewer");
  return (
    <>
      <PageHeader
        help="security-review"
        title="Observability & Audit"
        description="Operational state and the audit log, read from Samuori's own database. No external monitoring system is needed."
      />
      {diagnostics && <DiagnosticsSection />}
      {audit && <AuditSection />}
      {audit && <RetentionSection canEdit={admin} />}
    </>
  );
}

function counts(m: Record<string, number> | undefined): string {
  const entries = Object.entries(m ?? {}).sort(([a], [b]) =>
    a.localeCompare(b),
  );
  return entries.length === 0
    ? "none"
    : entries.map(([k, v]) => `${v} ${k.replace(/_/g, " ")}`).join(", ");
}

function age(seconds: number): string {
  if (seconds <= 0) return "none waiting";
  if (seconds < 120) return `${Math.round(seconds)} s`;
  if (seconds < 7200) return `${Math.round(seconds / 60)} min`;
  return `${Math.round(seconds / 3600)} h`;
}

function DiagnosticsSection() {
  const [d, setD] = useState<Diagnostics | null>();
  const load = useCallback(
    async () => setD((await api.GET("/system/diagnostics")).data ?? null),
    [],
  );
  useLoader(load);
  if (d === undefined) return <Loading />;
  if (d === null) return <Alert>Diagnostics could not be loaded.</Alert>;
  return (
    <section aria-labelledby="diag-heading">
      <h2 id="diag-heading">Diagnostics</h2>
      <p className="samuori-muted">
        Measured {new Date(d.at).toLocaleString()}.
      </p>
      <dl className="samuori-details">
        <dt>Health</dt>
        <dd>
          <span
            className={`samuori-status samuori-status--${d.health.status === "ok" ? "ok" : "unavailable"}`}
          >
            {d.health.status}
          </span>{" "}
          {(d.health.components ?? [])
            .filter((c) => c.status !== "ok")
            .map((c) => `${c.name} ${c.status}`)
            .join(", ")}
        </dd>
        <dt>Builds now</dt>
        <dd>{counts(d.builds.byState)}</dd>
        <dt>Oldest queued build</dt>
        <dd>{age(d.builds.queuedOldestSeconds)}</dd>
        <dt>Builds finished (24 h)</dt>
        <dd>{counts(d.builds.last24h)}</dd>
        <dt>Top failure classes (24 h)</dt>
        <dd>
          {d.builds.failureClasses24h.length === 0
            ? "none"
            : d.builds.failureClasses24h
                .map((c) => `${c.key.replace(/_/g, " ")} (${c.count})`)
                .join(", ")}
        </dd>
        <dt>Validations</dt>
        <dd>
          {counts(d.validations.byState)}; last 24 h:{" "}
          {counts(d.validations.last24h)}
        </dd>
        <dt>Publications</dt>
        <dd>
          {counts(d.publications.byState)}; last 24 h:{" "}
          {counts(d.publications.last24h)}
        </dd>
        <dt>Workers</dt>
        <dd>
          {counts(d.workers.byEnrollment)}; {d.workers.connected} connected,{" "}
          {d.workers.stale} without a recent heartbeat
        </dd>
        <dt>Outbox</dt>
        <dd>
          {d.outbox.pending} pending, oldest {age(d.outbox.oldestSeconds)}
        </dd>
        <dt>External outputs</dt>
        <dd>
          Metrics {d.telemetry.metricsEnabled ? "on" : "off"}, tracing{" "}
          {d.telemetry.tracingEnabled ? "on" : "off"}, scheduled audit export
          directory{" "}
          {d.telemetry.auditExportDirectory ? "configured" : "not configured"}
        </dd>
      </dl>
      <Table
        label="Worker pools"
        rows={d.workers.pools}
        empty="No worker pools yet."
        columns={[
          {
            id: "name",
            name: "Pool",
            isRowHeader: true,
            render: (p) => p.name,
          },
          ...(new Set(d.workers.pools.map((p) => p.location)).size > 1
            ? [
                {
                  id: "location",
                  name: "Location",
                  render: (p: (typeof d.workers.pools)[number]) => p.location,
                },
              ]
            : []),
          {
            id: "profile",
            name: "Isolation profile",
            render: (p) =>
              `${p.isolationProfile}${p.profileApproved ? "" : " (disabled)"}`,
          },
          {
            id: "workers",
            name: "Workers",
            render: (p) =>
              `${p.enrolled} enrolled, ${p.connected} connected${p.stale ? `, ${p.stale} stale` : ""}`,
          },
        ]}
      />
      <Table
        label="Registry connection tests"
        rows={d.registries.map((r) => ({
          ...r,
          id: `${r.registry}/${r.profile}`,
        }))}
        empty="No registry profiles yet."
        columns={[
          {
            id: "registry",
            name: "Registry",
            isRowHeader: true,
            render: (r) => r.registry,
          },
          { id: "profile", name: "Profile", render: (r) => r.profile },
          {
            id: "status",
            name: "Last test",
            render: (r) => (
              <span
                className={`samuori-status samuori-status--${r.status === "ok" ? "ok" : r.status === "failed" ? "unavailable" : "pending"}`}
              >
                {r.status}
                {r.testedAt ? `, ${new Date(r.testedAt).toLocaleString()}` : ""}
              </span>
            ),
          },
        ]}
      />
    </section>
  );
}

interface AuditQuery {
  action: string;
  resourceType: string;
  actorId: string;
  q: string;
}

const emptyQuery: AuditQuery = {
  action: "",
  resourceType: "",
  actorId: "",
  q: "",
};

function queryParams(f: AuditQuery): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(f)) if (v.trim()) out[k] = v.trim();
  return out;
}

function AuditSection() {
  const [draft, setDraft] = useState<AuditQuery>(emptyQuery);
  const [applied, setApplied] = useState<AuditQuery>(emptyQuery);
  const [events, setEvents] = useState<AuditEvent[] | null>();
  const [next, setNext] = useState<string | null>(null);
  const [error, setError] = useState<string>();
  const fetchPage = useCallback(async (f: AuditQuery, cursor?: string) => {
    const { data, error } = await api.GET("/audit-events", {
      params: {
        query: { ...queryParams(f), limit: 50, ...(cursor ? { cursor } : {}) },
      },
    });
    if (!data) {
      setError(describeError(error));
      return;
    }
    setError(undefined);
    setEvents((prev) =>
      cursor ? [...(prev ?? []), ...data.items] : data.items,
    );
    setNext(data.nextCursor ?? null);
  }, []);
  const load = useCallback(() => fetchPage(emptyQuery), [fetchPage]);
  useLoader(load);
  function search(e: FormEvent) {
    e.preventDefault();
    setApplied(draft);
    void fetchPage(draft);
  }
  const exportHref =
    "/api/v1/audit-events/export?" +
    new URLSearchParams(queryParams(applied)).toString();
  return (
    <section aria-labelledby="audit-heading">
      <h2 id="audit-heading">Audit log</h2>
      <p className="samuori-muted">
        Every change is recorded here with who made it. Events never hold secret
        values. Action matches exactly, or as a prefix ending in *.
      </p>
      <Form onSubmit={search} className="samuori-form samuori-form--inline">
        <TextField
          label="Action"
          placeholder="publication.*"
          value={draft.action}
          onChange={(v) => setDraft({ ...draft, action: v })}
        />
        <TextField
          label="Resource type"
          value={draft.resourceType}
          onChange={(v) => setDraft({ ...draft, resourceType: v })}
        />
        <TextField
          label="Actor ID"
          value={draft.actorId}
          onChange={(v) => setDraft({ ...draft, actorId: v })}
        />
        <TextField
          label="Text in details"
          value={draft.q}
          onChange={(v) => setDraft({ ...draft, q: v })}
        />
        <Button type="submit">Search</Button>
      </Form>
      <p>
        <a className="samuori-link" href={exportHref} download>
          Export matching events (JSON Lines)
        </a>
      </p>
      {error && <Alert>{error}</Alert>}
      {events === undefined ? (
        <Loading />
      ) : (
        <Table
          label="Audit events"
          rows={events ?? []}
          dependencies={[events]}
          empty="No events match."
          columns={[
            {
              id: "at",
              name: "When",
              isRowHeader: true,
              render: (e) => new Date(e.at).toLocaleString(),
            },
            { id: "action", name: "Action", render: (e) => e.action },
            {
              id: "actor",
              name: "Actor",
              render: (e) =>
                `${e.actor.type}${e.actor.id ? ` ${e.actor.id}` : ""}`,
            },
            {
              id: "resource",
              name: "Resource",
              render: (e) =>
                `${e.resourceType}${e.resourceId ? ` ${e.resourceId}` : ""}`,
            },
            { id: "outcome", name: "Outcome", render: (e) => e.outcome },
            { id: "reason", name: "Details", render: (e) => e.reason ?? "" },
          ]}
        />
      )}
      {next && (
        <Button
          variant="secondary"
          onPress={() => void fetchPage(applied, next)}
        >
          Load more
        </Button>
      )}
    </section>
  );
}

function RetentionSection({ canEdit }: { canEdit: boolean }) {
  const [policy, setPolicy] = useState<Schemas["AuditPolicy"] | null>();
  const [days, setDays] = useState("0");
  const [exportEnabled, setExportEnabled] = useState(false);
  const [requireExport, setRequireExport] = useState(true);
  const [message, setMessage] = useState<{
    tone: "error" | "success";
    text: string;
  }>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/audit-policy");
    setPolicy(data ?? null);
    if (data) {
      setDays(String(data.retentionDays));
      setExportEnabled(data.exportEnabled);
      setRequireExport(data.requireExport);
    }
  }, []);
  useLoader(load);
  async function save(e: FormEvent) {
    e.preventDefault();
    const { data, error } = await api.PUT("/audit-policy", {
      body: { retentionDays: Number(days) || 0, exportEnabled, requireExport },
    });
    if (!data) {
      setMessage({ tone: "error", text: describeError(error) });
      return;
    }
    setPolicy(data);
    setMessage({ tone: "success", text: "Audit policy saved." });
  }
  if (policy === undefined) return <Loading />;
  if (policy === null)
    return <Alert>The audit policy could not be loaded.</Alert>;
  return (
    <section aria-labelledby="retention-heading">
      <h2 id="retention-heading">Audit retention and export</h2>
      <dl className="samuori-details">
        <dt>Retention</dt>
        <dd>
          {policy.retentionDays === 0
            ? "Events are kept forever."
            : `Events older than ${policy.retentionDays} days are purged${policy.requireExport ? " once exported" : ""}.`}
        </dd>
        <dt>Scheduled export</dt>
        <dd>
          {policy.exportEnabled ? "On" : "Off"}
          {policy.exportedThrough
            ? `; exported through ${new Date(policy.exportedThrough).toLocaleString()}`
            : ""}
        </dd>
        {policy.lastExportError && (
          <>
            <dt>Last export error</dt>
            <dd>{policy.lastExportError}</dd>
          </>
        )}
        <dt>Last purge</dt>
        <dd>
          {policy.lastPurgeAt
            ? `${new Date(policy.lastPurgeAt).toLocaleString()}, ${policy.lastPurgeCount} event(s)`
            : "never"}
        </dd>
      </dl>
      {canEdit && (
        <Form onSubmit={(e) => void save(e)} className="samuori-form">
          <TextField
            label="Retention in days"
            description="0 keeps events forever. Otherwise at least 30; younger events are never purged."
            inputMode="numeric"
            value={days}
            onChange={setDays}
          />
          <Checkbox isSelected={exportEnabled} onChange={setExportEnabled}>
            Export new events hourly to JSON Lines files on the controller
          </Checkbox>
          <Checkbox isSelected={requireExport} onChange={setRequireExport}>
            Purge only events that have been exported
          </Checkbox>
          {message && <Alert tone={message.tone}>{message.text}</Alert>}
          <Button type="submit">Save policy</Button>
        </Form>
      )}
    </section>
  );
}
