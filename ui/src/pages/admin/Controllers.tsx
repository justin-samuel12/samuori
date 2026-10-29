import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { TextField } from "../../components/Field";
import { Alert } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type Status = Schemas["UpgradeStatus"];
type JoinToken = Schemas["ControllerJoinToken"];
type Issued = Schemas["ControllerJoinTokenIssued"];
type JoinRequest = Schemas["ControllerJoinRequest"];
type Removal = Schemas["ControllerRemoval"];

function removalText(r: Removal): string {
  if (r.state === "requested")
    return "Removing: handing over leadership and asking the host to wipe Samuori";
  if (r.state === "wiping") return "Removing: the host is wiping Samuori";
  return r.outcome === "wiped"
    ? "Removed; the host wiped Samuori"
    : `Removed; not confirmed wiped (${r.detail ?? ""}). Run sudo samuori-uninstall on the host or terminate it, then consider changing the database password`;
}

function RemoveControllerDialog({
  hostname,
  onClose,
  onDone,
}: {
  hostname?: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!hostname) return;
    setError(undefined);
    const { error } = await api.POST(
      "/system/cluster/controllers/{hostname}/remove",
      { params: { path: { hostname } }, body: { reason } },
    );
    if (error) setError(describeError(error));
    else {
      setReason("");
      setConfirm("");
      onDone();
    }
  }
  return (
    <Dialog
      title={`Remove ${hostname ?? "controller"}`}
      isOpen={!!hostname}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <Alert tone="warning">
          Leadership moves to another controller, then the host deletes Samuori
          (services, files, keys, users and the folder it was installed from),
          its identity is revoked and the other NATS members forget it. Coming
          back means installing again. Removing the last controller, one that
          would leave messaging without a majority, or any controller while
          another is down or an update runs, is refused.
        </Alert>
        <TextField
          label="Reason"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={3}
          maxLength={500}
          description="Recorded in the audit log."
        />
        <TextField
          label={`Type ${hostname ?? ""} to confirm`}
          value={confirm}
          onChange={setConfirm}
          isRequired
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant="danger"
            isDisabled={confirm !== hostname || reason.trim().length < 3}
          >
            Remove
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

// ControllerList shows the controllers sharing the database, with Remove for administrators, and past removals.
export function ControllerList({
  status,
  admin,
  onChanged,
}: {
  status: Status;
  admin: boolean;
  onChanged: () => void;
}) {
  const [removing, setRemoving] = useState<string>();
  const removals = status.removals ?? [];
  const beingRemoved = new Set(
    removals.filter((r) => r.state !== "removed").map((r) => r.hostname),
  );
  return (
    <>
      <Table
        label="Controllers"
        rows={status.controllers}
        empty="No controller is registered."
        columns={[
          {
            id: "host",
            name: "Host",
            isRowHeader: true,
            render: (c) =>
              `${c.host || c.hostname}${c.self ? " (this one)" : ""}`,
          },
          { id: "version", name: "Release", render: (c) => c.version },
          {
            id: "state",
            name: "State",
            render: (c) =>
              !c.live ? "Stopped" : c.leader ? "Running, leader" : "Running",
          },
          {
            id: "seen",
            name: "Last heartbeat",
            render: (c) => new Date(c.heartbeatAt).toLocaleString(),
          },
          {
            id: "compatible",
            name: "Upgrade window",
            render: (c) => (c.compatible ? "Compatible" : "Outside"),
          },
          ...(admin
            ? [
                {
                  id: "actions",
                  name: "Actions",
                  render: (c: Status["controllers"][number]) =>
                    beingRemoved.has(c.hostname) ? (
                      "Removing"
                    ) : (
                      <Button
                        variant="quiet-danger"
                        onPress={() => setRemoving(c.hostname)}
                        aria-label={`Remove ${c.hostname}`}
                      >
                        Remove
                      </Button>
                    ),
                },
              ]
            : []),
        ]}
      />
      {removals.length > 0 && (
        <>
          <h3>Removed controllers</h3>
          <Table
            label="Controller removals"
            rows={removals.map((r) => ({ ...r, id: r.hostname }))}
            empty=""
            columns={[
              {
                id: "host",
                name: "Host",
                isRowHeader: true,
                render: (r) => r.host || r.hostname,
              },
              { id: "state", name: "State", render: (r) => removalText(r) },
              { id: "reason", name: "Reason", render: (r) => r.reason },
              {
                id: "requested",
                name: "Requested",
                render: (r) =>
                  `${new Date(r.requestedAt).toLocaleString()} by ${r.requestedBy}`,
              },
            ]}
          />
        </>
      )}
      <RemoveControllerDialog
        hostname={removing}
        onClose={() => setRemoving(undefined)}
        onDone={() => {
          setRemoving(undefined);
          onChanged();
        }}
      />
    </>
  );
}

function tokenState(t: JoinToken) {
  if (t.usedAt) return `Used by ${t.usedHost ?? "a controller"}`;
  if (t.revokedAt) return "Revoked";
  if (new Date(t.expiresAt) < new Date()) return "Expired";
  return "Unused";
}

const requestStates: Record<JoinRequest["state"], string> = {
  pending: "Waiting for a decision",
  accepted: "Accepted; the host is collecting its configuration",
  rejected: "Rejected",
  completed: "Configuration sent",
  expired: "Expired",
};

function DecideJoinDialog({
  request,
  decision,
  onClose,
  onDone,
}: {
  request?: JoinRequest;
  decision: "accept" | "reject";
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!request) return;
    setError(undefined);
    const { error } = await api.POST(
      "/system/cluster/join-requests/{requestId}/decision",
      {
        params: { path: { requestId: request.id } },
        body: { decision, reason: reason || undefined },
      },
    );
    if (error) setError(describeError(error));
    else {
      setReason("");
      onDone();
    }
  }
  const accept = decision === "accept";
  return (
    <Dialog
      title={`${accept ? "Accept" : "Reject"} join request from ${request?.host ?? ""}`}
      isOpen={!!request}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        {accept ? (
          <p>
            Accept only if the host <strong>{request?.host}</strong> shows the
            verification code <code>{request?.verificationCode}</code>. It then
            receives this cluster&apos;s configuration, key, certificates and
            database credentials.
          </p>
        ) : (
          <p>
            <strong>{request?.host}</strong> will not join; its token stays
            used.
          </p>
        )}
        <TextField
          label="Reason (optional)"
          value={reason}
          onChange={setReason}
          maxLength={500}
          description="Recorded with the decision in the audit log."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">{accept ? "Accept" : "Reject"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}

// JoinRequests lists hosts that redeemed a token; none receives anything until an administrator accepts it.
function JoinRequests() {
  const [requests, setRequests] = useState<JoinRequest[]>();
  const [deciding, setDeciding] = useState<{
    request: JoinRequest;
    decision: "accept" | "reject";
  }>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/system/cluster/join-requests");
    setRequests(data?.items ?? []);
  }, []);
  useLoader(load);
  // A request opens on the joining host, so the list is polled while the page is open.
  useEffect(() => {
    const t = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(t);
  }, [load]);
  if (!requests || requests.length === 0) return null;
  return (
    <>
      <h3>Join requests</h3>
      <Table
        label="Join requests"
        rows={requests}
        empty=""
        columns={[
          {
            id: "host",
            name: "Host",
            isRowHeader: true,
            render: (r) => r.host,
          },
          { id: "address", name: "Address", render: (r) => r.remoteAddress },
          {
            id: "code",
            name: "Verification code",
            render: (r) => <code>{r.verificationCode}</code>,
          },
          {
            id: "requested",
            name: "Requested",
            render: (r) => new Date(r.createdAt).toLocaleString(),
          },
          {
            id: "state",
            name: "State",
            render: (r) =>
              requestStates[r.state] + (r.reason ? ` (${r.reason})` : ""),
          },
          {
            id: "actions",
            name: "Actions",
            render: (r) =>
              r.state === "pending" ? (
                <div className="samuori-actions samuori-actions--start">
                  <Button
                    variant="secondary"
                    onPress={() =>
                      setDeciding({ request: r, decision: "accept" })
                    }
                  >
                    Accept
                  </Button>
                  <Button
                    variant="quiet-danger"
                    onPress={() =>
                      setDeciding({ request: r, decision: "reject" })
                    }
                  >
                    Reject
                  </Button>
                </div>
              ) : null,
          },
        ]}
      />
      <DecideJoinDialog
        request={deciding?.request}
        decision={deciding?.decision ?? "reject"}
        onClose={() => setDeciding(undefined)}
        onDone={() => {
          setDeciding(undefined);
          void load();
        }}
      />
    </>
  );
}

