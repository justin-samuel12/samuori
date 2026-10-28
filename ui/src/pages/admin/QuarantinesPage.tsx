import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { TextField } from "../../components/Field";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type Quarantine = Schemas["ArtifactQuarantine"];

export function QuarantinesPage() {
  const [items, setItems] = useState<Quarantine[]>();
  const [releasing, setReleasing] = useState<Quarantine>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/artifact-quarantines");
    setItems(data?.items ?? []);
  }, []);
  useLoader(load);
  if (!items) return <Loading />;
  return (
    <>
      <PageHeader
        help="security-review"
        title="Quarantined images"
        description="Images built by a worker that was revoked as compromised. They cannot be published until a security reviewer or administrator releases them after review."
      />
      <Table
        label="Quarantined images"
        rows={items}
        empty="No image is quarantined."
        columns={[
          {
            id: "digest",
            name: "Image",
            isRowHeader: true,
            render: (q) => <code>{q.digest.slice(7, 19)}</code>,
          },
          {
            id: "worker",
            name: "Built by worker",
            render: (q) => <code>{q.workerId}</code>,
          },
          { id: "reason", name: "Reason", render: (q) => q.reason },
          {
            id: "since",
            name: "Quarantined",
            render: (q) => new Date(q.createdAt).toLocaleString(),
          },
          {
            id: "state",
            name: "State",
            render: (q) =>
              q.releasedAt ? `Released: ${q.releaseReason ?? ""}` : "Held",
          },
          {
            id: "actions",
            name: "Actions",
            render: (q) =>
              !q.releasedAt && (
                <Button
                  variant="secondary"
                  onPress={() => setReleasing(q)}
                  aria-label={`Release ${q.digest.slice(7, 19)}`}
                >
                  Release
                </Button>
              ),
          },
        ]}
      />
      <ReleaseDialog
        quarantine={releasing}
        onClose={() => setReleasing(undefined)}
        onDone={() => {
          setReleasing(undefined);
          void load();
        }}
      />
    </>
  );
}

function ReleaseDialog({
  quarantine,
  onClose,
  onDone,
}: {
  quarantine?: Quarantine;
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!quarantine) return;
    const { error } = await api.POST(
      "/artifact-quarantines/{quarantineId}/release",
      {
        params: { path: { quarantineId: quarantine.id } },
        body: { reason },
      },
    );
    if (error) setError(describeError(error));
    else {
      setReason("");
      onDone();
    }
  }
  return (
    <Dialog
      title="Release a quarantined image"
      isOpen={!!quarantine}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          Record what the review found, for example that the image was rebuilt
          on a clean worker and matched. The release is audited.
        </p>
        <TextField
          label="Review result"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={10}
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Release</Button>
        </div>
      </Form>
    </Dialog>
  );
}
