import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { TextField } from "../components/Field";
import { Alert } from "../components/Feedback";
import { Table } from "../components/Table";
import { useLoader } from "../hooks";
import { isForbidden } from "../session/session";

type BaseImageRequest = Schemas["BaseImageRequest"];

const stateText: Record<BaseImageRequest["state"], string> = {
  pending: "Waiting for an organization administrator",
  approved: "Approved; builds may use it",
  rejected: "Rejected",
  expired: "Expired",
};

/** The project's base-image requests, a group of Requests & exceptions that renders nothing when there are none. */
export function BaseImageRequests({
  projectId,
  onCount,
}: {
  projectId: string;
  onCount: (n: number) => void;
}) {
  const [forbidden, setForbidden] = useState(false);
  const [items, setItems] = useState<BaseImageRequest[]>();
  const load = useCallback(async () => {
    const res = await api.GET("/projects/{projectId}/base-image-requests", {
      params: { path: { projectId } },
    });
    setForbidden(isForbidden(res));
    setItems(res.data?.items ?? []);
  }, [projectId]);
  useLoader(load);
  const count = forbidden ? 0 : items?.length;
  useEffect(() => {
    if (count !== undefined) onCount(count);
  }, [count, onCount]);
  if (!count || !items) return null;
  return (
    <div>
      <h3>Base images</h3>
      <p className="samuori-muted">
        Outside image repositories asked for the organization&apos;s approved
        base images; builds may use one once an administrator approves it.
      </p>
      {items && (
        <Table
          label="Base image requests"
          rows={items}
          columns={[
            {
              id: "repo",
              name: "Repository",
              isRowHeader: true,
              render: (r) => <code>{r.repository}</code>,
            },
            {
              id: "state",
              name: "State",
              render: (r) =>
                r.reason && r.state !== "pending"
                  ? `${stateText[r.state]}: ${r.reason}`
                  : stateText[r.state],
            },
            { id: "why", name: "Reason", render: (r) => r.justification },
            {
              id: "when",
              name: "Requested",
              render: (r) => new Date(r.createdAt).toLocaleString(),
            },
          ]}
        />
      )}
    </div>
  );
}

export function RequestBaseImageDialog({
  projectId,
  repository: initial = "",
  isOpen,
  onClose,
  onDone,
}: {
  projectId: string;
  repository?: string;
  isOpen: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const [repository, setRepository] = useState(initial);
  const [justification, setJustification] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    const { error } = await api.POST(
      "/projects/{projectId}/base-image-requests",
      {
        params: { path: { projectId } },
        body: { repository: repository.trim(), justification },
      },
    );
    if (error) setError(describeError(error));
    else {
      setJustification("");
      onDone();
    }
  }
  return (
    <Dialog
      title="Request a base image repository"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          label="Repository"
          value={repository}
          onChange={setRepository}
          isRequired
          description="Registry host and path without tag or digest, such as mcr.microsoft.com/mssql/server."
        />
        <TextField
          label="Why it is needed"
          value={justification}
          onChange={setJustification}
          isRequired
          minLength={20}
          multiline
          description="At least 20 characters. An organization administrator reads this before deciding."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Send request</Button>
        </div>
      </Form>
    </Dialog>
  );
}
