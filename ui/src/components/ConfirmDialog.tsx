import { useState, type ReactNode } from "react";
import { Button } from "./Button";
import { Dialog } from "./Dialog";
import { Alert } from "./Feedback";

/** Asks before a destructive action; when the server refuses, it shows the reason and refusedHint. */
export function ConfirmDialog({
  title,
  isOpen,
  onClose,
  onConfirm,
  confirmLabel = "Delete",
  refusedHint,
  children,
}: {
  title: string;
  isOpen: boolean;
  onClose: () => void;
  /** Resolves to an error message, or undefined when the action succeeded. */
  onConfirm: () => Promise<string | undefined>;
  confirmLabel?: string;
  refusedHint?: ReactNode;
  children: ReactNode;
}) {
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const close = () => {
    setError(undefined);
    onClose();
  };
  async function confirm() {
    setBusy(true);
    const e = await onConfirm();
    setBusy(false);
    setError(e);
  }
  return (
    <Dialog
      title={title}
      size="confirm"
      isOpen={isOpen}
      onOpenChange={(o) => !o && close()}
    >
      <div className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>{children}</p>
        <div className="samuori-actions">
          {error && refusedHint}
          <Button variant="secondary" onPress={close}>
            Cancel
          </Button>
          <Button
            variant="danger"
            onPress={() => void confirm()}
            isDisabled={!!error || busy}
          >
            {confirmLabel}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}

export interface ConfirmRequest {
  title: string;
  message: ReactNode;
  confirmLabel: string;
  /** Resolves to an error message, or undefined when the action succeeded. */
  run: () => Promise<string | undefined>;
}

/** One confirmation dialog for a page's row actions: ask() opens it for the named item. */
export function useConfirm(): [ReactNode, (r: ConfirmRequest) => void] {
  const [request, setRequest] = useState<ConfirmRequest>();
  const dialog = (
    <ConfirmDialog
      title={request?.title ?? ""}
      isOpen={!!request}
      confirmLabel={request?.confirmLabel}
      onClose={() => setRequest(undefined)}
      onConfirm={async () => {
        const error = await request?.run();
        if (!error) setRequest(undefined);
        return error;
      }}
    >
      {request?.message}
    </ConfirmDialog>
  );
  return [dialog, setRequest];
}
