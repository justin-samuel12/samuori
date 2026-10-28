import { useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Checkbox, Select, TextField } from "../components/Field";
import { Alert } from "../components/Feedback";
import { Table } from "../components/Table";
import { api, describeError, type Schemas } from "../api/client";
import { get } from "../spec/model";
import { useEditor } from "./fields";
import { correctName } from "../format";

interface SecretDecl {
  id: string;
  required?: boolean;
  type?: string;
  mount?: { target: string };
}

const statusText: Record<string, string> = {
  ready: "Ready",
  missing: "Missing",
  unbound: "Not bound (optional)",
  "not-permitted": "Not permitted",
};

/** Logical secret references only; values, provider paths and credentials never appear here. */
export function SecretsPanel({
  readiness,
  mounted,
  projectId,
}: {
  readiness?: Schemas["SecretReadiness"][];
  mounted: { id: string; target?: string }[];
  projectId?: string;
}) {
  const [asking, setAsking] = useState<string>();
  const [asked, setAsked] = useState<string[]>([]);
  const [bindError, setBindError] = useState<string>();
  const queryClient = useQueryClient();
  const access = useQuery({
    queryKey: ["secret-grants", projectId],
    enabled: !!projectId,
    queryFn: async () => {
      const { data } = await api.GET("/projects/{projectId}/secret-grants", {
        params: { path: { projectId: projectId ?? "" } },
      });
      return data ?? { items: [], uses: [] };
    },
  });
  const grants = access.data?.items ?? [];
  const uses = access.data?.uses ?? [];
  // A key set offers each of its keys; the option ID is "<grant>#<key>".
  const keyOptions = grants.flatMap((g) =>
    g.fields?.length
      ? g.fields.map((f) => ({ id: `${g.id}#${f}`, label: `${g.name} › ${f}` }))
      : [{ id: g.id, label: g.name }],
  );
  const optionFor = (use?: Schemas["SecretGrantUse"]) =>
    use?.grantId
      ? use.field
        ? `${use.grantId}#${use.field}`
        : use.grantId
      : null;
  const external = (option: string | null) =>
    grants.find((g) => g.id === option?.split("#")[0])?.providerType !==
      "local" && !!option;
  async function post(logicalId: string, option: string, base64 = false) {
    if (!projectId) return undefined;
    const [grantId = "", field] = option.split("#");
    const { error } = await api.POST(
      "/projects/{projectId}/secret-grant-bindings",
      {
        params: {
          path: { projectId },
          header: { "Idempotency-Key": crypto.randomUUID() },
        },
        body: {
          logicalId,
          grantId,
          ...(field ? { field } : {}),
          ...(base64 ? { encoding: "base64" as const } : {}),
        },
      },
    );
    return error;
  }
  async function bind(logicalId: string, option: string, base64 = false) {
    setBindError(undefined);
    const error = await post(logicalId, option, base64);
    if (error) setBindError(describeError(error));
    await queryClient.invalidateQueries({
      queryKey: ["secret-grants", projectId],
    });
  }
  const { doc, edit } = useEditor();
  const secrets =
    (get(doc, ["spec", "secrets"]) as SecretDecl[] | undefined) ?? [];
  // Secret IDs that are unbound and named exactly like a key in a granted key set.
  const matches = secrets.flatMap((s) => {
    if (uses.some((u) => u.logicalId === s.id)) return [];
    const g = grants.find((x) => x.fields?.includes(s.id));
    return g ? [{ id: s.id, option: `${g.id}#${s.id}` }] : [];
  });
  async function bindMatching() {
    setBindError(undefined);
    for (const m of matches) {
      const error = await post(m.id, m.option);
      if (error) {
        setBindError(`${m.id}: ${describeError(error)}`);
        break;
      }
    }
    await queryClient.invalidateQueries({
      queryKey: ["secret-grants", projectId],
    });
  }
  const [id, setId] = useState("");
  const [required, setRequired] = useState(true);
  const finalId = correctName(id, { extra: "._", done: true });
  const valid =
    /^[a-z0-9][a-z0-9_.-]{0,62}$/.test(finalId) &&
    !secrets.some((s) => s.id === finalId);
  const undeclared = mounted.filter((m) => !secrets.some((s) => s.id === m.id));
  const rows = secrets.map((s) => ({
    ...s,
    rowId: s.id,
    status: readiness?.find((r) => r.id === s.id),
  }));
  return (
    <section className="samuori-section" aria-labelledby="secrets-h">
      <h3 id="secrets-h">Secrets the build needs</h3>
      <p className="samuori-muted">
        List secrets by name, then choose for each one a key an administrator
        granted to this project. A key set lists its key names only; you never
        see or enter values here.
      </p>
      {undeclared.length > 0 && (
        <div className="samuori-alert samuori-alert--warning" role="status">
          Your Dockerfile uses {undeclared.map((m) => m.id).join(", ")}.{" "}
          <Button
            variant="quiet"
            onPress={() =>
              edit(
                ["spec", "secrets"],
                [
                  ...secrets,
                  ...undeclared.map((m) => ({
                    id: m.id,
                    ...(m.target ? { mount: { target: m.target } } : {}),
                  })),
                ],
              )
            }
          >
            Add them
          </Button>
        </div>
      )}
      {bindError && <Alert>{bindError}</Alert>}
      {matches.length > 0 && (
        <div className="samuori-alert samuori-alert--info" role="status">
          <div className="samuori-alert-body">
            {matches.length} secret{matches.length === 1 ? "" : "s"} share a
            name with a granted key: {matches.map((m) => m.id).join(", ")}.{" "}
            <Button variant="quiet" onPress={() => void bindMatching()}>
              Use matching keys
            </Button>
          </div>
        </div>
      )}
      <Table
        label="Secrets"
        rows={rows.map((r) => ({ ...r, id: r.rowId }))}
        empty="No secrets. Most applications do not need any to build."
        dependencies={[keyOptions, uses, asked]}
        columns={[
          {
            id: "id",
            name: "Secret",
            isRowHeader: true,
            render: (s) => <code>{s.id}</code>,
          },
          {
            id: "req",
            name: "Required",
            render: (s) => (s.required === false ? "Optional" : "Required"),
          },
          {
            id: "type",
            name: "Type",
            render: (s) => (s.type === "binary" ? "Binary" : "Text"),
          },
          {
            id: "at",
            name: "Mounted at",
            render: (s) => (
              <code>{s.mount?.target ?? `/run/secrets/${s.id}`}</code>
            ),
          },
          {
            id: "key",
            name: "Key",
            render: (s) => {
              const use = uses.find((u) => u.logicalId === s.id);
              if (!projectId) return <span className="samuori-muted">—</span>;
              if (grants.length === 0)
                return use ? (
                  <span>Bound by an administrator</span>
                ) : (
                  <span className="samuori-muted">No keys granted</span>
                );
              const option = optionFor(use);
              const decoded = use?.encoding === "base64";
              return (
                <div className="min-w-48">
                  <Select
                    label={`Key for ${s.id}`}
                    hideLabel
                    placeholder={
                      use ? "Bound by an administrator" : "Choose a key"
                    }
                    options={keyOptions}
                    value={option}
                    onChange={(o) => void bind(s.id, o, decoded)}
                  />
                  {option && external(option) && (
                    <Checkbox
                      isSelected={decoded}
                      onChange={(on) => void bind(s.id, option, on)}
                    >
                      Stored as base64; decode before the build
                    </Checkbox>
                  )}
                </div>
              );
            },
          },
          {
            id: "status",
            name: "Status",
            render: (s) =>
              uses.some((u) => u.logicalId === s.id)
                ? "Ready"
                : s.status
                  ? `${statusText[s.status.status] ?? s.status.status}${s.status.reason ? ` — ${s.status.reason}` : ""}`
                  : "Not checked yet",
          },
          {
            id: "remove",
            name: "Actions",
            render: (s) => (
              <>
                {projectId &&
                  s.status?.status === "missing" &&
                  !uses.some((u) => u.logicalId === s.id) &&
                  (asked.includes(s.id) ? (
                    <span className="samuori-muted">Requested</span>
                  ) : (
                    <Button
                      variant="quiet"
                      aria-label={`Ask for a binding of ${s.id}`}
                      onPress={() => setAsking(s.id)}
                    >
                      Ask for binding
                    </Button>
                  ))}
                <Button
                  variant="quiet"
                  aria-label={`Remove secret ${s.id}`}
                  onPress={() =>
                    edit(
                      ["spec", "secrets"],
                      secrets.filter((x) => x.id !== s.id),
                    )
                  }
                >
                  Remove
                </Button>
              </>
            ),
          },
        ]}
      />
      <div className="samuori-inline-form">
        <TextField
          label="Secret name"
          value={id}
          onChange={(v) => setId(correctName(v, { extra: "._" }))}
          description="Saved in lowercase; dots, dashes and underscores are kept, for example npm-token."
        />
        <Checkbox isSelected={required} onChange={setRequired}>
          Required
        </Checkbox>
        <Button
          variant="secondary"
          isDisabled={!valid}
          onPress={() => {
            edit(
              ["spec", "secrets"],
              [
                ...secrets,
                required ? { id: finalId } : { id: finalId, required: false },
              ],
            );
            setId("");
          }}
        >
          Add secret
        </Button>
      </div>
      {projectId && (
        <RequestDialog
          projectId={projectId}
          logicalId={asking}
          onClose={() => setAsking(undefined)}
          onDone={(id) => {
            setAsking(undefined);
            setAsked([...asked, id]);
          }}
        />
      )}
    </section>
  );
}

function RequestDialog({
  projectId,
  logicalId,
  onClose,
  onDone,
}: {
  projectId: string;
  logicalId?: string;
  onClose: () => void;
  onDone: (id: string) => void;
}) {
  const [why, setWhy] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!logicalId) return;
    const { error } = await api.POST("/projects/{projectId}/secret-requests", {
      params: {
        path: { projectId },
        header: { "Idempotency-Key": crypto.randomUUID() },
      },
      body: { logicalId, justification: why },
    });
    if (error) setError(describeError(error));
    else {
      setWhy("");
      onDone(logicalId);
    }
  }
  return (
    <Dialog
      title={`Ask for ${logicalId ?? "a secret"}`}
      isOpen={!!logicalId}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          An administrator connects this name to a real secret for the project.
          You never see or enter the value.
        </p>
        <TextField
          label="Why the build needs it"
          value={why}
          onChange={setWhy}
          isRequired
          minLength={10}
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
