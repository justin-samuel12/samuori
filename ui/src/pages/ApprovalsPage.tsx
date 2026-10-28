import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import {
  RadioGroup,
  Select,
  TextField,
  type Option,
} from "../components/Field";
import { Alert, Loading, PageHeader } from "../components/Feedback";
import { Table } from "../components/Table";
import { useLoader } from "../hooks";
import { useCan } from "../session/session";

type Approval = Schemas["Approval"];

const subjectText: Record<string, string> = {
  publication: "Publication",
  "secret-binding": "Secret binding",
  "security-exception": "Security exception",
  "repository-request": "Repository request",
  "base-image-request": "Base image request",
  "egress-access-request": "Network access request",
};

export function ApprovalsPage() {
  const [pending, setPending] = useState<Approval[]>();
  const [history, setHistory] = useState<Approval[]>([]);
  const [deciding, setDeciding] = useState<Approval>();
  const can = useCan();
  const load = useCallback(async () => {
    const [p, all] = await Promise.all([
      api.GET("/approvals", { params: { query: { state: "pending" } } }),
      api.GET("/approvals", { params: { query: { limit: 50 } } }),
    ]);
    setPending(p.data?.items ?? []);
    setHistory((all.data?.items ?? []).filter((a) => a.state !== "pending"));
  }, []);
  useLoader(load);
  if (!pending) return <Loading />;
  return (
    <>
      <PageHeader
        help="approvals"
        title="Approvals"
        description="Requests that need someone other than the requester to decide."
      />
      <Table
        label="Pending approvals"
        rows={pending}
        empty="Nothing is waiting for a decision."
        columns={[
          {
            id: "what",
            name: "Request",
            isRowHeader: true,
            render: (a) => a.summary,
          },
          {
            id: "kind",
            name: "Kind",
            render: (a) => subjectText[a.subjectType] ?? a.subjectType,
          },
          {
            id: "expires",
            name: "Open until",
            render: (a) => new Date(a.expiresAt).toLocaleString(),
          },
          {
            id: "actions",
            name: "Actions",
            render: (a) =>
              a.canDecide ? (
                <Button
                  variant="secondary"
                  onPress={() => setDeciding(a)}
                  aria-label={`Decide: ${a.summary}`}
                >
                  Decide
                </Button>
              ) : (
                <span className="samuori-muted">Waiting for an approver</span>
              ),
          },
        ]}
      />
      {can("listPendingOutputExceptions") && <PendingExceptions />}
      <h2>Decided</h2>
      <Table
        label="Decided approvals"
        rows={history}
        empty="No decisions yet."
        columns={[
          {
            id: "what",
            name: "Request",
            isRowHeader: true,
            render: (a) => a.summary,
          },
          { id: "state", name: "Outcome", render: (a) => a.state },
          { id: "reason", name: "Reason", render: (a) => a.reason ?? "" },
          {
            id: "when",
            name: "Decided",
            render: (a) =>
              a.decidedAt ? new Date(a.decidedAt).toLocaleString() : "",
          },
        ]}
      />
      <DecideDialog
        approval={deciding}
        onClose={() => setDeciding(undefined)}
        onDone={() => {
          setDeciding(undefined);
          void load();
        }}
      />
    </>
  );
}

function DecideDialog({
  approval,
  onClose,
  onDone,
}: {
  approval?: Approval;
  onClose: () => void;
  onDone: () => void;
}) {
  const [decision, setDecision] = useState("approve");
  const [reason, setReason] = useState("");
  const [destination, setDestination] = useState<string | null>(null);
  const [destinations, setDestinations] = useState<Option[]>([]);
  const [error, setError] = useState<string>();
  const fulfills =
    approval?.subjectType === "repository-request" && decision === "approve";
  useEffect(() => {
    if (approval?.subjectType !== "repository-request") return;
    void (async () => {
      const { data } = await api.GET("/destinations");
      setDestinations(
        (data?.items ?? [])
          .filter((d) => d.allowedProjectIds?.includes(approval.projectId))
          .map((d) => ({ id: d.id, label: `${d.name} (${d.repository})` })),
      );
    })();
  }, [approval]);
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!approval) return;
    if (fulfills) {
      if (!destination) {
        setError("Choose the destination that fulfills the request.");
        return;
      }
      const { error } = await api.POST("/approvals/{approvalId}/fulfillment", {
        params: { path: { approvalId: approval.id } },
        body: { destinationId: destination, reason },
      });
      if (error) setError(describeError(error));
      else {
        setReason("");
        onDone();
      }
      return;
    }
    const { error } = await api.POST("/approvals/{approvalId}/decision", {
      params: {
        path: { approvalId: approval.id },
        header: { "Idempotency-Key": crypto.randomUUID() },
      },
      body: { decision: decision as "approve" | "reject", reason },
    });
    if (error) setError(describeError(error));
    else {
      setReason("");
      onDone();
    }
  }
  return (
    <Dialog
      title="Decide request"
      isOpen={!!approval}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>{approval?.summary}</p>
        {approval?.subjectType === "base-image-request" && (
          <Alert tone="info">
            Approving adds the repository to the organization&apos;s base-images
            policy as a new version, so every project may build from it. Pulls
            from a registry without an approved image source or mirror still
            need egress to it.
          </Alert>
        )}
        {approval?.subjectType === "egress-access-request" && (
          <EgressRequestDetails approval={approval} />
        )}
        <RadioGroup
          label="Decision"
          value={decision}
          onChange={setDecision}
          options={[
            {
              id: "approve",
              label: "Approve",
              description: "The request may go ahead.",
            },
            {
              id: "reject",
              label: "Reject",
              description: "The request is closed.",
            },
          ]}
        />
        {fulfills && (
          <Select
            label="Destination"
            value={destination}
            onChange={setDestination}
            options={destinations}
            isRequired
            description="Create the repository and a destination open to the project under Registries first."
          />
        )}
        <TextField
          label="Reason"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={10}
          description="At least 10 characters; recorded with the decision."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Record decision</Button>
        </div>
      </Form>
    </Dialog>
  );
}

