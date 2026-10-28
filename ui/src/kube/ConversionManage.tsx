import { useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { TextField } from "../components/Field";
import { Alert } from "../components/Feedback";
import { correctName } from "../format";
import { navigate } from "../router";
import { useCan } from "../session/session";

type Conversion = Schemas["KubeConversion"];

/** Rename and Delete for a conversion; edits of its files stay new revisions. */
export function ConversionManage({
  conversion,
  busy,
  onChange,
}: {
  conversion: Conversion;
  busy: boolean;
  onChange: (c: Conversion) => void;
}) {
  const can = useCan();
  const [dialog, setDialog] = useState<"rename" | "delete">();
  const [name, setName] = useState(conversion.name);
  const [error, setError] = useState<string>();
  const open = (d: "rename" | "delete") => {
    setName(conversion.name);
    setError(undefined);
    setDialog(d);
  };
  async function rename(e: FormEvent) {
    e.preventDefault();
    const { data, error } = await api.PATCH(
      "/kube-conversions/{conversionId}",
      {
        params: { path: { conversionId: conversion.id } },
        body: { name: correctName(name, { done: true }) },
      },
    );
    if (!data) {
      setError(describeError(error, "The conversion could not be renamed."));
      return;
    }
    setDialog(undefined);
    onChange(data);
  }
  async function remove() {
    const { error } = await api.DELETE("/kube-conversions/{conversionId}", {
      params: { path: { conversionId: conversion.id } },
    });
    if (error) {
      setError(describeError(error, "The conversion could not be deleted."));
      return;
    }
    navigate(`/projects/${conversion.projectId}/kubernetes`);
  }
  return (
    <>
      {can("updateKubeConversion") && (
        <Button variant="secondary" onPress={() => open("rename")}>
          Rename
        </Button>
      )}
      {can("deleteKubeConversion") && (
        <Button
          variant="secondary"
          isDisabled={busy}
          onPress={() => open("delete")}
        >
          Delete
        </Button>
      )}
      <Dialog
        title="Rename conversion"
        isOpen={dialog === "rename"}
        onOpenChange={(o) => !o && setDialog(undefined)}
      >
        <Form onSubmit={rename} className="samuori-form">
          {error && <Alert>{error}</Alert>}
          <TextField
            label="Name"
            value={name}
            onChange={(v) => setName(correctName(v))}
            isRequired
            description="A DNS label. Stored revisions keep the names already in their files; the next run or edit uses the new name."
          />
          <div className="samuori-actions">
            <Button variant="secondary" onPress={() => setDialog(undefined)}>
              Cancel
            </Button>
            <Button type="submit">Rename</Button>
          </div>
        </Form>
      </Dialog>
      <Dialog
        title={`Delete ${conversion.name}?`}
        size="confirm"
        isOpen={dialog === "delete"}
        onOpenChange={(o) => !o && setDialog(undefined)}
      >
        {error && <Alert>{error}</Alert>}
        <p className="mt-0">
          Every run, revision and generated file of this conversion is deleted
          and cannot be downloaded again. The audit trail keeps a record of it.
          Its creator or a maintainer of the project may delete it, and not
          while a run is in progress.
        </p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => setDialog(undefined)}>
            Keep it
          </Button>
          <Button variant="danger" onPress={() => void remove()}>
            Delete conversion
          </Button>
        </div>
      </Dialog>
    </>
  );
}
