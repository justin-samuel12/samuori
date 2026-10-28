import { useQuery } from "@tanstack/react-query";
import { api, type Schemas } from "../api/client";

/** The controller's health report; an unavailable controller answers 503 with the same body. */
export function useHealth() {
  return useQuery({
    queryKey: ["system", "health"],
    queryFn: async (): Promise<Schemas["Health"] | null> => {
      const { data, error } = await api.GET("/system/health");
      const report = data ?? (error as Partial<Schemas["Health"]> | undefined);
      return report?.status ? (report as Schemas["Health"]) : null;
    },
    refetchInterval: 30_000,
  });
}
