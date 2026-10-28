import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useLoader } from "../hooks";
import { api, onUnauthenticated, type Schemas } from "../api/client";
import { navigate } from "../router";
import { broadcast, onBroadcast, takeReturn } from "./broadcast";
import { expiryNotice, forgetDeadlines, SessionExpiry } from "./expiry";
import { can, canDownload, type Scope } from "./can";
import type { OperationId } from "../api/generated/roles";

export type Me = Schemas["Me"];
export type Role = Schemas["Role"];

type State =
  | { kind: "loading"; reconnecting?: boolean }
  | { kind: "unreachable" }
  | { kind: "setup" }
  | { kind: "anonymous"; notice?: string }
  | { kind: "signed-in"; me: Me };

interface Session {
  state: State;
  refresh: (notice?: string) => Promise<void>;
  signOut: () => Promise<void>;
}

const Ctx = createContext<Session | null>(null);

// A controller restarting, failing over or leaving the load balancer answers with a network error or 502–504 for a
// few seconds; the first calls are retried for that long before the portal says it cannot reach the server.
const reconnectFor = 15_000;

async function steady<T extends { response?: Response }>(
  call: () => Promise<T>,
  waiting: () => void,
): Promise<T | null> {
  const deadline = Date.now() + reconnectFor;
  for (;;) {
    const r = await call().catch(() => null);
    const status = r?.response?.status ?? 0;
    if (r && status !== 0 && (status < 502 || status > 504)) return r;
    if (Date.now() > deadline) return r;
    waiting();
    await new Promise((done) => setTimeout(done, 1500));
  }
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<State>({ kind: "loading" });
  const kind = useRef(state.kind);
  useEffect(() => {
    kind.current = state.kind;
  }, [state.kind]);

  const refresh = useCallback(async (notice?: string) => {
    const waiting = () =>
      setState((s) =>
        s.kind === "loading" || s.kind === "unreachable"
          ? { kind: "loading", reconnecting: true }
          : s,
      );
    const setup = await steady(() => api.GET("/setup/status"), waiting);
    if (!setup?.data) {
      setState({ kind: "unreachable" });
      return;
    }
    if (!setup.data.completed) {
      setState({ kind: "setup" });
      return;
    }
    const me = await steady(() => api.GET("/me"), waiting);
    if (!me) {
      setState({ kind: "unreachable" });
      return;
    }
    if (!me.data) {
      setState({ kind: "anonymous", notice });
      return;
    }
    setState({ kind: "signed-in", me: me.data });
    const back = takeReturn();
    if (back) {
      broadcast({ type: "signed-in" });
      navigate(back);
    }
  }, []);

  const end = useCallback((notice: string) => {
    forgetDeadlines();
    setState((s) =>
      s.kind === "signed-in" ? { kind: "anonymous", notice } : s,
    );
  }, []);

  const signOut = useCallback(async () => {
    await api.POST("/auth/logout");
    const notice = "You have signed out.";
    broadcast({ type: "signed-out", notice });
    end(notice);
  }, [end]);

  useLoader(refresh);
  useEffect(
    () =>
      onUnauthenticated(() => {
        if (kind.current !== "signed-in") return;
        const notice = expiryNotice();
        broadcast({ type: "signed-out", notice });
        end(notice);
      }),
    [end],
  );
  useEffect(
    () =>
      onBroadcast((m) => {
        if (m.type === "signed-out") end(m.notice);
        if (m.type === "signed-in" && kind.current === "anonymous")
          void refresh();
      }),
    [end, refresh],
  );

  return (
    <Ctx.Provider value={{ state, refresh, signOut }}>
      {children}
      {state.kind === "signed-in" && (
        <SessionExpiry onExpired={end} onSignOut={() => void signOut()} />
      )}
    </Ctx.Provider>
  );
}

export function useSession(): Session {
  const s = useContext(Ctx);
  if (!s) throw new Error("useSession outside SessionProvider");
  return s;
}

export function useMe(): Me {
  const { state } = useSession();
  if (state.kind !== "signed-in")
    throw new Error("useMe requires a signed-in session");
  return state.me;
}

/**
 * Whether the signed-in user's roles allow an operation, from the API document's x-samuori-roles; pass the project's
 * team and ID when known. It hides what the server would refuse; the server still authorizes every call.
 */
export function useCan(): (op: OperationId, scope?: Scope) => boolean {
  const me = useMe();
  return (op, scope) => can(me, op, scope);
}

/** A read the user's roles do not permit; the section that shows it renders nothing. */
export function isForbidden(res: { response?: Response }): boolean {
  return res.response?.status === 403;
}

export function isOrgAdmin(me: Me): boolean {
  return me.roles.some(
    (r) => r.role === "administrator" && r.scopeType === "organization",
  );
}

export function orgId(me: Me): string | undefined {
  return me.roles.find((r) => r.scopeType === "organization")?.scopeId;
}

export function useCanDownload(): boolean {
  return canDownload(useMe());
}
