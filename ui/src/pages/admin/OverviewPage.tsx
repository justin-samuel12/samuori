import { useQuery } from "@tanstack/react-query";
import { Link } from "react-aria-components";
import { HeartPulse, Server, UserRound, UsersRound } from "lucide-react";
import { api } from "../../api/client";
import {
  Alert,
  Badge,
  Loading,
  PageHeader,
  Section,
  StatCard,
} from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useHealth } from "../../shell/health";

const statusText: Record<string, string> = {
  ok: "Healthy",
  degraded: "Degraded",
  unavailable: "Unavailable",
  "not-configured": "Not configured",
};

const statusTone = {
  ok: "ok",
  degraded: "warn",
  unavailable: "bad",
  "not-configured": "neutral",
} as const;

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
  const workers = useQuery({
    queryKey: ["admin", "workers"],
    queryFn: async () =>
      (await api.GET("/workers", { params: { query: { limit: 200 } } })).data
        ?.items ?? null,
  });
  const ready = workers.data?.filter((w) => w.eligibility.untrustedBuilds);
  const h = health.data;

  return (
    <>
      <PageHeader
        title="Administration"
        description="Organization health, people and build capacity."
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
          label="Workers ready"
          value={ready ? `${ready.length} / ${workers.data?.length ?? 0}` : "—"}
          icon={Server}
          accent={ready && ready.length > 0 ? "green" : "amber"}
          hint="Eligible to build uploaded source"
          href="/admin/workers"
          linkLabel="Nodes & workers"
        />
        <StatCard
          label="System"
          value={h ? statusText[h.status] : "—"}
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
                  render: (c) => (
                    <Badge tone={statusTone[c.status]}>
                      {statusText[c.status]}
                    </Badge>
                  ),
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
              Only components with real connectivity checks are listed. Cluster
              details are under{" "}
              <Link href="/admin/upgrades" className="samuori-link">
                Upgrades & Diagnostics
              </Link>
              .
            </p>
          </>
        )}
      </Section>
    </>
  );
}
