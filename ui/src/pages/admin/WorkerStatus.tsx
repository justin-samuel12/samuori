import { useQuery } from "@tanstack/react-query";
import { Link } from "react-aria-components";
import { api, type Schemas } from "../../api/client";
import {
  figureLabels,
  figureMeaning,
  workerFigures,
  type WorkerFigures,
} from "./workerFigures";

export function useWorkerFigures(enabled = true) {
  return useQuery({
    enabled,
    queryKey: ["admin", "workers"],
    queryFn: async () => {
      const items = (
        await api.GET("/workers", { params: { query: { limit: 200 } } })
      ).data?.items;
      return items ? workerFigures(items) : null;
    },
    refetchInterval: 15_000,
  });
}

/** The shared worker figures as one line; each term explains itself on hover. A page that already lists the
 * workers passes them, so the line matches its table. */
export function WorkerStatus({
  workers,
  link = true,
}: {
  workers?: readonly Schemas["Worker"][];
  link?: boolean;
}) {
  const query = useWorkerFigures(!workers);
  const data = workers ? workerFigures(workers) : query.data;
  if (!workers && query.isPending) return null;
  if (!data)
    return <p className="samuori-muted">Worker status could not be read.</p>;
  const keys = Object.keys(figureLabels) as (keyof WorkerFigures)[];
  return (
    <p aria-label="Worker status">
      Workers:{" "}
      {keys.map((k, i) => (
        <span key={k}>
          {i > 0 && " · "}
          <strong>{data[k]}</strong>{" "}
          <abbr title={figureMeaning[k]} className="no-underline">
            {figureLabels[k]}
          </abbr>
        </span>
      ))}
      {link && (
        <>
          {" "}
          <Link href="/admin/workers" className="samuori-link">
            Workers &amp; pools
          </Link>
        </>
      )}
    </p>
  );
}
