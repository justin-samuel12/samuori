import { useQuery } from "@tanstack/react-query";
import { api, type Schemas } from "../api/client";
import { useMe } from "../session/session";
import { offeredHelp } from "../shell/nav";

export type HelpSummary = Schemas["HelpTopicSummary"];

/** The guides to offer this user: the server's role-filtered list, narrowed to the pages the menu shows them. */
export function useHelpTopics() {
  const me = useMe();
  return useQuery({
    queryKey: ["help", "topics"],
    queryFn: async (): Promise<HelpSummary[]> =>
      (await api.GET("/help/topics")).data?.items ?? [],
    staleTime: 5 * 60_000,
    select: (topics) => offeredHelp(me, topics),
  });
}
