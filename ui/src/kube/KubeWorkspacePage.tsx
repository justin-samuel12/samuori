import { useQueries, useQuery } from "@tanstack/react-query";
import { Link } from "react-aria-components";
import { api, type Schemas } from "../api/client";
import {
  StateBadge,
  EmptyState,
  Loading,
  PageHeader,
  Section,
} from "../components/Feedback";
import { Table } from "../components/Table";
import { useCan } from "../session/session";
import { GenerateButton } from "./KubeConversionPage";
import { conversionState } from "../pages/display";

type Conversion = Schemas["KubeConversion"];

const projectLimit = 50;
const perProject = 10;

/** Every project's Kubernetes configuration in one place: the newest conversions of up to 50 projects. */
export function KubeWorkspacePage() {
  const can = useCan();
  const projects = useQuery({
    queryKey: ["projects", "kubernetes"],
    queryFn: async () =>
      (
        await api.GET("/projects", {
          params: { query: { limit: projectLimit } },
        })
      ).data?.items ?? [],
  });
  const list = projects.data ?? [];
  const conversions = useQueries({
    queries: list.map((p) => ({
      queryKey: ["kube-conversions", p.id, perProject],
      queryFn: async () => {
        const res = await api.GET("/projects/{projectId}/kube-conversions", {
          params: { path: { projectId: p.id }, query: { limit: perProject } },
        });
        return res.data?.items ?? [];
      },
    })),
  });
  const loading = projects.isPending || conversions.some((c) => c.isPending);
  const names = Object.fromEntries(list.map((p) => [p.id, p.name]));
  const all: Conversion[] = conversions
    .flatMap((c) => c.data ?? [])
    .sort((a, b) => b.createdAt.localeCompare(a.createdAt));
  const counts = new Map<string, number>();
  for (const c of all)
    counts.set(c.projectId, (counts.get(c.projectId) ?? 0) + 1);

  return (
    <>
      <PageHeader
        title="Kubernetes configurations"
        help="kubernetes"
        description="Kubernetes files generated from your projects, to review and apply yourself. Samuori never deploys them."
      />
      {loading ? (
        <Loading />
      ) : list.length === 0 ? (
        <EmptyState title="No projects yet">
          <p>Kubernetes configuration is generated per project.</p>
        </EmptyState>
      ) : (
        <div className="grid grid-cols-1 gap-8 2xl:grid-cols-[minmax(0,1fr)_26rem]">
          <Section
            title="Recent conversions"
            description={`The newest ${perProject} of each project.`}
            className="mt-0"
          >
            <Table
              label="Kubernetes conversions"
              rows={all}
              empty="No conversions yet. Generate one from a project."
              columns={[
                {
                  id: "name",
                  name: "Name",
                  isRowHeader: true,
                  render: (c) => (
                    <Link
                      href={`/kube-conversions/${c.id}`}
                      className="samuori-link"
                    >
                      {c.name}
                    </Link>
                  ),
                  sortValue: (c) => c.name,
                },
                {
                  id: "project",
                  name: "Project",
                  render: (c) => names[c.projectId] ?? c.projectId,
                  sortValue: (c) => names[c.projectId] ?? "",
                },
                {
                  id: "state",
                  name: "State",
                  render: (c) => (
                    <StateBadge state={conversionState(c.state)} />
                  ),
                },
                {
                  id: "format",
                  name: "Format",
                  render: (c) => c.options.format,
                },
                { id: "provider", name: "Provider", render: (c) => c.provider },
                {
                  id: "rev",
                  name: "Revision",
                  align: "end",
                  render: (c) => c.currentRevision || "none",
                },
                {
                  id: "when",
                  name: "Created",
                  render: (c) => new Date(c.createdAt).toLocaleString(),
                  sortValue: (c) => c.createdAt,
                },
              ]}
            />
          </Section>
          <Section title="Projects" className="mt-0">
            <Table
              label="Projects"
              rows={list}
              columns={[
                {
                  id: "name",
                  name: "Project",
                  isRowHeader: true,
                  render: (p) => (
                    <Link
                      href={`/projects/${p.id}/kubernetes`}
                      className="samuori-link"
                    >
                      {p.name}
                    </Link>
                  ),
                  sortValue: (p) => p.name,
                },
                {
                  id: "count",
                  name: "Conversions",
                  align: "end",
                  render: (p) => {
                    const n = counts.get(p.id) ?? 0;
                    return n >= perProject ? `${perProject}+` : n;
                  },
                },
                {
                  id: "actions",
                  name: "Actions",
                  render: (p) =>
                    can("createKubeConversion", {
                      teamId: p.teamId,
                      projectId: p.id,
                    }) ? (
                      <GenerateButton
                        projectId={p.id}
                        variant="secondary"
                        label="Generate"
                      />
                    ) : null,
                },
              ]}
            />
            {list.length >= projectLimit && (
              <p className="samuori-muted mt-3 text-sm">
                Showing the first {projectLimit} projects. Open a project for
                all of its conversions.
              </p>
            )}
          </Section>
        </div>
      )}
    </>
  );
}
