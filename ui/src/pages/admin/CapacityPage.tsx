import { useQuery } from "@tanstack/react-query";
import { Link } from "react-aria-components";
import { HardDrive, Hammer, MemoryStick, Upload } from "lucide-react";
import { api, describeError, type Schemas } from "../../api/client";
import {
  Alert,
  Badge,
  Loading,
  PageHeader,
  Section,
  StatCard,
} from "../../components/Feedback";
import { Table } from "../../components/Table";
import { bytes } from "../../format";

type Overview = Schemas["CapacityOverview"];
type Host = Schemas["HostCapacity"];
type Tone = "ok" | "warn" | "bad";

/** Reports older than this are shown as stale: controllers report every 30 s, workers every 15 s. */
const staleAfter = 2 * 60_000;

function diskTone(free: number, total: number): Tone {
  const share = total ? free / total : 0;
  if (share < 0.05 || free < 1 << 30) return "bad";
  if (share < 0.15 || free < 5 * (1 << 30)) return "warn";
  return "ok";
}

function memoryTone(h: Host): Tone {
  if (!h.memoryTotalBytes) return "ok";
  const share = h.memoryAvailableBytes / h.memoryTotalBytes;
  return share < 0.05 ? "bad" : share < 0.1 ? "warn" : "ok";
}

const barTone: Record<Tone, string> = {
  ok: "bg-accent",
  warn: "bg-warn",
  bad: "bg-bad",
};

function Meter({
  label,
  used,
  total,
  tone,
  detail,
}: {
  label: string;
  used: number;
  total: number;
  tone: Tone;
  detail: string;
}) {
  const pct = total ? Math.min(100, Math.round((used / total) * 100)) : 0;
  return (
    <div className="min-w-0">
      <div className="flex items-baseline justify-between gap-3 text-[0.8125rem]">
        <span className="truncate font-medium">{label}</span>
        <span className="flex-none text-muted">{detail}</span>
      </div>
      <div
        className="mt-1 h-2 overflow-hidden rounded-full bg-raised"
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct}
        aria-valuetext={`${pct}% used, ${detail}`}
      >
        <div
          className={`h-full rounded-full ${barTone[tone]}`}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  );
}

function ago(at: string | undefined, now: number): string {
  if (!at) return "never";
  const s = Math.max(0, Math.round((now - new Date(at).getTime()) / 1000));
  if (s < 90) return `${s} s ago`;
  const m = Math.round(s / 60);
  if (m < 90) return `${m} min ago`;
  const h = Math.round(m / 60);
  return h < 48 ? `${h} h ago` : `${Math.round(h / 24)} days ago`;
}

function duration(from: string, now: number): string {
  return ago(from, now).replace(" ago", "");
}

function HostCard({
  kind,
  name,
  detail,
  status,
  capacity,
  now,
}: {
  kind: string;
  name: string;
  detail: string;
  status: { tone: "ok" | "warn" | "bad" | "neutral"; text: string };
  capacity?: Host;
  now: number;
}) {
  const stale =
    capacity && now - new Date(capacity.measuredAt).getTime() > staleAfter;
  return (
    <article
      className="samuori-card flex flex-col gap-3"
      aria-label={`${kind} ${name}`}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="m-0 text-xs font-semibold tracking-wider text-muted uppercase">
            {kind}
          </p>
          <h3 className="m-0 truncate text-base">{name}</h3>
          <p className="m-0 truncate text-[0.8125rem] text-muted">{detail}</p>
        </div>
        <Badge tone={status.tone}>{status.text}</Badge>
      </div>
      {!capacity ? (
        <p className="m-0 text-sm text-muted">
          No capacity report yet. Hosts on a release before this view report
          after they are updated.
        </p>
      ) : (
        <>
          {capacity.volumes.map((v) => (
            <Meter
              key={v.path}
              label={`${v.label} (${v.path})`}
              used={v.totalBytes - v.freeBytes}
              total={v.totalBytes}
              tone={diskTone(v.freeBytes, v.totalBytes)}
              detail={`${bytes(v.freeBytes)} free of ${bytes(v.totalBytes)}`}
            />
          ))}
          {capacity.memoryTotalBytes > 0 && (
            <Meter
              label="Memory"
              used={capacity.memoryTotalBytes - capacity.memoryAvailableBytes}
              total={capacity.memoryTotalBytes}
              tone={memoryTone(capacity)}
              detail={`${bytes(capacity.memoryAvailableBytes)} available of ${bytes(capacity.memoryTotalBytes)}`}
            />
          )}
          <p className="m-0 mt-auto text-xs text-muted">
            {capacity.cpus} CPUs · measured {ago(capacity.measuredAt, now)}
            {stale && (
              <>
                {" "}
                <Badge tone="warn">Stale</Badge>
              </>
            )}
          </p>
        </>
      )}
    </article>
  );
}

