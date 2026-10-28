import { useEffect } from "react";

/** Runs an async loader on mount and whenever it changes; state updates happen after the first await. */
export function useLoader(load: () => Promise<unknown>) {
  useEffect(() => {
    void load();
  }, [load]);
}
