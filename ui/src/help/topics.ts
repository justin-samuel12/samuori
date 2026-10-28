import { useQuery } from "@tanstack/react-query";
import { api, type Schemas } from "../api/client";

export type HelpSummary = Schemas["HelpTopicSummary"];

/** The guides the server lets this user read; the list is already filtered by role. */
export function useHelpTopics() {
  return useQuery({
    queryKey: ["help", "topics"],
    queryFn: async (): Promise<HelpSummary[]> =>
      (await api.GET("/help/topics")).data?.items ?? [],
    staleTime: 5 * 60_000,
  });
}