function lowest(o: Overview): number | undefined {
  let min: number | undefined;
  for (const h of [...o.controllers, ...o.workers])
    for (const v of h.capacity?.volumes ?? [])
      if (v.totalBytes) {
        const share = v.freeBytes / v.totalBytes;
        if (min === undefined || share < min) min = share;
      }
  return min;
}

export function CapacityPage() {
  const q = useQuery({
    queryKey: ["capacity"],
    queryFn: async () => {
      const { data, error } = await api.GET("/system/capacity");
      if (!data) throw new Error(describeError(error));
      return data;
    },
    refetchInterval: 15_000,
  });
  const header = (
    <PageHeader
      title="Capacity"
      description="Disk, memory and CPUs every controller and worker last reported, with builds still running and uploads not yet finished. Refreshes every 15 seconds."
    />
  );
  if (q.error)
    return (
      <>
        {header}
        <Alert>{q.error.message}</Alert>
      </>
    );
  if (!q.data)
    return (
      <>
        {header}
        <Loading />
      </>
    );
  const o = q.data;
  const now = new Date(o.measuredAt).getTime();
  const min = lowest(o);
  const overdue = o.builds.filter((b) => b.leaseOverdue).length;
  const memoryLow = [...o.controllers, ...o.workers].filter(
    (h) => h.capacity && memoryTone(h.capacity) !== "ok",
  ).length;
  return (
    <>
      {header}
      <section aria-label="Summary" className="samuori-stat-grid">
        <StatCard
          label="Lowest free disk"
          value={min === undefined ? "—" : `${Math.round(min * 100)}%`}
          icon={HardDrive}
          accent={
            min === undefined
              ? "slate"
              : min < 0.05
                ? "red"
                : min < 0.15
                  ? "amber"
                  : "green"
          }
          hint="Across every reported filesystem"
        />
        <StatCard
          label="Memory pressure"
          value={memoryLow}
          icon={MemoryStick}
          accent={memoryLow > 0 ? "amber" : "slate"}
          hint="Hosts with under 10% memory available"
        />
        <StatCard
          label="Builds in progress"
          value={o.builds.length}
          icon={Hammer}
          accent={overdue > 0 ? "red" : o.builds.length ? "blue" : "slate"}
          hint={
            overdue > 0
              ? `${overdue} with an overdue lease`
              : "Queued, assigned or running"
          }
        />
        <StatCard
          label="Open uploads"
          value={o.uploads.length}
          icon={Upload}
          accent={o.uploads.length ? "blue" : "slate"}
          hint="Each person may have 5 at once"
        />
      </section>

      <Section
        title="Controller capacity"
        description="Controller data holds the database when it is SQLite, logs and, with local storage, uploads and images."
      >
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 2xl:grid-cols-3">
          {o.controllers.map((c) => (
            <HostCard
              key={c.id}
              kind={c.leader ? "Controller · leader" : "Controller"}
              name={c.hostname}
              detail={c.version}
              status={
                c.live
                  ? { tone: "ok", text: "Running" }
                  : { tone: "neutral", text: "Stopped" }
              }
              capacity={c.capacity}
              now={now}
            />
          ))}
        </div>
      </Section>

      <Section
        title="Worker capacity"
        description="Builds and worker state holds each build's workspace while it runs; it is removed when the attempt ends."
      >
        {o.workers.length === 0 ? (
          <p className="samuori-muted">No worker is enrolled.</p>
        ) : (
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 2xl:grid-cols-3">
            {o.workers.map((w) => (
              <HostCard
                key={w.id}
                kind="Worker"
                name={w.name}
                detail={`${w.pool || "Unassigned"} · seen ${ago(w.lastSeenAt, now)}`}
                status={
                  w.state === "active"
                    ? { tone: "ok", text: "Active" }
                    : { tone: "warn", text: w.state }
                }
                capacity={w.capacity}
                now={now}
              />
            ))}
          </div>
        )}
      </Section>

      <Section
        title="Builds in progress"
        description="A worker renews its lease every 15 seconds. When a lease runs out the build is retried on another attempt, or fails once its attempts are used; it never waits forever."
      >
        <Table
          label="Builds in progress"
          rows={o.builds}
          empty="No build is queued or running."
          columns={[
            {
              id: "build",
              name: "Build",
              isRowHeader: true,
              render: (b) => (
                <Link href={`/builds/${b.id}`} className="samuori-link">
                  {b.project || b.projectId}
                </Link>
              ),
            },
            {
              id: "state",
              name: "State",
              render: (b) => (
                <span className="inline-flex flex-wrap items-center gap-1.5">
                  <Badge tone="info" live>
                    {b.state}
                  </Badge>
                  {b.leaseOverdue && <Badge tone="bad">Lease overdue</Badge>}
                </span>
              ),
            },
            {
              id: "in-state",
              name: "In this state",
              render: (b) => duration(b.updatedAt, now),
              sortValue: (b) => b.updatedAt,
            },
            {
              id: "age",
              name: "Started",
              render: (b) => ago(b.createdAt, now),
              sortValue: (b) => b.createdAt,
            },
            {
              id: "worker",
              name: "Worker",
              render: (b) => b.worker || "—",
            },
            {
              id: "attempts",
              name: "Attempt",
              align: "end",
              render: (b) => `${b.attemptsUsed} of ${b.maxAttempts}`,
            },
          ]}
        />
      </Section>

      <Section
        title="Open uploads"
        description="Uploads not yet finished. Starting the same upload again instead of resuming creates a new one; the old one counts toward its owner's limit of 5 until it expires, and its partial data is then deleted."
      >
        <Table
          label="Open uploads"
          rows={o.uploads}
          empty="No upload is in progress."
          columns={[
            {
              id: "project",
              name: "Project",
              isRowHeader: true,
              render: (u) => (
                <Link
                  href={`/projects/${u.projectId}`}
                  className="samuori-link"
                >
                  {u.project || u.projectId}
                </Link>
              ),
            },
            { id: "by", name: "By", render: (u) => u.createdBy },
            {
              id: "received",
              name: "Received",
              render: (u) =>
                `${bytes(u.receivedBytes)} of ${bytes(u.totalBytes)}`,
            },
            {
              id: "status",
              name: "Status",
              render: (u) =>
                u.status === "committing" ? "Being processed" : "Receiving",
            },
            {
              id: "started",
              name: "Started",
              render: (u) => ago(u.createdAt, now),
              sortValue: (u) => u.createdAt,
            },
            {
              id: "expires",
              name: "Expires",
              render: (u) => new Date(u.expiresAt).toLocaleString(),
              sortValue: (u) => u.expiresAt,
            },
          ]}
        />
      </Section>
    </>
  );
}
