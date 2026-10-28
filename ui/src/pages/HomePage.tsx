import { useQuery } from "@tanstack/react-query";
import { Link } from "react-aria-components";
import {
  CircleCheck,
  CircleX,
  FolderKanban,
  Hammer,
  ShieldCheck,
  SquarePlus,
} from "lucide-react";
import { api } from "../api/client";
import {
  Alert,
  Badge,
  Loading,
  PageHeader,
  Section,
  StatCard,
} from "../components/Feedback";
import { useCan, useMe } from "../session/session";
import { RunsTable } from "./BuildsPage";
import { WorkflowRail } from "../components/WorkflowRail";
import { terminal } from "./builds";

const page = 100;

function count(n: number | undefined): string {
  if (n === undefined) return "—";
  return n >= page ? `${page}+` : String(n);
}

export function HomePage() {
  const me = useMe();
  const can = useCan();
  const seesBuilds = can("listBuilds");
  const seesProjects = can("listProjects");
  const projects = useQuery({
    queryKey: ["projects", "home"],
    queryFn: async () =>
      (await api.GET("/projects", { params: { query: { limit: page } } })).data
        ?.items ?? [],
    enabled: seesProjects,
  });
  const builds = useQuery({
    queryKey: ["builds", "home"],
    queryFn: async () =>
      (await api.GET("/builds", { params: { query: { limit: page } } })).data
        ?.items ?? [],
    refetchInterval: 15_000,
    enabled: seesBuilds,
  });
  const approvals = useQuery({
    queryKey: ["approvals", "pending"],
    queryFn: async () => {
      const { data } = await api.GET("/approvals", {
        params: { query: { state: "pending", limit: page } },
      });
      return data ? data.items.length : null;
    },
  });

  const runs = builds.data ?? [];
  const active = runs.filter((r) => !terminal.has(r.state)).length;
  const built = runs.filter((r) => r.state === "succeeded").length;
  const failed = runs.filter((r) =>
    ["failed", "blocked", "timed_out"].includes(r.state),
  ).length;
  const names = Object.fromEntries(
    (projects.data ?? []).map((p) => [p.id, p.name]),
  );
  const scope =
    runs.length >= page ? `of the last ${page} builds` : "of all builds";

  return (
    <>
      <PageHeader
        help="getting-started"
        title={`Welcome, ${me.displayName ?? "there"}`}
        description="Turn your application into a validated container image from the browser."
        actions={
          can("createSpec") && (
            <Link
              href="/new-image"
              className="samuori-button samuori-button--primary no-underline"
            >
              <SquarePlus size={16} aria-hidden="true" />
              New image
            </Link>
          )
        }
      />
      <Section
        title="From source to image"
        description="Each stage links to where it is done. Builds, validation and publication are separate steps; a built image is not published until you publish it."
        className="mt-0 mb-8"
      >
        <WorkflowRail
          label="From source to image"
          info={{
            upload: { href: can("createSpec") ? "/new-image" : undefined },
            discover: { href: seesProjects ? "/projects" : undefined },
            configure: { href: can("createSpec") ? "/new-image" : undefined },
            build: {
              href: seesBuilds ? "/builds" : undefined,
              meta:
                builds.data && active > 0 ? `${active} running now` : undefined,
              active: active > 0,
            },
            validate: { href: seesBuilds ? "/builds" : undefined },
            publish: {
              href: can("listPublications") ? "/images" : undefined,
            },
            kubernetes: {
              href:
                seesProjects && can("listKubeConversions")
                  ? "/kubernetes"
                  : undefined,
            },
          }}
        />
      </Section>
      <section aria-label="Summary" className="samuori-stat-grid">
        {seesProjects && (
          <StatCard
            label="Projects"
            value={count(projects.data?.length)}
            icon={FolderKanban}
            href="/projects"
            linkLabel="View projects"
          />
        )}
        {seesBuilds && (
          <>
            <StatCard
              label="Running now"
              value={builds.data ? active : "—"}
              icon={Hammer}
              accent={active > 0 ? "blue" : "slate"}
              hint={
                active > 0 ? "Updates every 15 seconds" : "No builds running"
              }
              href="/builds"
              linkLabel="Build runs"
            />
            <StatCard
              label="Built"
              value={builds.data ? built : "—"}
              icon={CircleCheck}
              accent="green"
              hint={
                builds.data ? `${failed} failed or blocked ${scope}` : undefined
              }
            />
          </>
        )}
        {approvals.data === null || approvals.data === undefined ? (
          seesBuilds && (
            <StatCard
              label="Failed or blocked"
              value={builds.data ? failed : "—"}
              icon={CircleX}
              accent={failed > 0 ? "red" : "slate"}
              hint={scope}
            />
          )
        ) : (
          <StatCard
            label="Awaiting approval"
            value={count(approvals.data)}
            icon={ShieldCheck}
            accent={approvals.data > 0 ? "amber" : "slate"}
            href="/approvals"
            linkLabel="Approvals"
          />
        )}
      </section>

      <div className="mt-8 grid grid-cols-1 gap-6 xl:grid-cols-[minmax(0,1fr)_320px]">
        {seesBuilds ? (
          <Section
            title="Recent builds"
            className="mt-0"
            actions={
              <Link href="/builds" className="samuori-link text-sm font-medium">
                View all →
              </Link>
            }
          >
            {builds.isPending ? (
              <Loading />
            ) : (
              <RunsTable
                runs={runs.slice(0, 8)}
                projects={names}
                label="Recent builds"
              />
            )}
          </Section>
        ) : (
          <div />
        )}

        <aside className="space-y-4" aria-label="Your access">
          <div className="samuori-card">
            <h2 className="mb-3">Your access</h2>
            {me.roles.length === 0 ? (
              <p className="samuori-muted m-0">
                You have no roles yet. Ask an administrator to add you to a
                team.
              </p>
            ) : (
              <ul className="m-0 flex list-none flex-wrap gap-2 p-0">
                {me.roles.map((r, i) => (
                  <li key={i}>
                    <Badge tone="info">
                      {r.role.replace("-", " ")} · {r.scopeType}
                    </Badge>
                  </li>
                ))}
              </ul>
            )}
            <Link
              href="/account"
              className="samuori-link mt-4 inline-block text-sm font-medium"
            >
              View account →
            </Link>
          </div>
          <Alert tone="info">
            Builds run only on workers in a pool whose isolation profile is
            approved and verified. Uploaded code is analysed without being run.
          </Alert>
        </aside>
      </div>
    </>
  );
}