// The evidence shown here was taken by the server from stored data when the request was made (ADR 0052).
function EgressRequestDetails({ approval }: { approval: Approval }) {
  const [request, setRequest] = useState<Schemas["EgressAccessRequest"]>();
  useEffect(() => {
    void api
      .GET("/projects/{projectId}/egress-access-requests", {
        params: { path: { projectId: approval.projectId } },
      })
      .then(({ data }) =>
        setRequest(data?.items.find((r) => r.id === approval.subjectId)),
      );
  }, [approval]);
  if (!request) return null;
  const ev = request.evidence;
  const opens = [
    ...request.presets.map((p) => `the ${p} preset`),
    ...request.destinations,
    ...(request.internalDestinations ?? []).map((d) => `${d} (internal)`),
  ].join(", ");
  const internal = request.internalDestinations ?? [];
  return (
    <>
      {internal.length > 0 && (
        <Alert tone="warning">
          <strong>Internal network access.</strong> Approving lets this
          project&apos;s builds reach <strong>{internal.join(", ")}</strong>{" "}
          even when it resolves to a private address inside your network.
          Uploaded code runs in these builds, so approve only hosts the project
          really needs.
        </Alert>
      )}
      <Alert tone="info">
        Approving opens <strong>{opens}</strong> for{" "}
        <strong>{request.method}</strong> builds of this project only. The
        organization egress policy and other projects are unchanged. Set an end
        date with the decision API&apos;s validUntil if the access should lapse;
        otherwise it lasts until an administrator revokes it on the project
        page.
      </Alert>
      <dl className="samuori-details">
        <dt>Evidence</dt>
        <dd>
          {ev.kind === "build" ? (
            <>
              Build <code>{ev.buildId}</code>; its egress proxy denied{" "}
              {(ev.denied ?? []).join(", ")}
            </>
          ) : (
            <>
              Build readiness <code>{ev.code}</code>
              {ev.path && (
                <>
                  {" "}
                  at{" "}
                  <code>
                    {ev.path}
                    {ev.line ? `:${ev.line}` : ""}
                  </code>
                </>
              )}
              : {ev.message}
            </>
          )}
        </dd>
        <dt>Justification</dt>
        <dd>{request.justification}</dd>
      </dl>
    </>
  );
}

type OutputException = Schemas["OutputException"];

function PendingExceptions() {
  const [items, setItems] = useState<OutputException[]>();
  const [deciding, setDeciding] = useState<OutputException>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/output-exceptions");
    setItems(data?.items ?? []);
  }, []);
  useLoader(load);
  if (!items) return <Loading />;
  return (
    <section className="samuori-section" aria-labelledby="pending-exceptions-h">
      <h2 id="pending-exceptions-h">Output exception requests</h2>
      <Table
        label="Output exception requests"
        rows={items}
        empty="No output exception is waiting for a decision."
        columns={[
          {
            id: "paths",
            name: "Paths",
            isRowHeader: true,
            render: (x) => <code>{x.pathPatterns.join(", ")}</code>,
          },
          {
            id: "classes",
            name: "Classes",
            render: (x) => (x.classes.length ? x.classes.join(", ") : "any"),
          },
          {
            id: "why",
            name: "Justification",
            render: (x) => x.justification,
          },
          {
            id: "expires",
            name: "Would expire",
            render: (x) => new Date(x.expiresAt).toLocaleString(),
          },
          {
            id: "actions",
            name: "Actions",
            render: (x) => (
              <Button
                variant="secondary"
                onPress={() => setDeciding(x)}
                aria-label={`Decide exception for ${x.pathPatterns.join(", ")}`}
              >
                Decide
              </Button>
            ),
          },
        ]}
      />
      <DecideExceptionDialog
        exception={deciding}
        onClose={() => setDeciding(undefined)}
        onDone={() => {
          setDeciding(undefined);
          void load();
        }}
      />
    </section>
  );
}

function DecideExceptionDialog({
  exception,
  onClose,
  onDone,
}: {
  exception?: OutputException;
  onClose: () => void;
  onDone: () => void;
}) {
  const [decision, setDecision] = useState("approve");
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!exception) return;
    const { error } = await api.POST(
      "/output-exceptions/{exceptionId}/decision",
      {
        params: {
          path: { exceptionId: exception.id },
        },
        body: { approve: decision === "approve", reason },
      },
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
      title="Decide output exception"
      isOpen={!!exception}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          <code>{exception?.pathPatterns.join(", ")}</code>
        </p>
        <p>{exception?.justification}</p>
        <Alert tone="info">
          Images that pass only through this exception are restricted to
          destinations that accept restricted images.
        </Alert>
        <RadioGroup
          label="Decision"
          value={decision}
          onChange={setDecision}
          options={[
            {
              id: "approve",
              label: "Approve",
              description:
                "Matching findings pass until the exception expires.",
            },
            {
              id: "reject",
              label: "Reject",
              description: "Matching findings keep failing validation.",
            },
          ]}
        />
        <TextField
          label="Reason"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={10}
          description="At least 10 characters; recorded with the decision."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Record decision</Button>
        </div>
      </Form>
    </Dialog>
  );
}
