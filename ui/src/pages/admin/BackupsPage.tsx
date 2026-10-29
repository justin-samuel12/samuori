import { useCallback, useState } from "react";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type BackupList = Schemas["BackupList"];
type Backup = Schemas["Backup"];

function size(bytes: number): string {
  if (bytes < 1 << 20) return `${Math.max(1, Math.round(bytes / 1024))} KiB`;
  return `${(bytes / (1 << 20)).toFixed(1)} MiB`;
}

export function BackupsPage() {
  const [list, setList] = useState<BackupList>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/system/backups");
    setList(data);
  }, []);
  useLoader(load);
  if (!list) return <Loading />;

  async function backUp() {
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    const { data, error } = await api.POST("/system/backups", {});
    setBusy(false);
    if (error) setError(describeError(error));
    else if (data) {
      setNotice(
        `Backup written: ${data.tables} tables, ${data.rows} rows, ${data.blobs} blobs, ${size(data.size)}.`,
      );
      void load();
    }
  }

  async function check(b: Backup) {
    setError(undefined);
    const { data, error } = await api.POST("/system/backups/{backupId}/check", {
      params: { path: { backupId: b.id } },
    });
    if (error) setError(describeError(error));
    else if (data) {
      setNotice(
        data.checkResult === "intact"
          ? "The archive is intact."
          : `The archive is ${data.checkResult}.`,
      );
      void load();
    }
  }

  return (
    <>
      <PageHeader
        help="backups"
        title="Backups"
        description="Encrypted backups of the database, the controller's keys and CA, and blobs kept on this controller. Restoring needs the age identity, which Samuori never stores."
      />
      {!list.enabled ? (
        <Alert tone="info">
          Backups are not configured. Set backup.dir and backup.recipientsFile
          (age public keys) and restart the controller.
        </Alert>
      ) : (
        <>
          <dl className="samuori-details">
            <dt>Directory</dt>
            <dd>
              <code>{list.directory}</code>
            </dd>
            <dt>Schedule</dt>
            <dd>
              {list.intervalHours
                ? `Every ${list.intervalHours} hours`
                : "Only when started here or with samuori-server backup"}
            </dd>
            <dt>Kept</dt>
            <dd>The newest {list.keep}</dd>
          </dl>
          {error && <Alert>{error}</Alert>}
          {notice && <Alert tone="success">{notice}</Alert>}
          <Button onPress={backUp} isDisabled={busy}>
            {busy ? "Backing up…" : "Back up now"}
          </Button>
        </>
      )}
      <Table
        label="Backups"
        rows={list.items}
        empty="No backup has been written yet."
        columns={[
          {
            id: "created",
            name: "Written",
            isRowHeader: true,
            render: (b) => new Date(b.createdAt).toLocaleString(),
          },
          {
            id: "contents",
            name: "Contents",
            render: (b) =>
              `${b.dialect}, ${b.tables} tables, ${b.rows} rows, ${b.blobs} blobs${b.includesKeys ? ", keys" : ""}`,
          },
          {
            id: "size",
            name: "Size",
            align: "end",
            render: (b) => size(b.size),
          },
          {
            id: "trigger",
            name: "Started by",
            render: (b) =>
              b.trigger === "manual"
                ? "An administrator"
                : b.trigger === "command"
                  ? "The backup command"
                  : "The schedule",
          },
          {
            id: "check",
            name: "Integrity",
            render: (b) =>
              b.removed
                ? "Pruned"
                : b.checkResult
                  ? `${b.checkResult}, ${new Date(b.checkedAt ?? "").toLocaleString()}`
                  : "Not checked",
          },
          {
            id: "actions",
            name: "Actions",
            render: (b) =>
              !b.removed && (
                <Button
                  variant="secondary"
                  onPress={() => check(b)}
                  aria-label={`Check the backup of ${new Date(b.createdAt).toLocaleString()}`}
                >
                  Check
                </Button>
              ),
          },
        ]}
      />
      <h2>Restore</h2>
      <p>
        Restore on the host with the age identity, into an empty data directory
        and an empty database:{" "}
        <code>
          samuori-server restore --identity-file KEY --data-dir DIR [--database
          DSN] ARCHIVE
        </code>
        . The restore checks every file and table against the checksums taken
        when the backup was made before any migration runs. Verify an archive
        without restoring it with <code>samuori-server verify-backup</code>.
      </p>
    </>
  );
}
