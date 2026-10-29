import { useCallback, useEffect, useState } from "react";
import { Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";
import { isOrgAdmin, useMe } from "../../session/session";
import { SelfUpdate } from "./SelfUpdate";

type Status = Schemas["UpgradeStatus"];

function controllerSummary(status: Status): string {
  const live = status.controllers.filter((c) => c.live);
  const releases = [...new Set(live.map((c) => c.version))].sort();
  const outside = live.filter((c) => !c.compatible).length;
  return `${live.length} of ${status.controllers.length} controller${status.controllers.length === 1 ? "" : "s"} running${releases.length ? `, release ${releases.join(", ")}` : ""}${outside ? `; ${outside} outside the upgrade window` : ""}.`;
}

export function UpgradesPage() {
  const me = useMe();
  const [status, setStatus] = useState<Status>();
  const [failure, setFailure] = useState<string>();
  const load = useCallback(async () => {
    const { data, error, response } = await api
      .GET("/system/upgrades", { signal: AbortSignal.timeout(20000) })
      .catch(() => ({
        data: undefined,
        error: undefined,
        response: undefined,
      }));
    if (data) {
      setStatus(data);
      setFailure(undefined);
    } else
      setFailure(
        `${describeError(error, "The controller did not answer within 20 seconds.")} (HTTP ${response?.status ?? "no response"})`,
      );
  }, []);
  useLoader(load);
  const active = status?.removals?.some((r) => r.state !== "removed") ?? false;
  useEffect(() => {
    if (!active) return;
    const t = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(t);
  }, [active, load]);
  if (!status)
    return failure ? (
      <>
        <PageHeader
          help="upgrades"
          title="Upgrades & diagnostics"
          description="Controller, database schema and worker versions, checked against the supported version matrix."
        />
        <Alert>
          The upgrade status could not be read: {failure} Reload to try again;
          the controller log has the cause. Self-update below still works.
        </Alert>
        {isOrgAdmin(me) && <SelfUpdate current="" />}
      </>
    ) : (
      <Loading />
    );
  const admin = isOrgAdmin(me);
  const skew = status.controllers.some((c) => c.live && !c.compatible);
  const oldWorkers = status.workers.filter((w) => !w.compatible);
  return (
    <>
      <PageHeader
        help="upgrades"
        title="Upgrades & diagnostics"
        description="Controller, database schema and worker versions, checked against the supported version matrix."
      />
      <dl className="samuori-details">
        <dt>This controller</dt>
        <dd>
          <code>{status.version}</code>
        </dd>
        <dt>Database schema</dt>
        <dd>
          {status.schemaApplied}
          {status.schemaApplied === status.schemaKnown
            ? " (current for this release)"
            : ` (this release knows up to ${status.schemaKnown})`}
        </dd>
      </dl>
      {skew && (
        <Alert tone="warning">
          A controller runs a release outside the upgrade window. Upgrade
          controllers one minor version at a time.
        </Alert>
      )}
      {oldWorkers.length > 0 && (
        <Alert tone="warning">
          {oldWorkers.reduce((n, w) => n + w.count, 0)} worker(s) run a release
          this controller does not accept; they receive no builds until
          upgraded.
        </Alert>
      )}
      <h2>Controllers</h2>
      <p>
        {controllerSummary(status)}
        {admin && (
          <>
            {" "}
            <Link href="/admin/servers" className="samuori-link">
              Controllers under Servers
            </Link>
          </>
        )}
      </p>
      <h2>Workers</h2>
      <Table
        label="Worker releases"
        rows={status.workers.map((w) => ({ ...w, id: w.version }))}
        empty="No worker is enrolled. Add one under Infrastructure > Servers."
        columns={[
          {
            id: "version",
            name: "Release",
            isRowHeader: true,
            render: (w) => w.version || "unknown",
          },
          {
            id: "count",
            name: "Workers",
            align: "end",
            render: (w) => w.count,
          },
          {
            id: "compatible",
            name: "Accepted",
            render: (w) => (w.compatible ? "Yes" : `No: ${w.reason ?? ""}`),
          },
        ]}
      />
      <h2>Schema upgrades</h2>
      {status.history.length === 0 ? (
        <p>No schema upgrade has been applied to this installation.</p>
      ) : (
        <ul>
          {status.history.map((h) => (
            <li key={h.at}>
              {new Date(h.at).toLocaleString()}: {h.reason}
            </li>
          ))}
        </ul>
      )}
      <p>
        To upgrade: back up, upgrade controllers one at a time (each migrates
        the schema and writes a backup first when backups are configured), then
        drain and upgrade workers. See the upgrade guide in the operator
        documentation.
      </p>
      {admin && <SelfUpdate current={status.version} />}
    </>
  );
}
