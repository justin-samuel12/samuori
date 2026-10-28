import { useState } from "react";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "./Button";
import { Dialog } from "./Dialog";
import { Alert } from "./Feedback";

type Snapshot = Schemas["SourceSnapshot"];

/** Confirms and deletes an upload; the server allows it only for the person who uploaded it. */
export function DeleteUploadDialog({
  snapshot,
  onClose,
  onDone,
}: {
  snapshot?: Snapshot;
  onClose: () => void;
  onDone: () => void;
}) {
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  async function remove() {
    if (!snapshot) return;
    setBusy(true);
    const { error, response } = await api.DELETE("/snapshots/{snapshotId}", {
      params: { path: { snapshotId: snapshot.id } },
    });
    setBusy(false);
    if (response.status === 204) onDone();
    else setError(describeError(error, "The upload could not be deleted."));
  }
  return (
    <Dialog
      title="Delete this upload?"
      size="confirm"
      isOpen={!!snapshot}
      onOpenChange={(o) => !o && onClose()}
    >
      {error ? (
        <Alert>{error}</Alert>
      ) : (
        <>
          <p className="mt-0">
            Its {snapshot?.fileCount} files are removed from Samuori at once.
            This cannot be undone.
          </p>
          <p>
            Builds made from it stay in the history, but{" "}
            <strong>Build again</strong> no longer works for them. Images
            already built and published are not affected.
          </p>
        </>
      )}
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          {error ? "Close" : "Cancel"}
        </Button>
        {!error && (
          <Button
            variant="danger"
            isDisabled={busy}
            onPress={() => void remove()}
          >
            Delete upload
          </Button>
        )}
      </div>
    </Dialog>
  );
}

export function isUploader(s: Snapshot, principalId: string): boolean {
  return s.createdBy?.id === principalId;
}
