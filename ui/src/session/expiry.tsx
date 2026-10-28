import { useCallback, useEffect, useRef, useState } from "react";
import { api, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { Alert } from "../components/Feedback";
import { useLoader } from "../hooks";
import { broadcast, onBroadcast, type Deadlines } from "./broadcast";

// Session expiry follows docs/architecture/identity.md#sessions: reads never renew the idle deadline, real activity
// renews it at most once a minute once half of it has passed, and the absolute maximum is never extended.
const extendEveryMs = 60_000;
const finalNoticeMs = 10 * 60_000;
const syncEveryMs = 120_000;
const slackMs = 5_000;

let known: Deadlines | undefined;

function toDeadlines(s: Schemas["SessionStatus"]): Deadlines {
  const offset = Date.parse(s.serverTime) - Date.now();
  return {
    idleExpiresAt: Date.parse(s.idleExpiresAt) - offset,
    absoluteExpiresAt: Date.parse(s.absoluteExpiresAt) - offset,
    idleTimeoutMs: s.idleTimeoutSeconds * 1000,
    warningMs: s.warningSeconds * 1000,
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

interface Props {
  onExpired: (notice: string) => void;
  onSignOut: () => void;
}

export function SessionExpiry({ onExpired, onSignOut }: Props) {
  const [deadlines, setDeadlines] = useState<Deadlines | undefined>(known);
  const [now, setNow] = useState(() => Date.now());
  const [dismissed, setDismissed] = useState<number>();
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
        if (m.type === "deadlines") apply(m.deadlines, false);
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
  const warning = !capped && idleLeft > 0 && idleLeft <= deadlines.warningMs;
  const absoluteLeft = deadlines.absoluteExpiresAt - now;
  const ends = new Date(deadlines.absoluteExpiresAt).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
  });
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
      {absoluteLeft > 0 &&
        absoluteLeft <= finalNoticeMs &&
        dismissed !== deadlines.absoluteExpiresAt && (
          <div className="samuori-session-notice">
            <Alert tone="warning">
              <p>
                Your session ends at {ends}. Sessions last at most a fixed time
                and cannot be extended; sign in again to start a new one.
              </p>
              <div className="samuori-actions">
                <Button
                  variant="secondary"
                  onPress={() => setDismissed(deadlines.absoluteExpiresAt)}
                >
                  Dismiss
                </Button>
                <Button onPress={onSignOut}>Sign in again now</Button>
              </div>
            </Alert>
          </div>
        )}
    </>
  );
}
