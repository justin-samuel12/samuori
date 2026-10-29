import { useCallback, useState } from "react";
import { Link } from "react-aria-components";
import { api, type Schemas } from "../../api/client";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { useLoader } from "../../hooks";
import { RunsTable } from "../BuildsPage";
import { WorkerStatus } from "./WorkerStatus";

const active = [
  "queued",
  "assigned",
  "preparing",
  "building",
  "committing",
] as const;

export function QueuePage() {
  const [runs, setRuns] = useState<Schemas["BuildRun"][]>();
  const [projects, setProjects] = useState<Record<string, string>>({});
  const load = useCallback(async () => {
    const lists = await Promise.all(
      active.map((state) =>
        api.GET("/builds", { params: { query: { state, limit: 100 } } }),
      ),
    );
    const p = await api.GET("/projects", { params: { query: { limit: 100 } } });
    setRuns(
      lists
        .flatMap((l) => l.data?.items ?? [])
        .sort((a, b) => a.createdAt.localeCompare(b.createdAt)),
    );
    setProjects(
      Object.fromEntries((p.data?.items ?? []).map((x) => [x.id, x.name])),
    );
  }, []);
  useLoader(load);
  if (!runs) return <Loading />;
  const waiting = runs.filter((r) => r.state === "queued").length;
  return (
    <>
      <PageHeader
        help="workers"
        title="Build queue"
        description="Builds waiting for a worker or running now, oldest first. Teams take turns."
      />
      <p>
        {waiting} waiting, {runs.length - waiting} running. Finished builds are
        under{" "}
        <Link href="/builds" className="samuori-link">
          Build runs
        </Link>
        .
      </p>
      <WorkerStatus />
      {waiting > 0 && runs.length === waiting && (
        <Alert tone="warning">
          Builds are waiting but none is running. Check{" "}
          <Link href="/admin/workers" className="samuori-link">
            Workers &amp; pools
          </Link>{" "}
          for workers ready to build.
        </Alert>
      )}
      <RunsTable
        runs={runs}
        projects={projects}
        label="Queued and running builds"
      />
    </>
  );
}
