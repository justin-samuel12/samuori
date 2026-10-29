import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "./api/client";

/** Runs an async loader on mount and whenever it changes; state updates happen after the first await. */
export function useLoader(load: () => Promise<unknown>) {
  useEffect(() => {
    void load();
  }, [load]);
}

/** A project the user may read, shared between pages for names in titles and breadcrumbs; null when unreadable. */
export function useProject(id?: string) {
  return useQuery({
    queryKey: ["project", id],
    queryFn: async () =>
      (
        await api.GET("/projects/{projectId}", {
          params: { path: { projectId: id ?? "" } },
        })
      ).data ?? null,
    enabled: !!id,
  }).data;
}

/** A build specification's summary; its name is the app name an image is known by. */
export function useSpec(id?: string) {
  return useQuery({
    queryKey: ["spec", id],
    queryFn: async () =>
      (
        await api.GET("/specs/{specId}", {
          params: { path: { specId: id ?? "" } },
        })
      ).data ?? null,
    enabled: !!id,
  }).data;
}
