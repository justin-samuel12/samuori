import { useCallback, useState, type FormEvent } from "react";
import {
  FileTrigger,
  Form,
  Tab,
  TabList,
  TabPanel,
  Tabs,
} from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Checkbox, Select, TextField } from "../../components/Field";
import { Alert, Badge, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type LocalSecret = Schemas["LocalSecret"];
type Binding = Schemas["SecretBinding"];
type Grant = Schemas["SecretGrant"];

function base64(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}

type SecretProvider = Schemas["SecretProvider"];
type AllowedSecret = Schemas["AllowedSecret"];

const providerTypes: Record<string, string> = {
  openbao: "OpenBao",
  "vault-compat": "HashiCorp Vault (Community or Enterprise)",
  "aws-secrets-manager": "AWS Secrets Manager",
};

export function SecretsPage() {
  const [provider, setProvider] = useState<string | null>();
  const [externals, setExternals] = useState<SecretProvider[]>([]);
  const [allowed, setAllowed] = useState<AllowedSecret[]>([]);
  const [allowing, setAllowing] = useState(false);
  const [unallowing, setUnallowing] = useState<AllowedSecret>();
  const [relabeling, setRelabeling] = useState<AllowedSecret>();
  const [stored, setStored] = useState<LocalSecret[]>([]);
  const [storing, setStoring] = useState(false);
  const [versioning, setVersioning] = useState<LocalSecret>();
  const [deleting, setDeleting] = useState<LocalSecret>();
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<SecretProvider>();
  const [removing, setRemoving] = useState<SecretProvider>();
  const [message, setMessage] = useState<string>();

  const load = useCallback(async () => {
    const provs = (await api.GET("/secret-providers")).data?.items ?? [];
    const local = provs.find((p) => p.type === "local");
    setProvider(local?.id ?? null);
    setExternals(provs.filter((p) => p.type !== "local"));
    setAllowed((await api.GET("/allowed-secrets")).data?.items ?? []);
    if (local) {
      const list = await api.GET(
        "/secret-providers/{providerId}/local-secrets",
        { params: { path: { providerId: local.id } } },
      );
      setStored(list.data?.items ?? []);
    }
  }, []);
  useLoader(load);

  async function testProvider(id: string) {
    await api.POST("/secret-providers/{providerId}/test", {
      params: { path: { providerId: id } },
    });
    await load();
  }

  if (provider === undefined) return <Loading />;
  if (provider === null)
    return <Alert>The local secret store is not available.</Alert>;

  return (
    <>
      <PageHeader
        help="secrets-admin"
        title="Secrets"
        description="Values are encrypted at rest and never shown again. Builds get a value only while they hold a lease, and only for keys granted to their project."
      />
      {message && <Alert tone="success">{message}</Alert>}
      <Tabs>
        <TabList aria-label="Secrets" className="samuori-tablist">
          <Tab id="stored" className="samuori-tab">
            Stored secrets
          </Tab>
          <Tab id="external" className="samuori-tab">
            External stores
          </Tab>
          <Tab id="allowed" className="samuori-tab">
            Allowed secrets
          </Tab>
          <Tab id="access" className="samuori-tab">
            Project access
          </Tab>
        </TabList>
        <TabPanel id="stored">
          <div className="samuori-section-head">
            <p className="samuori-muted m-0">
              Kept in Samuori's own encrypted store.
            </p>
            <Button variant="secondary" onPress={() => setStoring(true)}>
              Store a secret
            </Button>
          </div>
          <Table
            label="Stored secrets"
            rows={stored}
            empty="No secrets stored yet. Choose Store a secret to add one."
            columns={[
              {
                id: "name",
                name: "Name",
                isRowHeader: true,
                sortValue: (s) => s.name,
                render: (s) => <span className="font-medium">{s.name}</span>,
              },
              { id: "type", name: "Type", render: (s) => s.type },
              {
                id: "use",
                name: "Use",
                render: (s) =>
                  !s.controllerOnly ? (
                    "Builds, when granted or bound"
                  ) : s.samuoriUses?.length ? (
                    `Kept by Samuori for ${s.samuoriUses.join(", ")}`
                  ) : (
                    <Badge tone="warn">No longer used by Samuori</Badge>
                  ),
              },
              {
                id: "version",
                name: "Version",
                render: (s) => String(s.version),
              },
              {
                id: "updated",
                name: "Updated",
                sortValue: (s) => s.updatedAt,
                render: (s) => new Date(s.updatedAt).toLocaleString(),
              },
              {
                id: "actions",
                name: "Actions",
                render: (s) => (
                  <span className="inline-flex gap-1">
                    <Button
                      variant="quiet"
                      aria-label={`New version of ${s.name}`}
                      onPress={() => setVersioning(s)}
                    >
                      New version
                    </Button>
                    <Button
                      variant="quiet-danger"
                      aria-label={`Delete ${s.name}`}
                      onPress={() => setDeleting(s)}
                    >
                      Delete
                    </Button>
                  </span>
                ),
              },
            ]}
          />
        </TabPanel>
        <TabPanel id="external">
          <div className="samuori-section-head">
            <p className="samuori-muted m-0 max-w-3xl">
              Values stay in the external store and are read only when a build
              holding a lease needs them. Store credentials are kept for the
              controller and never shown.
            </p>
            <Button variant="secondary" onPress={() => setAdding(true)}>
              Add external store
            </Button>
          </div>
          <Table
            label="External secret stores"
            rows={externals}
            empty="No external stores. Secrets come from the local encrypted store; choose Add external store to connect one."
            columns={[
              {
                id: "name",
                name: "Name",
                isRowHeader: true,
                render: (p) => <span className="font-medium">{p.name}</span>,
              },
              {
                id: "type",
                name: "Type",
                render: (p) => providerTypes[p.type] ?? p.type,
              },
              {
                id: "where",
                name: "Connection",
                render: (p) =>
                  [
                    p.address ?? p.region,
                    p.namespace && `namespace ${p.namespace}`,
                    p.authMethod,
                  ]
                    .filter(Boolean)
                    .join(", ") +
                  (p.longLivedCredential
                    ? " (long-lived key: prefer an identity-based method)"
                    : ""),
              },
              {
                id: "health",
                name: "Last test",
                render: (p) => (
                  <Badge
                    tone={
                      p.health.status === "ok"
                        ? "ok"
                        : p.health.status === "failed"
                          ? "bad"
                          : "neutral"
                    }
                  >
                    {p.health.status}
                    {p.health.detail ? `: ${p.health.detail}` : ""}
                  </Badge>
                ),
              },
              {
                id: "actions",
                name: "Actions",
                render: (p) => (
                  <div className="samuori-actions samuori-actions--start">
                    <Button
                      variant="quiet"
                      onPress={() => void testProvider(p.id)}
                    >
                      Test {p.name}
                    </Button>
                    <Button
                      variant="quiet"
                      aria-label={`View or edit ${p.name}`}
                      onPress={() => setEditing(p)}
                    >
                      View or edit
                    </Button>
                    <Button
                      variant="quiet-danger"
                      aria-label={`Delete ${p.name}`}
                      onPress={() => setRemoving(p)}
                    >
                      Delete
                    </Button>
                  </div>
                ),
              },
            ]}
          />
        </TabPanel>
        <TabPanel id="allowed">
          <div className="samuori-section-head">
            <p className="samuori-muted m-0 max-w-3xl">
              A store connection allows nothing by itself. List here the secrets
              of external stores that may be granted to projects: a whole secret
              (projects choose its keys) or one key. Samuori reads each once to
              check it exists and never keeps values.
            </p>
            <Button
              variant="secondary"
              isDisabled={externals.length === 0}
              onPress={() => setAllowing(true)}
            >
              Allow a secret
            </Button>
          </div>
          <Table
            label="Allowed secrets"
            rows={allowed}
            empty={
              externals.length === 0
                ? "Add an external store first."
                : "Nothing allowed yet. Projects can be granted only what is listed here."
            }
            columns={[
              {
                id: "label",
                name: "Secret",
                isRowHeader: true,
                render: (a) => (
                  <>
                    <span className="font-medium">{a.label}</span>
                    {a.note && <div className="samuori-muted">{a.note}</div>}
                  </>
                ),
              },
              {
                id: "where",
                name: "Store and reference",
                render: (a) => (
                  <>
                    {a.providerName}{" "}
                    <code>
                      {a.providerType === "aws-secrets-manager"
                        ? a.reference
                        : a.reference.replace(/^([^:#]+):/, "$1 › ")}
                    </code>
                  </>
                ),
              },
              {
                id: "keys",
                name: "Keys",
                render: (a) =>
                  a.fields.length ? a.fields.join(", ") : "One key",
              },
              {
                id: "actions",
                name: "Actions",
                render: (a) => (
                  <span className="inline-flex gap-1">
                    <Button
                      variant="quiet"
                      aria-label={`Edit ${a.label}`}
                      onPress={() => setRelabeling(a)}
                    >
                      Edit
                    </Button>
                    <Button
                      variant="quiet-danger"
                      aria-label={`Remove ${a.label} from the allowed secrets`}
                      onPress={() => setUnallowing(a)}
                    >
                      Remove
                    </Button>
                  </span>
                ),
              },
            ]}
          />
        </TabPanel>
        <TabPanel id="access">
          <ProjectAccess
            provider={provider}
            externals={externals}
            allowed={allowed}
            stored={stored}
            onMessage={setMessage}
          />
        </TabPanel>
      </Tabs>
      <StoreDialog
        isOpen={storing}
        provider={provider}
        onClose={() => setStoring(false)}
        onDone={(s) => {
          setStoring(false);
          setMessage(`Stored ${s.name} as version ${s.version}.`);
          void load();
        }}
      />
      <StoreDialog
        key={versioning?.id ?? "none"}
        isOpen={!!versioning}
        provider={provider}
        existing={versioning}
        onClose={() => setVersioning(undefined)}
        onDone={(s) => {
          setVersioning(undefined);
          setMessage(`Stored ${s.name} as version ${s.version}.`);
          void load();
        }}
      />
      <DeleteSecretDialog
        key={deleting?.id ?? "none-delete"}
        provider={provider}
        secret={deleting}
        onClose={() => setDeleting(undefined)}
        onDone={(s) => {
          setDeleting(undefined);
          setMessage(`Deleted ${s.name} and all its versions.`);
          void load();
        }}
      />
      <ProviderDialog
        isOpen={adding}
        onClose={() => setAdding(false)}
        onDone={(p) => {
          setAdding(false);
          setMessage(`Added ${p.name}. Test it before granting keys from it.`);
          void load();
        }}
      />
      {editing && (
        <ProviderDialog
          key={editing.id}
          existing={editing}
          isOpen
          onClose={() => setEditing(undefined)}
          onDone={(p) => {
            setEditing(undefined);
            setMessage(`Saved ${p.name}. Test it again before builds use it.`);
            void load();
          }}
        />
      )}
      {allowing && (
        <AllowDialog
          externals={externals}
          onClose={() => setAllowing(false)}
          onDone={(a) => {
            setAllowing(false);
            setMessage(
              `Allowed ${a.label}. Grant it to projects under Project access.`,
            );
            void load();
          }}
        />
      )}
      {relabeling && (
        <AllowedEditDialog
          entry={relabeling}
          onClose={() => setRelabeling(undefined)}
          onDone={(a) => {
            setRelabeling(undefined);
            setMessage(
              `Saved ${a.label}. Grants already made keep their own names.`,
            );
            void load();
          }}
        />
      )}
      {unallowing && (
        <Dialog
          title={`Remove ${unallowing.label}?`}
          size="confirm"
          isOpen
          onOpenChange={(o) => !o && setUnallowing(undefined)}
        >
          <p>
            No new grants of it can be made. Projects already granted it keep it
            until you revoke their grants under Project access.
          </p>
          <div className="samuori-actions">
            <Button
              variant="secondary"
              onPress={() => setUnallowing(undefined)}
            >
              Cancel
            </Button>
            <Button
              variant="danger"
              onPress={async () => {
                await api.DELETE("/allowed-secrets/{allowedSecretId}", {
                  params: { path: { allowedSecretId: unallowing.id } },
                });
                setMessage(
                  `Removed ${unallowing.label} from the allowed list.`,
                );
                setUnallowing(undefined);
                void load();
              }}
            >
              Remove
            </Button>
          </div>
        </Dialog>
      )}
      {removing && (
        <DeleteProviderDialog
          provider={removing}
          onClose={() => setRemoving(undefined)}
          onDone={() => {
            setMessage(`Deleted ${removing.name} and its stored credential.`);
            setRemoving(undefined);
            void load();
          }}
        />
      )}
    </>
  );
}

function ProjectAccess({
  provider,
  externals,
  allowed,
  stored,
  onMessage,
}: {
  provider: string;
  externals: SecretProvider[];
  allowed: AllowedSecret[];
  stored: LocalSecret[];
  onMessage: (m: string) => void;
}) {
  const [projects, setProjects] = useState<{ id: string; label: string }[]>([]);
  const [project, setProject] = useState<string | null>(null);
  const [grants, setGrants] = useState<Grant[]>([]);
  const [bindings, setBindings] = useState<Binding[]>([]);
  const [granting, setGranting] = useState(false);
  const [revoking, setRevoking] = useState<Grant>();
  const [editingGrant, setEditingGrant] = useState<Grant>();
  const [acting, setActing] = useState<{
    binding: Binding;
    action: "disable" | "rotate";
  }>();
  const [pools, setPools] = useState<{ id: string; name: string }[]>([]);

  const loadProjects = useCallback(async () => {
    const ps = await api.GET("/projects", {
      params: { query: { limit: 100 } },
    });
    setProjects(
      (ps.data?.items ?? []).map((p) => ({ id: p.id, label: p.name })),
    );
    const wp = await api.GET("/worker-pools");
    setPools((wp.data?.items ?? []).map((p) => ({ id: p.id, name: p.name })));
  }, []);
  useLoader(loadProjects);

  const load = useCallback(async () => {
    if (!project) return;
    const path = { params: { path: { projectId: project } } };
    const [g, b] = await Promise.all([
      api.GET("/projects/{projectId}/secret-grants", path),
      api.GET("/projects/{projectId}/secret-bindings", path),
    ]);
    setGrants(g.data?.items ?? []);
    setBindings((b.data?.items ?? []).filter((x) => x.status === "active"));
  }, [project]);
  useLoader(load);

  const grantName = (id?: string) => grants.find((g) => g.id === id)?.name;

  async function refresh(g: Grant) {
    if (!project) return;
    const { data, error } = await api.POST(
      "/projects/{projectId}/secret-grants/{grantId}/refresh",
      { params: { path: { projectId: project, grantId: g.id } } },
    );
    if (data)
      onMessage(
        `${g.name} has ${data.fields?.length ?? 0} keys. Bindings to removed keys are disabled.`,
      );
    else onMessage(describeError(error, "The keys could not be refreshed."));
    void load();
  }

  return (
    <>
      <p className="samuori-muted mt-0 max-w-3xl">
        Grant keys to a project. Its developers and maintainers then choose from
        these keys for each secret their build uses; they never see values or
        store paths. Revoking a key stops every build that uses it. Developers
        cannot disable a binding themselves: they choose another key or remove
        the secret from their spec, or ask you to disable it here.
      </p>
      <div className="max-w-md">
        <Select
          label="Project"
          options={projects}
          value={project}
          onChange={setProject}
        />
      </div>
      {project && (
        <>
          <div className="samuori-section-head mt-8">
            <h2 className="m-0">Granted keys</h2>
            <Button
              variant="secondary"
              onPress={() => setGranting(true)}
              isDisabled={stored.length === 0 && externals.length === 0}
            >
              Grant a key
            </Button>
          </div>
          <Table
            label="Granted keys"
            rows={grants}
            empty="No keys granted. Developers of this project cannot use secrets yet; choose Grant a key."
            columns={[
              {
                id: "name",
                name: "Key",
                isRowHeader: true,
                sortValue: (g) => g.name,
                render: (g) => (
                  <>
                    <span className="font-medium">{g.name}</span>
                    {g.fields?.length ? (
                      <div className="samuori-muted text-xs">
                        Key set: {g.fields.join(", ")}
                      </div>
                    ) : null}
                  </>
                ),
              },
              {
                id: "store",
                name: "Store",
                render: (g) => (
                  <>
                    {g.providerName}
                    {g.providerType !== "local" && g.providerReference && (
                      <div>
                        <code className="samuori-muted">
                          {g.providerReference}
                        </code>
                      </div>
                    )}
                  </>
                ),
              },
              {
                id: "usage",
                name: "Usage",
                render: (g) => (
                  <>
                    {g.usage.join(", ")}
                    <div className="samuori-muted text-xs">
                      {g.version && g.version !== "latest"
                        ? `Version ${g.version}`
                        : "Latest version"}
                      {", "}
                      {g.eligiblePoolIds?.length
                        ? g.eligiblePoolIds
                            .map(
                              (id) =>
                                pools.find((p) => p.id === id)?.name ?? id,
                            )
                            .join(", ")
                        : "any eligible pool"}
                    </div>
                  </>
                ),
              },
              {
                id: "used",
                name: "Used as",
                render: (g) => {
                  const ids = bindings
                    .filter((b) => b.grantId === g.id)
                    .map((b) => b.logicalId);
                  return ids.length ? (
                    <code>{ids.join(", ")}</code>
                  ) : (
                    <span className="samuori-muted">Not used yet</span>
                  );
                },
              },
              { id: "reason", name: "Reason", render: (g) => g.reason },
              {
                id: "actions",
                name: "Actions",
                render: (g) => (
                  <span className="inline-flex gap-1">
                    {g.fields?.length ? (
                      <Button
                        variant="quiet"
                        aria-label={`Refresh the keys of ${g.name}`}
                        onPress={() => void refresh(g)}
                      >
                        Refresh keys
                      </Button>
                    ) : null}
                    <Button
                      variant="quiet"
                      aria-label={`Edit ${g.name}`}
                      onPress={() => setEditingGrant(g)}
                    >
                      Edit
                    </Button>
                    <Button
                      variant="quiet-danger"
                      aria-label={`Revoke ${g.name}`}
                      onPress={() => setRevoking(g)}
                    >
                      Revoke
                    </Button>
                  </span>
                ),
              },
            ]}
            dependencies={[bindings]}
          />
          <h2>Secrets in use</h2>
          <Table
            label="Bindings"
            rows={bindings}
            empty="No secret of this project is bound."
            columns={[
              {
                id: "logical",
                name: "Secret ID in the spec",
                isRowHeader: true,
                sortValue: (b) => b.logicalId,
                render: (b) => <code>{b.logicalId}</code>,
              },
              {
                id: "key",
                name: "Key",
                render: (b) =>
                  `${
                    (grantName(b.grantId) &&
                      `${grantName(b.grantId)}${b.providerReference.includes("#") && grants.find((g) => g.id === b.grantId)?.fields?.length ? ` › ${b.providerReference.split("#").pop()}` : ""}`) ??
                    (b.grantId
                      ? "Revoked key"
                      : `${b.secretName ?? b.providerReference} (bound by an administrator)`)
                  }${b.encoding === "base64" ? ", decoded from base64" : ""}`,
              },
              {
                id: "version",
                name: "Version",
                render: (b) => String(b.version),
              },
              {
                id: "actions",
                name: "Actions",
                render: (b) => (
                  <span className="inline-flex gap-1">
                    <Button
                      variant="quiet"
                      onPress={() =>
                        setActing({ binding: b, action: "rotate" })
                      }
                      aria-label={`Rotate binding ${b.logicalId}`}
                    >
                      Rotate
                    </Button>
                    <Button
                      variant="quiet"
                      onPress={() =>
                        setActing({ binding: b, action: "disable" })
                      }
                      aria-label={`Disable binding ${b.logicalId}`}
                    >
                      Disable
                    </Button>
                  </span>
                ),
              },
            ]}
            dependencies={[grants]}
          />
          <GrantDialog
            isOpen={granting}
            provider={provider}
            externals={externals}
            allowed={allowed}
            project={project}
            secrets={stored}
            onClose={() => setGranting(false)}
            onDone={(g) => {
              setGranting(false);
              onMessage(`Granted ${g.name}.`);
              void load();
            }}
          />
          <RevokeDialog
            project={project}
            grant={revoking}
            onClose={() => setRevoking(undefined)}
            onDone={(g) => {
              setRevoking(undefined);
              onMessage(`Revoked ${g.name}. Builds can no longer receive it.`);
              void load();
            }}
          />
          {editingGrant && (
            <GrantEditDialog
              key={editingGrant.id}
              project={project}
              grant={editingGrant}
              pools={pools}
              bindings={bindings.filter((b) => b.grantId === editingGrant.id)}
              onClose={() => setEditingGrant(undefined)}
              onDone={(u) => {
                setEditingGrant(undefined);
                onMessage(
                  u.disabled.length
                    ? `Saved ${u.grant.name}. Disabled ${u.disabled.map((d) => d.logicalId).join(", ")}: the key no longer covers them.`
                    : `Saved ${u.grant.name}. Its bindings follow the change.`,
                );
                void load();
              }}
            />
          )}
          <BindingActionDialog
            key={acting ? `${acting.binding.id}-${acting.action}` : "none"}
            binding={acting?.binding}
            action={acting?.action ?? "disable"}
            onClose={() => setActing(undefined)}
            onDone={() => {
              const a = acting?.action;
              setActing(undefined);
              onMessage(
                a === "rotate"
                  ? "Binding rotated. Builds admitted before now can no longer receive it; new builds can."
                  : "Binding disabled. Running builds can no longer receive it.",
              );
              void load();
            }}
          />
        </>
      )}
    </>
  );
}

function StoreDialog({
  isOpen,
  provider,
  existing,
  onClose,
  onDone,
}: {
  isOpen: boolean;
  provider: string;
  /** Stores a new version of this secret: its name and type are fixed. */
  existing?: LocalSecret;
  onClose: () => void;
  onDone: (s: LocalSecret) => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [type, setType] = useState<"text" | "binary">(
    existing?.type === "binary" ? "binary" : "text",
  );
  const [value, setValue] = useState("");
  const [file, setFile] = useState<{ name: string; data: string }>();
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    const body =
      type === "binary"
        ? { name, type, value: file?.data ?? "" }
        : { name, type, value };
    const { data, error } = await api.POST(
      "/secret-providers/{providerId}/local-secrets",
      {
        params: {
          path: { providerId: provider },
          header: { "Idempotency-Key": crypto.randomUUID() },
        },
        body,
      },
    );
    setValue("");
    setFile(undefined);
    if (error || !data) setError(describeError(error));
    else {
      if (!existing) setName("");
      onDone(data);
    }
  }
  return (
    <Dialog
      title={existing ? `New version of ${existing.name}` : "Store a secret"}
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        {existing ? (
          <p className="m-0">
            This stores version {existing.version + 1} of{" "}
            <strong>{existing.name}</strong> ({existing.type}). Keys granted
            with version “latest” use it from the next build; keys pinned to a
            version keep theirs. Version {existing.version} is kept.
          </p>
        ) : (
          <>
            <TextField
              placeholder="npm_token"
              label="Name"
              value={name}
              onChange={setName}
              isRequired
              description="Storing under an existing name creates a new version."
            />
            <Select
              label="Type"
              options={[
                { id: "text", label: "Text" },
                { id: "binary", label: "Binary file" },
              ]}
              value={type}
              onChange={(v) => setType(v as "text" | "binary")}
            />
          </>
        )}
        {type === "text" ? (
          <TextField
            label="Value"
            type="password"
            value={value}
            onChange={setValue}
            isRequired
            description="The value cannot be viewed after you save it."
          />
        ) : (
          <div className="samuori-field">
            <FileTrigger
              onSelect={(files) => {
                const f = files?.[0];
                if (!f) return;
                void f.arrayBuffer().then((buf) =>
                  setFile({
                    name: f.name,
                    data: base64(new Uint8Array(buf)),
                  }),
                );
              }}
            >
              <Button variant="secondary">Choose file</Button>
            </FileTrigger>
            <span className="samuori-muted">
              {file ? file.name : "No file chosen"}
            </span>
          </div>
        )}
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit" isDisabled={type === "binary" && !file}>
            {existing ? "Store new version" : "Store"}
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

function DeleteSecretDialog({
  provider,
  secret,
  onClose,
  onDone,
}: {
  provider: string;
  secret?: LocalSecret;
  onClose: () => void;
  onDone: (s: LocalSecret) => void;
}) {
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  async function remove() {
    if (!secret) return;
    setBusy(true);
    const { error, response } = await api.DELETE(
      "/secret-providers/{providerId}/local-secrets/{secretId}",
      { params: { path: { providerId: provider, secretId: secret.id } } },
    );
    setBusy(false);
    if (response.status === 204) onDone(secret);
    else setError(describeError(error, "The secret could not be deleted."));
  }
  return (
    <Dialog
      title={`Delete ${secret?.name ?? ""}`}
      size="confirm"
      isOpen={!!secret}
      onOpenChange={(o) => !o && onClose()}
    >
      {error ? (
        <Alert tone="warning">{error}</Alert>
      ) : (
        <p className="mt-0">
          All {secret?.version} version{secret?.version === 1 ? "" : "s"} of{" "}
          <strong>{secret?.name}</strong> are destroyed. This cannot be undone.
          A secret that is granted to a project, bound, or still used by Samuori
          itself cannot be deleted.
          {secret?.controllerOnly && !secret.samuoriUses?.length
            ? " Samuori kept it for credentials that were deleted or replaced; nothing refers to it any more."
            : ""}
        </p>
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
            Delete
          </Button>
        )}
      </div>
    </Dialog>
  );
}

function pin(
  external: SecretProvider | undefined,
  version: string,
): Pick<
  Schemas["SecretBindingCreate"],
  "versionPolicy" | "pinnedVersion" | "pinnedVersionId"
> {
  const v = version.trim();
  if (!external || v === "" || v === "latest")
    return { versionPolicy: "latest" };
  return external.type === "aws-secrets-manager"
    ? { versionPolicy: "pinned", pinnedVersionId: v }
    : { versionPolicy: "pinned", pinnedVersion: Number(v) };
}

function GrantDialog({
  isOpen,
  provider,
  externals,
  allowed,
  project,
  secrets,
  onClose,
  onDone,
}: {
  isOpen: boolean;
  provider: string;
  externals: SecretProvider[];
  allowed: AllowedSecret[];
  project: string;
  secrets: LocalSecret[];
  onClose: () => void;
  onDone: (g: Grant) => void;
}) {
  const [name, setName] = useState("");
  const [source, setSource] = useState<string | null>(provider);
  const [secret, setSecret] = useState<string | null>(null);
  const [choice, setChoice] = useState<string | null>(null);
  const [version, setVersion] = useState("latest");
  const external = externals.find((p) => p.id === source);
  // A whole allowed secret, or one of its keys as "<id>#<key>".
  const choices = allowed
    .filter((a) => a.providerId === source)
    .flatMap((a) => [
      {
        id: a.id,
        label: a.fields.length ? `${a.label} (all keys)` : a.label,
      },
      ...a.fields.map((f) => ({
        id: `${a.id}#${f}`,
        label: `${a.label} › ${f}`,
      })),
    ]);
  const [build, setBuild] = useState(true);
  const [test, setTest] = useState(false);
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!source || (external ? !choice : !secret)) return;
    const [allowedId = "", key] = (choice ?? "").split("#");
    const entry = allowed.find((a) => a.id === allowedId);
    const target = !external
      ? { providerId: source, providerReference: secret ?? "" }
      : key && entry
        ? { providerId: source, providerReference: `${entry.reference}#${key}` }
        : { allowedSecretId: allowedId };
    const usage: ("build" | "test")[] = [];
    if (build) usage.push("build");
    if (test) usage.push("test");
    const { data, error } = await api.POST(
      "/projects/{projectId}/secret-grants",
      {
        params: {
          path: { projectId: project },
          header: { "Idempotency-Key": crypto.randomUUID() },
        },
        body: {
          ...(name ? { name } : {}),
          ...target,
          ...pin(external, version),
          usage,
          reason,
        },
      },
    );
    if (error || !data) setError(describeError(error));
    else {
      setName("");
      setChoice(null);
      setReason("");
      onDone(data);
    }
  }
  return (
    <Dialog
      title="Grant a key"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        {externals.length > 0 && (
          <Select
            label="Store"
            options={[
              { id: provider, label: "Local encrypted store" },
              ...externals.map((p) => ({ id: p.id, label: p.name })),
            ]}
            value={source}
            onChange={setSource}
          />
        )}
        {external ? (
          <>
            {choices.length === 0 ? (
              <Alert tone="info">
                Nothing from {external.name} is allowed yet. Add it under
                Allowed secrets first.
              </Alert>
            ) : (
              <Select
                label="Allowed secret"
                options={choices}
                value={choice}
                onChange={setChoice}
                isRequired
                description="All keys lets the project choose keys by name; or grant one key."
              />
            )}
            <TextField
              label="Version"
              value={version}
              onChange={setVersion}
              description="latest, or a version to pin."
            />
          </>
        ) : (
          <Select
            label="Stored secret"
            options={secrets
              .filter((s) => !s.controllerOnly)
              .map((s) => ({ id: s.id, label: s.name }))}
            value={secret}
            onChange={setSecret}
            isRequired
          />
        )}
        <TextField
          placeholder="npm_token"
          label="Name developers see"
          value={name}
          onChange={setName}
          description="Optional. Defaults to the stored secret's name or the allowed secret's label."
        />
        <Checkbox isSelected={build} onChange={setBuild}>
          Builds may use it
        </Checkbox>
        <Checkbox isSelected={test} onChange={setTest}>
          Runtime tests may use it
        </Checkbox>
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
            type="submit"
            isDisabled={(external ? !choice : !secret) || (!build && !test)}
          >
            Grant
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

function RevokeDialog({
  project,
  grant,
  onClose,
  onDone,
}: {
  project: string;
  grant?: Grant;
  onClose: () => void;
  onDone: (g: Grant) => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!grant) return;
    const { error } = await api.POST(
      "/projects/{projectId}/secret-grants/{grantId}/revoke",
      {
        params: {
          path: { projectId: project, grantId: grant.id },
          header: { "Idempotency-Key": crypto.randomUUID() },
        },
        body: { reason },
      },
    );
    if (error) setError(describeError(error));
    else {
      setReason("");
      onDone(grant);
    }
  }
  return (
    <Dialog
      title={`Revoke ${grant?.name ?? ""}`}
      isOpen={!!grant}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          Every binding made from this key is disabled now. Builds that need it
          are refused, including builds that are running.
        </p>
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
          <Button variant="danger" type="submit">
            Revoke
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

function BindingActionDialog({
  binding,
  action,
  onClose,
  onDone,
}: {
  binding?: Binding;
  action: "disable" | "rotate";
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  const rotate = action === "rotate";
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!binding) return;
    const { error } = await api.POST("/secret-bindings/{bindingId}/actions", {
      params: {
        path: { bindingId: binding.id },
        header: { "Idempotency-Key": crypto.randomUUID() },
      },
      body: { action, reason },
    });
    if (error) setError(describeError(error));
    else {
      setReason("");
      onDone();
    }
  }
  return (
    <Dialog
      title={`${rotate ? "Rotate" : "Disable"} ${binding?.logicalId ?? ""}`}
      isOpen={!!binding}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          {rotate
            ? "The binding gets a new version, for example after its value changed in the store. Builds admitted with the old version can no longer receive it, and caches keyed on it are not reused. New builds use it as before."
            : "Builds that need this secret will be refused, including builds that are running now."}
        </p>
        <TextField
          label="Reason"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={5}
          description="At least 5 characters; it is recorded in the audit log."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit" isDisabled={reason.trim().length < 5}>
            {rotate ? "Rotate" : "Disable"}
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

// covers mirrors secrets.Covers on the server: an empty pool list allows every pool.
function covers(
  usage: string[],
  pools: string[],
  innerUsage: string[],
  innerPools: string[],
) {
  if (!innerUsage.every((u) => usage.includes(u))) return false;
  if (pools.length === 0) return true;
  return innerPools.length > 0 && innerPools.every((p) => pools.includes(p));
}

function GrantEditDialog({
  project,
  grant,
  pools,
  bindings,
  onClose,
  onDone,
}: {
  project: string;
  grant: Grant;
  pools: { id: string; name: string }[];
  bindings: Binding[];
  onClose: () => void;
  onDone: (u: Schemas["SecretGrantUpdated"]) => void;
}) {
  const [name, setName] = useState(grant.name);
  const [build, setBuild] = useState(grant.usage.includes("build"));
  const [test, setTest] = useState(grant.usage.includes("test"));
  const [version, setVersion] = useState(grant.version ?? "latest");
  const [chosen, setChosen] = useState<string[]>(grant.eligiblePoolIds ?? []);
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  const usage: ("build" | "test")[] = [];
  if (build) usage.push("build");
  if (test) usage.push("test");
  const narrows = !covers(
    usage,
    chosen,
    grant.usage,
    grant.eligiblePoolIds ?? [],
  );
  const losing = bindings.filter(
    (b) => !covers(usage, chosen, b.usage, b.eligiblePoolIds ?? []),
  );
  const v = version.trim();
  const versionChanged = (v || "latest") !== (grant.version ?? "latest");
  async function submit(e: FormEvent) {
    e.preventDefault();
    const pinned =
      v === "" || v === "latest"
        ? { versionPolicy: "latest" as const }
        : grant.providerType === "aws-secrets-manager"
          ? { versionPolicy: "pinned" as const, pinnedVersionId: v }
          : { versionPolicy: "pinned" as const, pinnedVersion: Number(v) };
    const { data, error } = await api.PUT(
      "/projects/{projectId}/secret-grants/{grantId}",
      {
        params: { path: { projectId: project, grantId: grant.id } },
        body: { name, usage, eligiblePoolIds: chosen, reason, ...pinned },
      },
    );
    if (error || !data) setError(describeError(error));
    else onDone(data);
  }
  return (
    <Dialog
      title={`Edit ${grant.name}`}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p className="samuori-muted m-0">
          The key itself cannot change; to grant a different secret, grant it
          and revoke this one.
        </p>
        <TextField
          placeholder="npm_token"
          label="Name developers see"
          value={name}
          onChange={setName}
          isRequired
        />
        <Checkbox isSelected={build} onChange={setBuild}>
          Builds may use it
        </Checkbox>
        <Checkbox isSelected={test} onChange={setTest}>
          Runtime tests may use it
        </Checkbox>
        <TextField
          label="Version"
          value={version}
          onChange={setVersion}
          description="latest, or a version to pin."
        />
        {pools.length > 0 && (
          <fieldset className="samuori-fieldset">
            <legend>Worker pools</legend>
            <p className="samuori-muted m-0 text-xs">
              None selected allows every eligible pool.
            </p>
            {pools.map((p) => (
              <Checkbox
                key={p.id}
                isSelected={chosen.includes(p.id)}
                onChange={(on) =>
                  setChosen((c) =>
                    on ? [...c, p.id] : c.filter((x) => x !== p.id),
                  )
                }
              >
                {p.name}
              </Checkbox>
            ))}
          </fieldset>
        )}
        {narrows && losing.length > 0 && (
          <Alert tone="warning">
            This narrows the grant. Saving disables{" "}
            {losing.map((b) => b.logicalId).join(", ")} at once: builds that
            need {losing.length === 1 ? "it" : "them"} are refused, including
            running builds, until a developer binds another key.
          </Alert>
        )}
        {narrows && losing.length === 0 && (
          <Alert tone="info">
            This narrows the grant. No current binding is affected.
          </Alert>
        )}
        {versionChanged && bindings.length > 0 && (
          <Alert tone="info">
            Changing the version gives its bindings a new version: builds
            admitted before now can no longer receive the value.
          </Alert>
        )}
        <TextField
          label="Reason for the change"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={3}
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant={narrows && losing.length > 0 ? "danger" : "primary"}
            isDisabled={
              !name.trim() || usage.length === 0 || reason.trim().length < 3
            }
          >
            Save
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

function AllowedEditDialog({
  entry,
  onClose,
  onDone,
}: {
  entry: AllowedSecret;
  onClose: () => void;
  onDone: (a: AllowedSecret) => void;
}) {
  const [label, setLabel] = useState(entry.label);
  const [note, setNote] = useState(entry.note ?? "");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    const { data, error } = await api.PUT(
      "/allowed-secrets/{allowedSecretId}",
      {
        params: { path: { allowedSecretId: entry.id } },
        body: { label: label.trim(), note },
      },
    );
    if (error || !data) setError(describeError(error));
    else onDone(data);
  }
  return (
    <Dialog
      title={`Edit ${entry.label}`}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p className="samuori-muted m-0">
          The store and reference cannot change; remove the entry and allow the
          other secret instead.
        </p>
        <TextField label="Label" value={label} onChange={setLabel} isRequired />
        <TextField label="Note" value={note} onChange={setNote} />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit" isDisabled={!label.trim()}>
            Save
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

function AllowDialog({
  externals,
  onClose,
  onDone,
}: {
  externals: SecretProvider[];
  onClose: () => void;
  onDone: (a: AllowedSecret) => void;
}) {
  const [store, setStore] = useState<string | null>(externals[0]?.id ?? null);
  const [mount, setMount] = useState("secret");
  const [reference, setReference] = useState("");
  const [label, setLabel] = useState("");
  const [note, setNote] = useState("");
  const [error, setError] = useState<string>();
  const aws =
    externals.find((p) => p.id === store)?.type === "aws-secrets-manager";
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!store) return;
    const { data, error } = await api.POST("/allowed-secrets", {
      body: {
        providerId: store,
        reference: aws
          ? reference.trim()
          : `${mount.trim()}:${reference.trim()}`,
        ...(label ? { label } : {}),
        ...(note ? { note } : {}),
      },
    });
    if (error || !data) setError(describeError(error));
    else onDone(data);
  }
  return (
    <Dialog title="Allow a secret" isOpen onOpenChange={(o) => !o && onClose()}>
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <Select
          label="Store"
          options={externals.map((p) => ({ id: p.id, label: p.name }))}
          value={store}
          onChange={setStore}
        />
        {!aws && (
          <TextField
            placeholder="secret"
            label="KV v2 mount"
            value={mount}
            onChange={setMount}
            isRequired
            description="The secrets engine the secret lives in, such as secret or docker."
          />
        )}
        <TextField
          label={aws ? "Secret" : "Path"}
          value={reference}
          onChange={setReference}
          isRequired
          description={
            aws
              ? "A secret name or ARN, with #key for one field of a JSON secret."
              : "Such as operations/mssql-base for all its keys, or operations/mssql-base#TDE_CERT for one key."
          }
        />
        <TextField
          label="Label"
          value={label}
          onChange={setLabel}
          description="What administrators pick when granting; defaults to the reference."
        />
        <TextField label="Note" value={note} onChange={setNote} />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            isDisabled={!store || !reference.trim() || (!aws && !mount.trim())}
          >
            Allow
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

function DeleteProviderDialog({
  provider,
  onClose,
  onDone,
}: {
  provider: SecretProvider;
  onClose: () => void;
  onDone: () => void;
}) {
  const [error, setError] = useState<string>();
  async function remove() {
    const { error } = await api.DELETE("/secret-providers/{providerId}", {
      params: { path: { providerId: provider.id } },
    });
    if (error) setError(describeError(error));
    else onDone();
  }
  return (
    <Dialog
      title={`Delete ${provider.name}?`}
      size="confirm"
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      {error && <Alert>{error}</Alert>}
      <p>
        Samuori forgets this store and deletes the credential it keeps for it.
        The secrets in the store itself are not touched. A store that grants,
        bindings or signing keys ever used is kept for their history.
      </p>
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          Cancel
        </Button>
        <Button variant="danger" onPress={() => void remove()}>
          Delete
        </Button>
      </div>
    </Dialog>
  );
}

function ProviderDialog({
  existing,
  isOpen,
  onClose,
  onDone,
}: {
  existing?: SecretProvider;
  isOpen: boolean;
  onClose: () => void;
  onDone: (p: SecretProvider) => void;
}) {
  const e0 = existing;
  const [name, setName] = useState(e0?.name ?? "");
  const [type, setType] = useState<string | null>(e0?.type ?? "openbao");
  const [address, setAddress] = useState(e0?.address ?? "");
  const mount = e0?.mount ?? "";
  const [namespace, setNamespace] = useState(e0?.namespace ?? "");
  const [region, setRegion] = useState(e0?.region ?? "");
  const [auth, setAuth] = useState<string | null>(e0?.authMethod ?? "token");
  const [roleId, setRoleId] = useState(e0?.roleId ?? "");
  const [roleArn, setRoleArn] = useState(e0?.roleArn ?? "");
  const [externalId, setExternalId] = useState(e0?.externalId ?? "");
  const prefixes = (e0?.allowedPrefixes ?? []).join(",");
  const [credential, setCredential] = useState("");
  const [error, setError] = useState<string>();
  const aws = type === "aws-secrets-manager";
  const authOptions = aws
    ? [
        {
          id: "workload-identity",
          label: "The controller's workload identity",
        },
        { id: "assume-role", label: "Assume a role" },
        { id: "access-key", label: "Access key (long-lived; not recommended)" },
      ]
    : [
        { id: "token", label: "Token" },
        { id: "approle", label: "AppRole" },
      ];
  const needsCredential = !aws || auth === "access-key";
  const optionalCredential = auth === "approle";
  // A kept credential is never sent somewhere new; the server enforces the same rule.
  const redirected =
    !!e0 &&
    (address !== (e0.address ?? "") ||
      auth !== e0.authMethod ||
      roleId !== (e0.roleId ?? ""));
  const credentialRequired =
    needsCredential &&
    !optionalCredential &&
    (!e0?.hasCredential || redirected);
  async function submit(e: FormEvent) {
    e.preventDefault();
    const body: Schemas["SecretProviderUpdate"] = {
      name,
      authMethod: auth as Schemas["SecretProviderUpdate"]["authMethod"],
    };
    if (e0?.caCertPem) body.caCertPem = e0.caCertPem;
    if (e0?.endpoint) body.endpoint = e0.endpoint;
    if (aws) {
      body.region = region;
      if (auth === "assume-role") {
        body.roleArn = roleArn;
        if (externalId) body.externalId = externalId;
      }
    } else {
      body.address = address;
      if (mount) body.mount = mount;
      if (namespace) body.namespace = namespace;
      if (auth === "approle") body.roleId = roleId;
    }
    const list = prefixes
      .split(",")
      .map((p) => p.trim())
      .filter(Boolean);
    if (list.length) body.allowedPrefixes = list;
    if (needsCredential && credential) body.credential = credential;
    const { data, error } = e0
      ? await api.PUT("/secret-providers/{providerId}", {
          params: { path: { providerId: e0.id } },
          body,
        })
      : await api.POST("/secret-providers", {
          body: {
            ...body,
            type: type as Schemas["SecretProviderCreate"]["type"],
          },
        });
    if (error || !data) setError(describeError(error));
    else {
      setCredential("");
      onDone(data);
    }
  }
  return (
    <Dialog
      title={e0 ? `${e0.name}: view or edit` : "Add an external secret store"}
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          placeholder="team-vault"
          label="Name"
          value={name}
          onChange={setName}
          isRequired
        />
        <Select
          label="Type"
          isDisabled={!!e0}
          options={Object.entries(providerTypes).map(([id, label]) => ({
            id,
            label,
          }))}
          value={type}
          onChange={(v) => {
            setType(v);
            setAuth(
              v === "aws-secrets-manager" ? "workload-identity" : "token",
            );
          }}
        />
        {aws ? (
          <TextField
            placeholder="eu-west-1"
            label="Region"
            value={region}
            onChange={setRegion}
            isRequired
          />
        ) : (
          <>
            <TextField
              placeholder="https://vault.example.com:8200"
              label="Address"
              value={address}
              onChange={setAddress}
              isRequired
              description="https://bao.example:8200"
            />
            <TextField
              label="Namespace"
              value={namespace}
              onChange={setNamespace}
              description="Optional; Vault Enterprise and OpenBao namespaces only."
            />
          </>
        )}
        <Select
          label="Authentication"
          options={authOptions}
          value={auth}
          onChange={setAuth}
        />
        {auth === "approle" && (
          <TextField
            label="Role ID"
            value={roleId}
            onChange={setRoleId}
            isRequired
          />
        )}
        {auth === "assume-role" && (
          <>
            <TextField
              placeholder="arn:aws:iam::123456789012:role/samuori-secrets"
              label="Role ARN"
              value={roleArn}
              onChange={setRoleArn}
              isRequired
            />
            <TextField
              label="External ID"
              value={externalId}
              onChange={setExternalId}
            />
          </>
        )}
        {needsCredential && (
          <TextField
            label={
              auth === "approle"
                ? "Secret ID"
                : auth === "access-key"
                  ? "Access key"
                  : "Token"
            }
            type="password"
            value={credential}
            onChange={setCredential}
            isRequired={credentialRequired}
            description={
              optionalCredential && !e0?.hasCredential
                ? "Optional: leave empty for a role created with bind_secret_id=false and bound to the controllers' addresses (token_bound_cidrs)."
                : e0?.hasCredential && !redirected
                  ? "Leave empty to keep the stored one; it is never shown."
                  : e0?.hasCredential
                    ? "Enter it again: Samuori never sends a stored credential to a new address, sign-in method or role."
                    : auth === "access-key"
                      ? "ACCESS_KEY_ID:SECRET_ACCESS_KEY"
                      : "Stored for the controller only and never shown again."
            }
          />
        )}
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">{e0 ? "Save" : "Add"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}
