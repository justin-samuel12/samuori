import { useCallback, useState } from "react";
import { useLoader } from "../hooks";
import { api, type Schemas } from "../api/client";

export type Team = Schemas["Team"];

export function useTeams() {
  const [teams, setTeams] = useState<Team[]>();
  const [error, setError] = useState(false);
  const reload = useCallback(async () => {
    const { data } = await api.GET("/teams", {
      params: { query: { limit: 200 } },
    });
    if (data) setTeams(data.items);
    else setError(true);
  }, []);
  useLoader(reload);
  return { teams, error, reload };
}

export function teamName(teams: Team[] | undefined, id: string): string {
  if (!teams) return "…";
  return teams.find((t) => t.id === id)?.name ?? "Unknown team";
}
