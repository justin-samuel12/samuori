import { useCallback, useState } from "react";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { TextField } from "../../components/Field";
import { Alert, Badge, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type Status = Schemas["DatabaseStatus"];
type ControllerCheck = Status["controllers"][number];

const tone = { verified: "ok", failed: "bad", pending: "warn" } as const;

export function DatabasePage() {
  const [status, setStatus] = useState<Status>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [testing, setTesting] = useState(false);
  const [updating, setUpdating] = useState(false);
  const load = useCallback(async () => {
    const { data, error } = await api.GET("/system/database");
    if (data) setStatus(data);
    else
      setError(describeError(error, "The database status could not be read."));
  }, []);
  useLoader(load);
  if (!status) return error ? <Alert>{error}</Alert> : <Loading />;

  async function test() {
    setTesting(true);
    setError(undefined);
    setNotice(undefined);
    const { data, error } = await api.POST("/system/database/test", {});
    setTesting(false);
    if (data)
      setNotice(
        `Signed in${data.user ? ` as ${data.user}` : ""} with a new connection in ${data.latencyMs} ms (${status?.engine === "sqlite" ? "SQLite" : "PostgreSQL"} ${data.serverVersion}).`,
      );
    else setError(describeError(error));
  }

  return (
    <>
      <PageHeader
        help="database"
        title="Database"
        description="The metadata database every controller shares. Samuori never changes the database password: change it at the database first, then enter it here so every controller verifies it and switches."
        actions={
          <>
            <Button
              variant="secondary"
              onPress={() => void test()}
              isDisabled={testing}
            >
              {testing ? "Testing…" : "Test connection"}
            </Button>
            {status.canUpdate && (
              <Button onPress={() => setUpdating(true)}>Update password</Button>
            )}
          </>
        }
      />
      {error && <Alert>{error}</Alert>}
      {notice && <Alert tone="success">{notice}</Alert>}
      <dl className="samuori-details">
        <dt>Engine</dt>
        <dd>
          {status.engine === "sqlite" ? "SQLite" : "PostgreSQL"}{" "}
          {status.serverVersion}
        </dd>
        {status.host && (
          <>
            <dt>Server</dt>
            <dd>
              <code>
                {status.host}:{status.port}/{status.database}
              </code>
            </dd>
            <dt>User</dt>
            <dd>
              <code>{status.user}</code>
            </dd>
            <dt>TLS</dt>
            <dd>
              <code>sslmode={status.tls}</code>
            </dd>
          </>
        )}
        <dt>Query time</dt>
        <dd>{status.latencyMs} ms through this controller</dd>
        <dt>Connections</dt>
        <dd>
          {status.pool.open} open ({status.pool.inUse} in use,{" "}
          {status.pool.idle} idle) of {status.pool.maxOpen}
        </dd>
        <dt>Password</dt>
        <dd>
          {status.revision
            ? `Revision ${status.revision.number}, entered ${new Date(status.revision.createdAt).toLocaleString()}: ${status.revision.reason}`
            : status.canUpdate
              ? "The one in each controller's configuration"
              : "SQLite has no password"}
        </dd>
      </dl>
      <h2>Controllers</h2>
      <Table<ControllerCheck>
        label="Controllers"
        rows={status.controllers}
        empty="No live controller."
        columns={[
          {
            id: "host",
            name: "Controller",
            isRowHeader: true,
            render: (c) => c.hostname,
          },
          {
            id: "status",
            name: status.revision ? "Newest password" : "Connection",
            render: (c) => (
              <Badge tone={tone[c.status]}>
                {status.revision ? c.status : "connected"}
              </Badge>
            ),
          },
          {
            id: "detail",
            name: "Detail",
            render: (c) =>
              c.detail ||
              (c.checkedAt ? new Date(c.checkedAt).toLocaleString() : ""),
          },
        ]}
      />
      {updating && (
        <UpdateDialog
          onClose={() => setUpdating(false)}
          onDone={(s) => {
            setUpdating(false);
            setStatus(s);
            setNotice(
              "The password works from this controller and is stored for the others; each verifies it within a few seconds. Reload to follow them.",
            );
          }}
        />
      )}
    </>
  );
}

function UpdateDialog({
  onClose,
  onDone,
}: {
  onClose: () => void;
  onDone: (s: Status) => void;
}) {
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  async function save() {
    setBusy(true);
    setError(undefined);
    const { data, error } = await api.POST("/system/database/password", {
      body: { password, reason },
    });
    setBusy(false);
    if (data) onDone(data);
    else setError(describeError(error));
  }
  return (
    <Dialog
      title="Update the database password"
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <p className="mt-0">
        First change the password at the database (for example in RDS). This
        controller signs in with it before anything is stored; if that fails,
        nothing changes.
      </p>
      {error && <Alert>{error}</Alert>}
      <TextField
        label="New password"
        type="password"
        value={password}
        onChange={setPassword}
        isRequired
        autoComplete="new-password"
      />
      <TextField
        label="Confirm password"
        type="password"
        value={confirm}
        onChange={setConfirm}
        isRequired
        autoComplete="new-password"
      />
      <TextField
        label="Reason"
        value={reason}
        onChange={setReason}
        isRequired
      />
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          Cancel
        </Button>
        <Button
          isDisabled={
            busy ||
            password === "" ||
            password !== confirm ||
            reason.trim().length < 3
          }
          onPress={() => void save()}
        >
          {busy ? "Verifying…" : "Verify and update"}
        </Button>
      </div>
    </Dialog>
  );
}
