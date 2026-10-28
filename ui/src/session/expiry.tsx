import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import { Form } from "react-aria-components";
import { api, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { TextField } from "../components/Field";
import { Alert } from "../components/Feedback";
import { Spinner } from "../components/Spinner";
import { useLoader } from "../hooks";
import { broadcast, onBroadcast, type Deadlines } from "./broadcast";
import { currentPage, renewalDue } from "./renewal";

// Session expiry follows docs/architecture/identity.md#sessions: reads never renew the idle deadline, real activity
// renews it at most once a minute once half of it has passed, and the maximum is never extended. Before the maximum
// the person can renew, which re-authenticates into a new session (ADR 0068).
const extendEveryMs = 60_000;
const syncEveryMs = 120_000;
const slackMs = 5_000;
const confirmationMs = 6_000;

let known: Deadlines | undefined;

function toDeadlines(s: Schemas["SessionStatus"]): Deadlines {
  const offset = Date.parse(s.serverTime) - Date.now();
  return {
    idleExpiresAt: Date.parse(s.idleExpiresAt) - offset,
    absoluteExpiresAt: Date.parse(s.absoluteExpiresAt) - offset,
    idleTimeoutMs: s.idleTimeoutSeconds * 1000,
    warningMs: s.warningSeconds * 1000,
    renewWith: s.renewWith,
    providerName: s.providerName,
  };
}

/** Why the session ended, judged from the last known deadlines. */
export function expiryNotice(now = Date.now()): string {
  const d = known;
  if (d && now >= d.absoluteExpiresAt - slackMs)
    return "Your session reached its maximum length. Sign in again.";
  if (d && now >= d.idleExpiresAt - slackMs) {
    const minutes = Math.round(d.idleTimeoutMs / 60_000);
    return `Signed out after ${minutes} minutes of inactivity.`;
  }
  return "Your session ended. Sign in again.";
}

export function forgetDeadlines() {
  known = undefined;
}

function clock(ms: number): string {
  const s = Math.max(0, Math.ceil(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

function timeOfDay(ms: number): string {
  return new Date(ms).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
  });
}

interface Props {
  onExpired: (notice: string) => void;
  onSignOut: () => void;
}

export function SessionExpiry({ onExpired, onSignOut }: Props) {
  const [deadlines, setDeadlines] = useState<Deadlines | undefined>(known);
  const [now, setNow] = useState(() => Date.now());
  const [dismissed, setDismissed] = useState<number>();
  const [renewing, setRenewing] = useState(false);
  const [renewedUntil, setRenewedUntil] = useState<number>();
  const lastExtend = useRef(0);
  const checking = useRef(false);
  const warnedFor = useRef(0);

  const apply = useCallback((d: Deadlines, share: boolean) => {
    known = d;
    setDeadlines(d);
    if (share) broadcast({ type: "deadlines", deadlines: d });
  }, []);

  const expire = useCallback(() => {
    const notice = expiryNotice();
    broadcast({ type: "signed-out", notice });
    onExpired(notice);
  }, [onExpired]);

  const sync = useCallback(async () => {
    const { data, response } = await api.GET("/auth/session");
    if (data) apply(toDeadlines(data), true);
    else if (response.status === 404) expire();
  }, [apply, expire]);

  const extend = useCallback(async () => {
    lastExtend.current = Date.now();
    const { data, response } = await api.POST("/auth/session/extend");
    if (data) apply(toDeadlines(data), true);
    else if (response.status === 404) expire();
  }, [apply, expire]);

  const renewed = useCallback(
    (s: Schemas["SessionStatus"]) => {
      const d = toDeadlines(s);
      apply(d, true);
      setRenewing(false);
      setRenewedUntil(d.absoluteExpiresAt);
    },
    [apply],
  );

  useEffect(() => {
    if (renewedUntil === undefined) return;
    const t = window.setTimeout(
      () => setRenewedUntil(undefined),
      confirmationMs,
    );
    return () => window.clearTimeout(t);
  }, [renewedUntil]);

  const activity = useCallback(() => {
    const d = known;
    const t = Date.now();
    if (!d || t - lastExtend.current < extendEveryMs) return;
    const left = d.idleExpiresAt - t;
    if (left > d.idleTimeoutMs / 2 || left <= d.warningMs) return;
    if (d.idleExpiresAt >= d.absoluteExpiresAt) return;
    void extend();
  }, [extend]);

  useEffect(() => {
    const opts = { capture: true, passive: true };
    window.addEventListener("pointerdown", activity, opts);
    window.addEventListener("keydown", activity, opts);
    window.addEventListener("popstate", activity);
    return () => {
      window.removeEventListener("pointerdown", activity, opts);
      window.removeEventListener("keydown", activity, opts);
      window.removeEventListener("popstate", activity);
    };
  }, [activity]);

  useLoader(sync);
  useEffect(() => {
    const poll = window.setInterval(() => void sync(), syncEveryMs);
    const tick = window.setInterval(() => setNow(Date.now()), 1000);
    return () => {
      window.clearInterval(poll);
      window.clearInterval(tick);
    };
  }, [sync]);

  useEffect(
    () =>
      onBroadcast((m) => {
        if (m.type !== "deadlines") return;
        apply(m.deadlines, false);
        if (!renewalDue(m.deadlines.absoluteExpiresAt, 0, Date.now()))
          setRenewing(false);
      }),
    [apply],
  );

  useEffect(() => {
    if (!deadlines || checking.current) return;
    const left = deadlines.idleExpiresAt - now;
    const confirm = async () => {
      checking.current = true;
      await sync();
      checking.current = false;
    };
    if (left <= 0) {
      void confirm();
    } else if (
      left <= deadlines.warningMs &&
      warnedFor.current !== deadlines.idleExpiresAt
    ) {
      warnedFor.current = deadlines.idleExpiresAt;
      void confirm();
    }
  }, [deadlines, now, sync]);

  if (!deadlines) return null;
  const idleLeft = deadlines.idleExpiresAt - now;
  const capped = deadlines.idleExpiresAt >= deadlines.absoluteExpiresAt;
  const warning =
    !renewing && !capped && idleLeft > 0 && idleLeft <= deadlines.warningMs;
  const due = renewalDue(deadlines.absoluteExpiresAt, deadlines.warningMs, now);
  return (
    <>
      <Dialog
        title="Are you still there?"
        isOpen={warning}
        onOpenChange={() => undefined}
        isDismissable={false}
        size="confirm"
      >
        <p role="timer">
          You&apos;ll be signed out in {clock(idleLeft)} because of inactivity.
        </p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onSignOut}>
            Sign out
          </Button>
          <Button autoFocus onPress={() => void extend()}>
            Stay signed in
          </Button>
        </div>
      </Dialog>
      {renewing && due && (
        <RenewDialog
          isOpen
          deadlines={deadlines}
          onClose={() => setRenewing(false)}
          onRenewed={renewed}
          onEnded={expire}
          onSignOut={onSignOut}
        />
      )}
      {due && !renewing && dismissed !== deadlines.absoluteExpiresAt && (
        <div className="samuori-session-notice">
          <Alert tone="warning">
            <p role="timer">
              Your session ends at {timeOfDay(deadlines.absoluteExpiresAt)}.
              Renew it to keep working.
            </p>
            <div className="samuori-actions">
              <Button
                variant="secondary"
                onPress={() => setDismissed(deadlines.absoluteExpiresAt)}
              >
                Dismiss
              </Button>
              <Button onPress={() => setRenewing(true)}>Renew session</Button>
            </div>
          </Alert>
        </div>
      )}
      {renewedUntil !== undefined && (
        <div className="samuori-session-notice">
          <Alert tone="success">
            Session renewed. It now ends at {timeOfDay(renewedUntil)}.
          </Alert>
        </div>
      )}
    </>
  );
}

interface RenewProps {
  isOpen: boolean;
  deadlines: Deadlines;
  onClose: () => void;
  onRenewed: (s: Schemas["SessionStatus"]) => void;
  onEnded: () => void;
  onSignOut: () => void;
}

function RenewDialog({
  isOpen,
  deadlines,
  onClose,
  onRenewed,
  onEnded,
  onSignOut,
}: RenewProps) {
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const provider = deadlines.renewWith === "provider";
  const name = deadlines.providerName ?? "your identity provider";

  async function renew(e?: FormEvent) {
    e?.preventDefault();
    setBusy(true);
    setError(undefined);
    const {
      data,
      error: problem,
      response,
    } = await api.POST("/auth/session/renewal", {
      body: provider
        ? { returnTo: currentPage(window.location) }
        : { password },
    });
    setBusy(false);
    setPassword("");
    if (response.status === 202 && data && "redirectUrl" in data) {
      window.location.assign(data.redirectUrl);
      return;
    }
    if (data && "absoluteExpiresAt" in data) {
      onRenewed(data);
      return;
    }
    if (response.status === 404) {
      onEnded();
    } else if (response.status === 429) {
      setError("Too many attempts. Wait a minute, then try again.");
    } else if (problem?.code === "wrong_password") {
      setError("That password is not right. Try again.");
    } else {
      setError(
        problem?.message ??
          "The session could not be renewed. Sign out and sign in again.",
      );
    }
  }

  return (
    <Dialog
      title="Renew your session"
      isOpen={isOpen}
      onOpenChange={(open) => !open && onClose()}
      size="confirm"
    >
      {provider ? (
        <>
          <p>
            Continue with {name} to confirm it&apos;s you. You&apos;ll come
            straight back to this page; wizard progress is saved as you go, but
            finish typing in any open form first.
          </p>
          {error && <Alert>{error}</Alert>}
          <div className="samuori-actions">
            <Button variant="secondary" onPress={onSignOut}>
              Sign out
            </Button>
            <Button autoFocus isDisabled={busy} onPress={() => void renew()}>
              {busy && <Spinner decorative />}
              Continue with {name}
            </Button>
          </div>
        </>
      ) : (
        <Form onSubmit={(e) => void renew(e)} className="samuori-form">
          <p>
            Enter your password to start a new session. Nothing on this page is
            lost.
          </p>
          <TextField
            label="Password"
            type="password"
            value={password}
            onChange={setPassword}
            isRequired
            autoFocus
            autoComplete="current-password"
          />
          {error && <Alert>{error}</Alert>}
          <div className="samuori-actions">
            <Button variant="secondary" onPress={onSignOut}>
              Sign out
            </Button>
            <Button type="submit" isDisabled={busy || password === ""}>
              {busy && <Spinner decorative />}
              {busy ? "Renewing…" : "Renew session"}
            </Button>
          </div>
        </Form>
      )}
    </Dialog>
  );
}
