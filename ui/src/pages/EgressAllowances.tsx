import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { TextField } from "../components/Field";
import { Alert, Badge } from "../components/Feedback";
import { Table } from "../components/Table";
import { useLoader } from "../hooks";
import { useCan } from "../session/session";

type Allowance = Schemas["EgressAllowance"];

const statusText: Record<Allowance["status"], string> = {
  active: "Active",
  revoked: "Revoked",
  expired: "Expired",
};

// Allowances open egress for this project only (ADR 0052); the organization egress policy is unchanged. A group of
// Requests & exceptions that renders nothing when there are none.
export function EgressAllowances({
  projectId,
  onCount,
}: {
  projectId: string;
  onCount: (n: number) => void;
}) {
  const admin = useCan()("revokeEgressAllowance");
  const [items, setItems] = useState<Allowance[]>();
  const [revoking, setRevoking] = useState<Allowance>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/projects/{projectId}/egress-allowances", {
      params: { path: { projectId } },
    });
    setItems(data?.items ?? []);
  }, [projectId]);
  useLoader(load);
  const count = items?.length;
  useEffect(() => {
    if (count !== undefined) onCount(count);
  }, [count, onCount]);
  if (!count || !items) return null;
  return (
    <div>
      <h3>Network access</h3>
      <p className="samuori-muted">
        Egress presets and destinations an organization administrator opened for
        this project only, after a request from its build readiness or a blocked
        build. Other projects keep the organization&apos;s egress policy.
      </p>
      {items && (
        <Table
          label="Project egress allowances"
          rows={items}
          columns={[
            {
              id: "what",
              name: "Opens",
              isRowHeader: true,
              render: (a) =>
                a.kind === "preset" ? (
                  <>
                    the <code>{a.preset}</code> preset
                  </>
                ) : (
                  <>
                    <code>{a.destination}</code>
                    {a.network === "internal" && (
                      <>
                        {" "}
                        <Badge tone="warn">Internal network</Badge>
                      </>
                    )}
                  </>
                ),
            },
            {
              id: "methods",
              name: "For builds",
              render: (a) => a.methods.join(", "),
            },
            {
              id: "status",
              name: "Status",
              render: (a) =>
                a.status === "revoked" && a.revokeReason
                  ? `${statusText[a.status]}: ${a.revokeReason}`
                  : a.expiresAt && a.status === "active"
                    ? `Active until ${new Date(a.expiresAt).toLocaleString()}`
                    : statusText[a.status],
            },
            { id: "why", name: "Reason", render: (a) => a.reason },
            {
              id: "actions",
              name: "Actions",
              render: (a) =>
                admin && a.status === "active" ? (
                  <Button
                    variant="secondary"
                    onPress={() => setRevoking(a)}
                    aria-label={`Revoke ${a.preset ?? a.destination}`}
                  >
                    Revoke
                  </Button>
                ) : null,
            },
          ]}
        />
      )}
      <RevokeDialog
        allowance={revoking}
        onClose={() => setRevoking(undefined)}
        onDone={() => {
          setRevoking(undefined);
          void load();
        }}
      />
    </div>
  );
}

function RevokeDialog({
  allowance,
  onClose,
  onDone,
}: {
  allowance?: Allowance;
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!allowance) return;
    const { error } = await api.POST(
      "/egress-allowances/{allowanceId}/revocation",
      { params: { path: { allowanceId: allowance.id } }, body: { reason } },
    );
    if (error) setError(describeError(error));
    else {
      setReason("");
      setError(undefined);
      onDone();
    }
  }
  return (
    <Dialog
      title="Revoke network access"
      isOpen={!!allowance}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          Builds of this project planned from now on no longer reach{" "}
          <code>{allowance?.preset ?? allowance?.destination}</code>. Builds
          already planned keep the allowlist they were planned with.
        </p>
        <TextField
          label="Reason"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={10}
          description="At least 10 characters; recorded in the audit log."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Revoke</Button>
        </div>
      </Form>
    </Dialog>
  );
}
