import { useQuery } from "@tanstack/react-query";
import { Link } from "react-aria-components";
import { HeartPulse, Server, UserRound, UsersRound } from "lucide-react";
import { api } from "../../api/client";
import {
  Alert,
  Loading,
  PageHeader,
  Section,
  StatCard,
  StateBadge,
} from "../../components/Feedback";
import { healthState } from "../display";
import { Table } from "../../components/Table";
import { useHealth } from "../../shell/health";
import { useWorkerFigures } from "./WorkerStatus";
import { figureMeaning } from "./workerFigures";

export function OverviewPage() {
  const health = useHealth();
  const people = useQuery({
    queryKey: ["admin", "people"],
    queryFn: async () => {
      const [u, t] = await Promise.all([
        api.GET("/users"),
        api.GET("/teams", { params: { query: { limit: 200 } } }),
      ]);
      return u.data && t.data
        ? { users: u.data.items.length, teams: t.data.items.length }
        : null;
    },
  });
  const workers = useWorkerFigures().data;
  const h = health.data;

  return (
    <>
      <PageHeader
        title="Health"
        description="Controller checks, people and build capacity."
      />
      <section aria-label="Summary" className="samuori-stat-grid">
        <StatCard
          label="Users"
          value={people.data?.users ?? "—"}
          icon={UserRound}
          href="/admin/users"
          linkLabel="Manage users"
        />
        <StatCard
          label="Teams"
          value={people.data?.teams ?? "—"}
          icon={UsersRound}
          href="/admin/teams"
          linkLabel="Manage teams"
        />
        <StatCard
          label="Workers ready to build"
          value={workers ? `${workers.ready} / ${workers.enrolled}` : "—"}
          icon={Server}
          accent={workers && workers.ready > 0 ? "green" : "amber"}
          hint={
            workers
              ? `${workers.connected} of ${workers.enrolled} enrolled connected now`
              : figureMeaning.ready
          }
          href="/admin/workers"
          linkLabel="Workers & pools"
        />
        <StatCard
          label="Controller"
          value={h ? healthState(h.status).text : "—"}
          icon={HeartPulse}
          accent={
            h?.status === "ok"
              ? "green"
              : h?.status === "degraded"
                ? "amber"
                : "red"
          }
          hint="Database, storage and messaging checks"
        />
      </section>

      <Section title="System health">
        {health.isPending && <Loading />}
        {health.data === null && (
          <Alert>The health endpoint could not be reached.</Alert>
        )}
        {h && (
          <>
            <Table
              label="Component health"
              rows={(h.components ?? []).map((c) => ({ ...c, id: c.name }))}
              columns={[
                {
                  id: "name",
                  name: "Component",
                  isRowHeader: true,
                  sortValue: (c) => c.name,
                  render: (c) => <span className="font-medium">{c.name}</span>,
                },
                {
                  id: "status",
                  name: "Status",
                  sortValue: (c) => c.status,
                  render: (c) => <StateBadge state={healthState(c.status)} />,
                },
                {
                  id: "checked",
                  name: "Checked",
                  render: (c) =>
                    c.checkedAt
                      ? new Date(c.checkedAt).toLocaleTimeString()
                      : "—",
                },
              ]}
            />
            <p className="samuori-muted mt-3 text-sm">
              Only components with real connectivity checks are listed.
              Controllers are under{" "}
              <Link href="/admin/servers" className="samuori-link">
                Servers
              </Link>
              , releases under{" "}
              <Link href="/admin/upgrades" className="samuori-link">
                Upgrades & diagnostics
              </Link>
              .
            </p>
          </>
        )}
      </Section>
    </>
  );
}
