import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { TextField } from "../../components/Field";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type Bundle = Schemas["TrustBundleStatus"];

const stateText: Record<string, string> = {
  issuing: "Issuing",
  next: "Next (trusted, not issuing yet)",
  retiring: "Retiring",
};

export function CertificatesPage() {
  const [bundle, setBundle] = useState<Bundle>();
  const [error, setError] = useState<string>();
  const [mode, setMode] = useState<"planned" | "emergency">();
  const load = useCallback(async () => {
    const { data, error } = await api.GET("/pki");
    if (data) setBundle(data);
    else setError(describeError(error));
  }, []);
  useLoader(load);

  async function switchIssuance() {
    setError(undefined);
    const { data, error } = await api.POST("/pki/rotations/switch", {});
    if (data) setBundle(data);
    else setError(describeError(error));
  }

  if (!bundle && !error) return <Loading />;
  return (
    <>
      <PageHeader
        title="Certificates"
        description="The internal certificate authority that identifies workers. Workers pin the CA, not a server certificate, and receive the trust bundle on every connection."
        actions={
          bundle && (
            <div className="samuori-actions">
              {bundle.rotationInProgress ? (
                <Button
                  onPress={() => void switchIssuance()}
                  isDisabled={bundle.pendingWorkers.length > 0}
                >
                  Switch to the new CA
                </Button>
              ) : (
                <Button onPress={() => setMode("planned")}>
                  Rotate the CA
                </Button>
              )}
              <Button variant="danger" onPress={() => setMode("emergency")}>
                Emergency rotation
              </Button>
            </div>
          )
        }
      />
      {error && <Alert>{error}</Alert>}
      {bundle && (
        <>
          <dl className="samuori-details">
            <dt>Trust bundle version</dt>
            <dd>{bundle.bundleVersion}</dd>
            <dt>CA pin for new workers</dt>
            <dd>
              <code>{bundle.issuingPin}</code>
            </dd>
          </dl>
          <Table
            label="Certificate authorities"
            rows={bundle.anchors.map((a) => ({ ...a, id: a.pin }))}
            empty="No CA."
            columns={[
              {
                id: "pin",
                name: "Pin",
                isRowHeader: true,
                render: (a) => <code>{a.pin.slice(0, 23)}…</code>,
              },
              {
                id: "state",
                name: "State",
                render: (a) => stateText[a.state] ?? a.state,
              },
              {
                id: "valid",
                name: "Valid until",
                render: (a) => new Date(a.notAfter).toLocaleDateString(),
              },
              {
                id: "retire",
                name: "Removed after",
                render: (a) =>
                  a.retireAfter ? new Date(a.retireAfter).toLocaleString() : "",
              },
            ]}
          />
          {bundle.rotationInProgress && (
            <section className="samuori-section" aria-labelledby="pending-h">
              <h2 id="pending-h">Waiting for workers</h2>
              <p className="samuori-muted">
                The new CA issues certificates only after every active worker
                has acknowledged the new trust bundle. Connected workers
                acknowledge it within a minute; offline workers must connect
                once.
              </p>
              <Table
                label="Workers that have not acknowledged the bundle"
                rows={bundle.pendingWorkers}
                empty="Every active worker has the new bundle. You can switch now."
                columns={[
                  {
                    id: "name",
                    name: "Worker",
                    isRowHeader: true,
                    render: (w) => w.name,
                  },
                  {
                    id: "version",
                    name: "Bundle it holds",
                    render: (w) => String(w.bundleVersion),
                  },
                ]}
              />
            </section>
          )}
        </>
      )}
      <RotationDialog
        mode={mode}
        onClose={() => setMode(undefined)}
        onDone={(b) => {
          setMode(undefined);
          setBundle(b);
        }}
      />
    </>
  );
}

function RotationDialog({
  mode,
  onClose,
  onDone,
}: {
  mode?: "planned" | "emergency";
  onClose: () => void;
  onDone: (b: Bundle) => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!mode) return;
    const { data, error } = await api.POST("/pki/rotations", {
      body: { mode, reason },
    });
    if (data) {
      setReason("");
      onDone(data);
    } else setError(describeError(error));
  }
  return (
    <Dialog
      title={mode === "emergency" ? "Emergency CA rotation" : "Rotate the CA"}
      isOpen={!!mode}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        {mode === "emergency" ? (
          <Alert tone="warning">
            Every CA is replaced at once and every worker identity is revoked.
            Running builds are requeued. Each worker must then be enrolled again
            with a new token and the new CA pin. Use this only when the CA key
            may be compromised.
          </Alert>
        ) : (
          <p>
            A new CA joins the trust bundle. Workers receive it automatically;
            once all have acknowledged it you switch issuance. The old CA stays
            trusted until every certificate it issued has expired.
          </p>
        )}
        <TextField
          label="Reason"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={10}
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant={mode === "emergency" ? "danger" : undefined}
          >
            {mode === "emergency" ? "Replace every CA" : "Add the new CA"}
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}
