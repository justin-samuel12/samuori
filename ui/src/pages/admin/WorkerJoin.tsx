import { useCallback, useState } from "react";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Alert } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

export type WorkerToken = Schemas["EnrollmentTokenIssued"];

export async function issueWorkerToken(): Promise<
  { token: WorkerToken } | { error: string }
> {
  const { data, error } = await api.POST("/enrollment-tokens", {
    params: { header: { "Idempotency-Key": crypto.randomUUID() } },
    body: { ttlSeconds: 3600 },
  });
  return data ? { token: data } : { error: describeError(error) };
}

function joinCommand(code: string): string {
  return `sudo ./samuori-join worker ${code}`;
}

// WorkerJoinNotice shows a freshly issued worker token once: as one join code for samuori-join, and as the CA pin and
// token for a host already installed with install.sh --worker-only.
export function WorkerJoinNotice({
  token,
  onDone,
}: {
  token: WorkerToken;
  onDone: () => void;
}) {
  const code = token.code;
  return (
    <Alert tone="success">
      <p>
        <strong>
          Join code for one worker. It is shown once and expires{" "}
          {new Date(token.expiresAt).toLocaleString()}.
        </strong>
      </p>
      {code ? (
        <>
          <p>
            On the worker VM, run this from the release folder. It installs the
            worker and enrolls it in one step:
          </p>
          <code className="samuori-secret" data-testid="enroll-join-command">
            {joinCommand(code)}
          </code>
          <div className="samuori-actions justify-start">
            <Button
              variant="secondary"
              onPress={() =>
                void navigator.clipboard.writeText(joinCommand(code))
              }
            >
              Copy command
            </Button>
          </div>
          <p>
            Or, on a worker host already installed with{" "}
            <code>install.sh --worker-only</code>, run the command below and
            paste the token when asked:
          </p>
        </>
      ) : (
        <p>
          This controller&apos;s worker endpoint listens on this host only, so
          no join code is offered. On a worker host installed with{" "}
          <code>install.sh --worker-only</code>, run the command below and paste
          the token when asked:
        </p>
      )}
      <code className="samuori-secret" data-testid="enroll-command">
        sudo samuori-enroll-worker {token.caPin}
      </code>
      <code className="samuori-secret" data-testid="enroll-token">
        {token.token}
      </code>
      <p>
        The worker joins unassigned: connected, but with no builds, secrets or
        self-test until you assign it to a pool.
      </p>
      <div className="samuori-actions">
        <Button variant="quiet" onPress={onDone}>
          Done
        </Button>
      </div>
    </Alert>
  );
}

type EnrollmentToken = Schemas["EnrollmentToken"];

function usable(t: EnrollmentToken): boolean {
  return !t.usedAt && !t.revokedAt && new Date(t.expiresAt) > new Date();
}

function enrollmentState(t: EnrollmentToken): string {
  if (t.usedAt) return `Used by ${t.workerName ?? t.workerId ?? "a worker"}`;
  if (t.revokedAt)
    return `Revoked${t.revokedBy ? ` by ${t.revokedBy}` : ""}, ${new Date(t.revokedAt).toLocaleString()}`;
  if (!usable(t)) return "Expired";
  return `Unused; expires ${new Date(t.expiresAt).toLocaleString()}`;
}

// EnrollmentTokens lists recent worker join codes without their values, so an administrator can revoke one shared
// by mistake before it expires.
export function EnrollmentTokens() {
  const [tokens, setTokens] = useState<EnrollmentToken[]>();
  const [error, setError] = useState<string>();
  const load = useCallback(async () => {
    const { data, error } = await api.GET("/enrollment-tokens");
    if (data) setTokens(data.items);
    else setError(describeError(error));
  }, []);
  useLoader(load);

  async function revoke(t: EnrollmentToken) {
    setError(undefined);
    const { error } = await api.DELETE("/enrollment-tokens/{tokenId}", {
      params: { path: { tokenId: t.id } },
    });
    if (error) setError(describeError(error));
    void load();
  }

  if (!tokens?.length && !error) return null;
  return (
    <section className="samuori-section" aria-labelledby="enroll-tokens-h">
      <h2 id="enroll-tokens-h">Worker join codes</h2>
      <p className="samuori-muted">
        Each code enrolls one worker. Revoke an unused code that was shared by
        mistake; a revoked code can never enroll a worker. To end a worker that
        already joined, revoke the worker instead.
      </p>
      {error && <Alert>{error}</Alert>}
      <Table
        label="Worker join codes"
        rows={tokens ?? []}
        empty=""
        columns={[
          {
            id: "created",
            name: "Created",
            isRowHeader: true,
            render: (t) => new Date(t.createdAt).toLocaleString(),
          },
          { id: "by", name: "Issued by", render: (t) => t.createdBy },
          { id: "state", name: "State", render: enrollmentState },
          {
            id: "actions",
            name: "Actions",
            render: (t) =>
              usable(t) ? (
                <Button
                  variant="quiet"
                  onPress={() => void revoke(t)}
                  aria-label={`Revoke the join code created ${new Date(t.createdAt).toLocaleString()}`}
                >
                  Revoke
                </Button>
              ) : null,
          },
        ]}
      />
    </section>
  );
}
