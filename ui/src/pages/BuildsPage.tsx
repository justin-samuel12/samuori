import { useCallback, useState } from "react";
import { Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import {
  Alert,
  Badge,
  EmptyState,
  Loading,
  PageHeader,
} from "../components/Feedback";
import { Table } from "../components/Table";
import { shortDigest } from "../format";
import { useLoader } from "../hooks";
import { badgeTone, stateText, terminal } from "./builds";

type Run = Schemas["BuildRun"];

export function RunsTable({
  runs,
  projects,
  label,
}: {
  runs: Run[];
  projects: Record<string, string>;
  label: string;
}) {
  return (
    <Table
      label={label}
      rows={runs}
      empty={
        <EmptyState title="No builds yet">
          Start one from a project with “New image”.
        </EmptyState>
      }
      columns={[
        {
          id: "when",
          name: "Started",
          isRowHeader: true,
          sortValue: (r) => r.createdAt,
          render: (r) => (
            <Link href={`/builds/${r.id}`} className="samuori-link font-medium">
              {new Date(r.createdAt).toLocaleString()}
            </Link>
          ),
        },
        {
          id: "project",
          name: "Project",
          sortValue: (r) => projects[r.projectId] ?? r.projectId,
          render: (r) => projects[r.projectId] ?? r.projectId,
        },
        {
          id: "state",
          name: "Status",
          sortValue: (r) => stateText[r.state] ?? r.state,
          render: (r) => (
            <Badge tone={badgeTone(r.state)} live={!terminal.has(r.state)}>
              {stateText[r.state] ?? r.state}
            </Badge>
          ),
        },
        {
          id: "spec",
          name: "Spec",
          sortValue: (r) => r.specRevision,
          render: (r) => `revision ${r.specRevision}`,
        },
        {
          id: "digest",
          name: "Image",
          render: (r) =>
            r.artifactDigest ? (
              <code>{shortDigest(r.artifactDigest)}</code>
            ) : (
              "—"
            ),
        },
      ]}
    />
  );
}

export function BuildsPage() {
  const [runs, setRuns] = useState<Run[]>();
  const [projects, setProjects] = useState<Record<string, string>>({});
  const [error, setError] = useState<string>();
  const load = useCallback(async () => {
    const [b, p] = await Promise.all([
      api.GET("/builds", { params: { query: { limit: 100 } } }),
      api.GET("/projects", { params: { query: { limit: 100 } } }),
    ]);
    if (!b.data) {
      setError(describeError(b.error, "Builds could not be loaded."));
      return;
    }
    setRuns(b.data.items);
    setProjects(
      Object.fromEntries((p.data?.items ?? []).map((x) => [x.id, x.name])),
    );
  }, []);
  useLoader(load);
  return (
    <>
      <PageHeader
        help="builds"
        title="Build runs"
        description="Every build you can see, newest first."
      />
      {error && <Alert>{error}</Alert>}
      {!runs && !error ? (
        <Loading />
      ) : (
        runs && <RunsTable runs={runs} projects={projects} label="Build runs" />
      )}
    </>
  );
}
