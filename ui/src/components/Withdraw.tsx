import { useState } from "react";
import { api, describeError } from "../api/client";
import { Button } from "./Button";
import { Dialog } from "./Dialog";
import { TextField } from "./Field";
import { Alert } from "./Feedback";

/**
 * Confirms withdrawing a pending request before anyone decides it (ADR 0062). Offer it only where the server says
 * the caller may (canWithdraw); the server still checks the call. Render it outside tables, keyed by the approval.
 */
export function WithdrawDialog({
  approvalId,
  what,
  onClose,
  onDone,
}: {
  approvalId?: string;
  what: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  async function withdraw(id: string) {
    setBusy(true);
    const { error } = await api.POST("/approvals/{approvalId}/withdrawal", {
      params: { path: { approvalId: id } },
      body: reason.trim() ? { reason: reason.trim() } : {},
    });
    setBusy(false);
    if (error) {
      setError(describeError(error, "The request could not be withdrawn."));
      return;
    }
    onDone();
  }
  return (
    <Dialog
      title={`Withdraw the ${what}?`}
      size="confirm"
      isOpen={approvalId !== undefined}
      onOpenChange={(o) => !o && onClose()}
    >
      {error && <Alert>{error}</Alert>}
      <p className="mt-0">
        Administrators will no longer see it as waiting. It stays in the history
        as withdrawn; send a new request if it is needed again.
      </p>
      <TextField
        label="Reason (optional)"
        value={reason}
        onChange={setReason}
        maxLength={2048}
      />
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          Keep it
        </Button>
        <Button
          variant="danger"
          isDisabled={busy || !approvalId}
          onPress={() => approvalId && void withdraw(approvalId)}
        >
          Withdraw request
        </Button>
      </div>
    </Dialog>
  );
}
