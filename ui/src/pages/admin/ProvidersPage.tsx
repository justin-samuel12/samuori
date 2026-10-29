import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Checkbox, Select, TextField } from "../../components/Field";
import { Alert, Badge, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type List = Schemas["ProviderList"];
type Provider = Schemas["Provider"];
type Policy = Schemas["BuildMethodPolicy"];

const methodLabel: Record<string, string> = {
  recipe: "Samuori recipe",
  dockerfile: "Custom Dockerfile",
  railpack: "Railpack",
  buildpacks: "Paketo Buildpacks",
};

// ProvidersPage approves optional third-party build and conversion tools and sets the build method policy
// (ADR 0047, ADR 0048).
export function ProvidersPage() {
  const [list, setList] = useState<List>();
  const [pools, setPools] = useState<Schemas["WorkerPool"][]>([]);
  const [editing, setEditing] = useState<Provider>();
  const [message, setMessage] = useState<string>();
  const load = useCallback(async () => {
    const [l, p] = await Promise.all([
      api.GET("/providers"),
      api.GET("/worker-pools"),
    ]);
    setList(l.data);
    setPools(p.data?.items ?? []);
  }, []);
  useLoader(load);
  if (!list) return <Loading />;
  return (
    <>
      <PageHeader
        title="Build & conversion providers"
        description="Optional open-source tools Samuori runs on workers inside build isolation. Each stays off until you approve a version pinned by checksum or digest."
      />
      {message && <Alert tone="success">{message}</Alert>}
      <PolicyForm
        policy={list.policy}
        providers={list.items}
        onSaved={(l) => {
          setList(l);
          setMessage("Build method policy saved.");
        }}
      />
      <section className="samuori-section" aria-labelledby="providers-h">
        <h2 id="providers-h">Providers</h2>
        <Table
          label="Providers"
          rows={list.items.map((p) => ({ ...p, id: p.name }))}
          columns={[
            {
              id: "name",
              name: "Provider",
              isRowHeader: true,
              render: (p) => (
                <>
                  <strong>{p.displayName}</strong>
                  <div className="samuori-muted">
                    {p.kind === "build"
                      ? "Build method"
                      : "Kubernetes conversion"}{" "}
                    · {p.license} · {p.architectures.join(", ")}
                  </div>
                  {p.maintenance && (
                    <div className="samuori-muted">⚠ {p.maintenance}</div>
                  )}
                </>
              ),
            },
            {
              id: "state",
              name: "State",
              render: (p) =>
                p.config.enabled ? (
                  <Badge tone="ok">Enabled {p.config.version}</Badge>
                ) : (
                  <Badge tone="neutral">Off</Badge>
                ),
            },
            {
              id: "workers",
              name: "Workers",
              render: (p) =>
                p.workers.online === 0
                  ? "No online worker has it"
                  : `${p.workers.withApprovedVersion} of ${p.workers.online} online${p.workerVersion ? " can run it" : " with the approved version"}`,
            },
            {
              id: "actions",
              name: "Actions",
              render: (p) => (
                <Button variant="secondary" onPress={() => setEditing(p)}>
                  Configure {p.displayName}
                </Button>
              ),
            },
          ]}
        />
      </section>
      {editing && (
        <ProviderDialog
          provider={editing}
          pools={pools}
          onClose={() => setEditing(undefined)}
          onSaved={(l) => {
            setList(l);
            setEditing(undefined);
            setMessage(`${editing.displayName} saved.`);
          }}
        />
      )}
    </>
  );
}

function PolicyForm({
  policy,
  providers,
  onSaved,
}: {
  policy: Policy;
  providers: Provider[];
  onSaved: (l: List) => void;
}) {
  const [allowed, setAllowed] = useState<string[]>(policy.allowed);
  const [def, setDef] = useState<string>(policy.default || "automatic");
  const [error, setError] = useState<string>();
  const enabled = (m: string) =>
    providers.find((p) => p.method === m)?.config.enabled ?? true;
  async function submit(e: FormEvent) {
    e.preventDefault();
    const { data, error } = await api.PUT("/build-method-policy", {
      body: {
        allowed: allowed as Policy["allowed"],
        default: (def === "automatic" ? "" : def) as Policy["default"],
      },
    });
    if (error || !data) setError(describeError(error));
    else {
      setError(undefined);
      onSaved(data);
    }
  }
  return (
    <section className="samuori-section" aria-labelledby="policy-h">
      <h2 id="policy-h">Build methods</h2>
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        {Object.entries(methodLabel).map(([m, label]) => (
          <Checkbox
            key={m}
            isSelected={allowed.includes(m)}
            onChange={(on) =>
              setAllowed(on ? [...allowed, m] : allowed.filter((x) => x !== m))
            }
          >
            {label}
            {!enabled(m) && " (its provider is off)"}
          </Checkbox>
        ))}
        <Select
          label="Default for new BuildSpecs"
          options={[
            { id: "automatic", label: "Automatic: recommended from discovery" },
            ...allowed.map((m) => ({ id: m, label: methodLabel[m] ?? m })),
          ]}
          value={def}
          onChange={setDef}
          description="Existing BuildSpecs keep their method. Admission refuses a method that is not allowed."
        />
        <div className="samuori-actions samuori-actions--start">
          <Button
            type="submit"
            isDisabled={
              [...allowed].sort().join() ===
                [...policy.allowed].sort().join() &&
              def === (policy.default || "automatic")
            }
          >
            Save build methods
          </Button>
        </div>
      </Form>
    </section>
  );
}

function ProviderDialog({
  provider,
  pools,
  onClose,
  onSaved,
}: {
  provider: Provider;
  pools: Schemas["WorkerPool"][];
  onClose: () => void;
  onSaved: (l: List) => void;
}) {
  const c = provider.config;
  const [enabled, setEnabled] = useState(c.enabled);
  const [version, setVersion] = useState(c.version);
  const [artifacts, setArtifacts] = useState<Record<string, string>>(
    c.artifacts ?? {},
  );
  const [chosen, setChosen] = useState<string[]>(c.pools);
  const [presets, setPresets] = useState(c.egressPresets.join(", "));
  const [timeout, setTimeout] = useState(String(c.timeoutSeconds));
  const [runAsRoot, setRunAsRoot] = useState(c.runAsRoot ?? false);
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    const { data, error } = await api.PUT("/providers/{providerName}", {
      params: {
        path: {
          providerName: provider.name as
            "railpack" | "buildpacks" | "kompose" | "move2kube",
        },
      },
      body: {
        enabled,
        version: version.trim(),
        artifacts: Object.fromEntries(
          Object.entries(artifacts)
            .map(([k, v]) => [k, v.trim()])
            .filter(([, v]) => v),
        ),
        pools: chosen,
        projects: c.projects,
        egressPresets: presets
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
        timeoutSeconds: Number(timeout) || 0,
        ...(provider.name === "railpack" && { runAsRoot }),
      },
    });
    if (error || !data) setError(describeError(error));
    else onSaved(data);
  }
  return (
    <Dialog
      title={`Configure ${provider.displayName}`}
      size="wide"
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        {provider.maintenance && (
          <Alert tone="error">{provider.maintenance}</Alert>
        )}
        <p className="samuori-muted">
          Needs: {provider.prerequisites.join("; ")}.
        </p>
        <Checkbox isSelected={enabled} onChange={setEnabled}>
          Enabled
        </Checkbox>
        <TextField
          label="Approved version"
          value={version}
          onChange={setVersion}
          description="Such as v0.43.0. Workers must report this version to run it."
        />
        {provider.artifacts.map((a) => (
          <TextField
            key={a.key}
            label={a.key}
            value={artifacts[a.key] ?? ""}
            onChange={(v) => setArtifacts({ ...artifacts, [a.key]: v })}
            description={a.description}
          />
        ))}
        {pools.length > 0 && (
          <fieldset className="samuori-fieldset">
            <legend>Worker pools (none selected: every pool)</legend>
            {pools.map((p) => (
              <Checkbox
                key={p.id}
                isSelected={chosen.includes(p.id)}
                onChange={(on) =>
                  setChosen(
                    on ? [...chosen, p.id] : chosen.filter((x) => x !== p.id),
                  )
                }
              >
                {p.name}
              </Checkbox>
            ))}
          </fieldset>
        )}
        <TextField
          label="Egress presets"
          value={presets}
          onChange={setPresets}
          description="Comma-separated presets this provider may reach while planning or building, such as github. Empty: none."
        />
        <TextField
          label="Time limit in seconds"
          type="number"
          value={timeout}
          onChange={setTimeout}
          description="0 uses the default."
        />
        {provider.name === "railpack" && (
          <Checkbox isSelected={runAsRoot} onChange={setRunAsRoot}>
            Keep Railpack's root user (by default Samuori makes images run as
            10001:10001 and gives that user the app directories)
          </Checkbox>
        )}
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Save</Button>
        </div>
      </Form>
    </Dialog>
  );
}
