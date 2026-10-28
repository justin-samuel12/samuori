import { useState } from "react";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { TextField } from "../components/Field";
import { Alert } from "../components/Feedback";
import { navigate } from "../router";
import { useCan } from "../session/session";
import type { Scope } from "../session/can";

type Project = Schemas["Project"];

export function isArchived(p: Project): boolean {
  return p.state === "archived" || p.state === "purged";
}

function when(at?: string, by?: string) {
  const date = at ? new Date(at).toLocaleString() : "";
  return by ? `${date} by ${by}` : date;
}

/** Archiving stops new work and revokes the project's grants (ADR 0058). */
export function ArchiveProjectButton({
  project,
  scope,
}: {
  project: Project;
  scope: Scope;
}) {
  const can = useCan();
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  if (isArchived(project) || !can("archiveProject", scope)) return null;

  async function archive() {
    setBusy(true);
    const { data, error } = await api.POST("/projects/{projectId}/archive", {
      params: { path: { projectId: project.id } },
      body: reason.trim() ? { reason: reason.trim() } : {},
    });
    setBusy(false);
    if (!data) {
      setError(describeError(error, "The project could not be archived."));
      return;
    }
    setOpen(false);
    navigate(
      "/projects",
      `${project.name} was archived. Tick "Show archived projects" to find it.`,
    );
  }

  return (
    <>
      <Button variant="secondary" onPress={() => setOpen(true)}>
        Archive project
      </Button>
      <Dialog
        title={`Archive ${project.name}?`}
        size="confirm"
        isOpen={open}
        onOpenChange={setOpen}
      >
        {error && <Alert>{error}</Alert>}
        <p className="mt-0">
          The project is hidden from project lists and accepts no new uploads,
          specs, builds, validations, publications or requests.
        </p>
        <ul>
          <li>Builds that are queued or running are cancelled.</li>
          <li>
            Its secret grants and bindings, egress allowances, destination
            access and pipelines are revoked, and pending requests are closed.
          </li>
          <li>
            Restoring the project later does <strong>not</strong> bring these
            grants back; an administrator must grant them again.
          </li>
          <li>
            Images, logs and history stay until an administrator purges it.
          </li>
        </ul>
        <TextField
          label="Reason (optional)"
          value={reason}
          onChange={setReason}
          maxLength={1024}
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => setOpen(false)}>
            Cancel
          </Button>
          <Button
            variant="danger"
            isDisabled={busy}
            onPress={() => void archive()}
          >
            Archive project
          </Button>
        </div>
      </Dialog>
    </>
  );
}

/** Shown on an archived or purged project, with Restore and, for administrators, Purge. */
export function ArchivedBanner({
  project,
  scope,
  onChange,
}: {
  project: Project;
  scope: Scope;
  onChange: (p: Project) => void;
}) {
  const can = useCan();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [purging, setPurging] = useState(false);
  if (!isArchived(project)) return null;

  if (project.state === "purged")
    return (
      <Alert tone="warning">
        <p className="mt-0">
          This project was purged {when(project.purgedAt, project.purgedBy)}.
          Its uploads, images, logs and reports were deleted from Samuori; its
          records and audit trail remain. It cannot be restored.
        </p>
        <p className="mb-0">
          Images already pushed to external registries were not deleted.
        </p>
        {can("purgeProject", scope) && <PurgeAction project={project} again />}
      </Alert>
    );

  async function restore() {
    setBusy(true);
    const { data, error } = await api.POST("/projects/{projectId}/restore", {
      params: { path: { projectId: project.id } },
    });
    setBusy(false);
    if (data) onChange(data);
    else setError(describeError(error, "The project could not be restored."));
  }

  return (
    <Alert tone="warning">
      <p className="mt-0">
        This project was archived {when(project.archivedAt, project.archivedBy)}
        . It accepts no new work, and its grants were revoked. Restoring it does
        not return them.
      </p>
      {error && <p className="text-bad">{error}</p>}
      <div className="samuori-actions">
        {can("restoreProject", scope) && (
          <Button
            variant="secondary"
            isDisabled={busy}
            onPress={() => void restore()}
          >
            Restore
          </Button>
        )}
        {can("purgeProject", scope) && (
          <Button variant="danger" onPress={() => setPurging(true)}>
            Purge
          </Button>
        )}
      </div>
      {purging && (
        <PurgeDialog project={project} onClose={() => setPurging(false)} />
      )}
    </Alert>
  );
}

function PurgeAction({
  project,
  again,
}: {
  project: Project;
  again?: boolean;
}) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <div className="samuori-actions">
        <Button variant="secondary" onPress={() => setOpen(true)}>
          {again ? "Purge again" : "Purge"}
        </Button>
      </div>
      {open && <PurgeDialog project={project} onClose={() => setOpen(false)} />}
    </>
  );
}

function PurgeDialog({
  project,
  onClose,
}: {
  project: Project;
  onClose: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<Schemas["ProjectPurge"]>();

  async function purge() {
    setBusy(true);
    const { data, error } = await api.POST("/projects/{projectId}/purge", {
      params: {
        path: { projectId: project.id },
        header: { "X-Samuori-Confirm": typed },
      },
    });
    setBusy(false);
    if (!data) {
      setError(describeError(error, "The project could not be purged."));
      return;
    }
    setResult(data);
  }

  function close() {
    if (result) {
      navigate("/projects", `${project.name} was purged.`);
      return;
    }
    onClose();
  }

  return (
    <Dialog
      title={`Purge ${project.name}?`}
      size="confirm"
      isOpen
      onOpenChange={(o) => !o && close()}
    >
      {result ? (
        <>
          <Alert tone="success">
            {result.removedBlobs} stored files were removed.{" "}
            {result.keptBlobs > 0 &&
              `${result.keptBlobs} were kept because another project or a hold still uses them.`}
          </Alert>
          <p>{result.externalCopies}</p>
        </>
      ) : (
        <>
          {error && <Alert>{error}</Alert>}
          <p className="mt-0">
            This deletes from Samuori everything the project stored: uploaded
            source, built images, build logs, scan reports and SBOMs, caches,
            discovery results and conversion output. It cannot be undone.
          </p>
          <p>
            The project's name, its records (IDs, digests and times) and the
            audit trail are kept, so published images stay traceable. Files
            another project still uses are kept.
          </p>
          <p>
            <strong>
              Images already pushed to external registries are not deleted.
            </strong>{" "}
            Remove them in each registry if they must go.
          </p>
          <TextField
            label={`Type ${project.name} to confirm`}
            value={typed}
            onChange={setTyped}
            autoComplete="off"
          />
        </>
      )}
      <div className="samuori-actions">
        <Button variant="secondary" onPress={close}>
          {result ? "Close" : "Cancel"}
        </Button>
        {!result && (
          <Button
            variant="danger"
            isDisabled={busy || typed !== project.name}
            onPress={() => void purge()}
          >
            Purge project
          </Button>
        )}
      </div>
    </Dialog>
  );
}