// joinCommand is one line with plain spaces, so it pastes into a shell unchanged.
function joinCommand(t: Issued): string {
  return `sudo ./samuori-join controller ${t.code}`;
}

// JoinControllers issues single-use tokens that let another host join this cluster without copying keys.
export function JoinControllers() {
  const [tokens, setTokens] = useState<JoinToken[]>();
  const [issued, setIssued] = useState<Issued>();
  const [error, setError] = useState<string>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/system/cluster/join-tokens");
    setTokens(data?.items ?? []);
  }, []);
  useLoader(load);

  async function issue() {
    setError(undefined);
    const { data, error } = await api.POST("/system/cluster/join-tokens", {
      body: { ttlSeconds: 1800 },
    });
    if (data) setIssued(data);
    else setError(describeError(error));
    void load();
  }

  async function revoke(t: JoinToken) {
    setError(undefined);
    const { error } = await api.DELETE(
      "/system/cluster/join-tokens/{tokenId}",
      {
        params: { path: { tokenId: t.id } },
      },
    );
    if (error) setError(describeError(error));
    void load();
  }

  return (
    <section className="samuori-section" aria-labelledby="join-h">
      <h2 id="join-h">Add a controller</h2>
      <p>
        A join code lets one more host ask to become a controller of this
        cluster. The host shows a verification code and waits; once you accept
        its request below, it receives the cluster&apos;s configuration, key and
        certificates over a connection pinned to this controller. Nothing is
        copied by hand.
      </p>
      {error && <Alert>{error}</Alert>}
      {issued ? (
        <Alert tone="success">
          <p>
            <strong>
              Join code for one controller. It is shown once and expires{" "}
              {new Date(issued.expiresAt).toLocaleString()}.
            </strong>
          </p>
          <p>
            On the new host, run this from the release folder. It uses the
            host&apos;s DNS name (add <code>--host NAME</code> to choose
            another). Then accept its join request below once the verification
            codes match.
          </p>
          <code className="samuori-secret" data-testid="join-command">
            {joinCommand(issued)}
          </code>
          <div className="samuori-actions samuori-actions--start">
            <Button
              variant="secondary"
              onPress={() =>
                void navigator.clipboard.writeText(joinCommand(issued))
              }
            >
              Copy command
            </Button>
          </div>
          <div className="samuori-actions">
            <Button variant="quiet" onPress={() => setIssued(undefined)}>
              Done
            </Button>
          </div>
        </Alert>
      ) : (
        <Button onPress={() => void issue()}>Create a join code</Button>
      )}
      {tokens && tokens.length > 0 && (
        <Table
          label="Join tokens"
          rows={tokens}
          empty=""
          columns={[
            {
              id: "created",
              name: "Created",
              isRowHeader: true,
              render: (t) => new Date(t.createdAt).toLocaleString(),
            },
            { id: "state", name: "State", render: tokenState },
            {
              id: "actions",
              name: "Actions",
              render: (t) =>
                tokenState(t) === "Unused" ? (
                  <Button variant="quiet-danger" onPress={() => void revoke(t)}>
                    Revoke
                  </Button>
                ) : null,
            },
          ]}
        />
      )}
      <JoinRequests />
    </section>
  );
}
