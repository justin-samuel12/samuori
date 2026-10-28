import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Alert } from "../../components/Feedback";

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
