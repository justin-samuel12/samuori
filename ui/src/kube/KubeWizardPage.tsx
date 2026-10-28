import { useCallback, useState, type ReactNode } from "react";
import { Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { RadioGroup, Select, TextField } from "../components/Field";
import { Alert, Loading, PageHeader } from "../components/Feedback";
import { correctName } from "../format";
import { useLoader } from "../hooks";
import { navigate } from "../router";

type Create = Schemas["KubeConversionCreate"];
type Options = Schemas["KubeConversionOptions"];
type InputKind = Schemas["KubeConversionInput"]["kind"];
type ProviderOption = Schemas["KubeProviderList"]["items"][number];

const providerText: Record<string, string> = {
  kompose: "converts Compose files and Samuori images",
  move2kube:
    "also reads source code and Dockerfiles; it answers its questions with defaults, which you review and answer before running again",
};

const steps = [
  "Input",
  "Provider & format",
  "Name & namespace",
  "Ports & exposure",
  "Replicas & resources",
  "Storage",
  "Secrets",
  "Review & generate",
];

const composeNames = [
  "compose.yml",
  "compose.yaml",
  "docker-compose.yml",
  "docker-compose.yaml",
];

function query(name: string): string | undefined {
  return new URLSearchParams(window.location.search).get(name) ?? undefined;
}

/** "Generate Kubernetes configuration": collects the input and options, then queues the conversion. */
export function KubeWizardPage({ projectId }: { projectId: string }) {
  const build = query("build");
  const publication = query("publication");
  const [step, setStep] = useState(0);
  const [kind, setKind] = useState<InputKind>(
    publication ? "publication" : build ? "build" : "compose",
  );
  const [snapshotId, setSnapshotId] = useState<string>();
  const [composePath, setComposePath] = useState("compose.yml");
  const [sourceDir, setSourceDir] = useState("");
  const [provider, setProvider] = useState<Create["provider"]>("kompose");
  const [providers, setProviders] = useState<ProviderOption[]>([]);
  const [buildRunId, setBuildRunId] = useState(build ?? "");
  const [publicationId, setPublicationId] = useState(publication ?? "");
  const [service, setService] = useState("");
  const [name, setName] = useState("");
  const [opts, setOpts] = useState<Options>({
    format: "manifests",
    replicas: 1,
    exposure: { type: "cluster-ip" },
    resources: {},
    storage: { volumes: "persistentVolumeClaim" },
  });
  const [snapshots, setSnapshots] = useState<Schemas["SourceSnapshot"][]>();
  const [images, setImages] = useState<Schemas["ImageSummary"][]>([]);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    const [s, i, p] = await Promise.all([
      api.GET("/projects/{projectId}/snapshots", {
        params: { path: { projectId }, query: { limit: 50 } },
      }),
      api.GET("/projects/{projectId}/images", {
        params: { path: { projectId }, query: { limit: 100 } },
      }),
      api.GET("/projects/{projectId}/kube-providers", {
        params: { path: { projectId } },
      }),
    ]);
    const snaps = s.data?.items ?? [];
    setSnapshots(snaps);
    setSnapshotId((cur) => cur ?? snaps[0]?.id);
    setImages(i.data?.items ?? []);
    setProviders(p.data?.items ?? []);
  }, [projectId]);
  useLoader(load);

  const set = (patch: Partial<Options>) => setOpts((o) => ({ ...o, ...patch }));
  const usesImage = kind === "build" || kind === "publication";
  const chosen = providers.find((p) => p.name === provider);
  const sourceOK = providers.some(
    (p) => p.available && p.inputs.includes("source"),
  );
  const providerName = chosen?.displayName ?? provider;
  function pickKind(v: InputKind) {
    setKind(v);
    if (v === "source") setProvider("move2kube");
  }

  async function generate() {
    setBusy(true);
    setError(undefined);
    const body: Create = {
      name,
      provider,
      input:
        kind === "compose"
          ? {
              kind,
              snapshotId,
              composePath,
              ...(buildRunId && service ? { buildRunId, service } : {}),
            }
          : kind === "source"
            ? {
                kind,
                snapshotId,
                ...(sourceDir ? { sourceDir } : {}),
                ...(buildRunId && service ? { buildRunId, service } : {}),
              }
            : kind === "build"
              ? { kind, buildRunId, service: service || undefined }
              : { kind, publicationId, service: service || undefined },
      options: opts,
    };
    const { data, error } = await api.POST(
      "/projects/{projectId}/kube-conversions",
      {
        params: {
          path: { projectId },
          header: { "Idempotency-Key": crypto.randomUUID() },
        },
        body,
      },
    );
    setBusy(false);
    if (data) navigate(`/kube-conversions/${data.id}`);
    else setError(describeError(error, "The conversion could not be queued."));
  }

  if (!snapshots) return <Loading />;
  const imageOptions = images.map((a) => ({
    id: a.buildRunId,
    label: `Build ${a.buildRunId.slice(-8)} — ${a.digest.slice(7, 19)}`,
  }));
  const pubOptions = images.flatMap((a) =>
    a.publications
      .filter((p) => p.state === "published")
      .map((p) => ({ id: p.id, label: p.destinationRef ?? p.id })),
  );
  return (
    <>
      <PageHeader
        title="Generate Kubernetes configuration"
        help="kubernetes"
        crumbs={[
          { label: "Projects", href: "/projects" },
          { label: "Project", href: `/projects/${projectId}` },
          { label: "Kubernetes", href: `/projects/${projectId}/kubernetes` },
        ]}
        description="Samuori converts your Compose file, source or image with an approved provider on a worker, with no network and no credentials, and checks the result. It never deploys anything: you review and download the files."
      />
      <ol className="samuori-stepper" aria-label="Steps">
        {steps.map((s, i) => (
          <li key={s}>
            <Button
              variant="quiet"
              onPress={() => setStep(i)}
              aria-current={i === step ? "step" : undefined}
            >
              {i + 1}. {s}
            </Button>
          </li>
        ))}
      </ol>
      <section className="samuori-section" aria-labelledby="kube-step-h">
        <h2 id="kube-step-h">
          Step {step + 1}: {steps[step]}
        </h2>
        <div className="samuori-form">
          {step === 0 && (
            <>
              <RadioGroup
                label="What to convert"
                value={kind}
                onChange={(v) => pickKind(v as InputKind)}
                options={[
                  {
                    id: "compose",
                    label: "A Compose file",
                    description:
                      "from one of this project's uploads; services, ports, environment and volumes are converted",
                  },
                  {
                    id: "source",
                    label: "Source code or a Dockerfile",
                    description: sourceOK
                      ? "a folder of an upload, analysed by Move2Kube without running anything in it"
                      : "needs Move2Kube, which an administrator has not enabled for this project",
                    isDisabled: !sourceOK,
                  },
                  {
                    id: "build",
                    label: "A built image",
                    description:
                      "one service from Samuori's records only; add the rest by hand",
                  },
                  {
                    id: "publication",
                    label: "A published image",
                    description: "as above, referenced by its registry digest",
                  },
                ]}
              />
              {(kind === "compose" || kind === "source") && (
                <>
                  {snapshots.length === 0 ? (
                    <Alert tone="info">
                      No uploads yet.{" "}
                      <Link
                        href={`/projects/${projectId}/upload`}
                        className="samuori-link"
                      >
                        Upload your project
                      </Link>{" "}
                      first.
                    </Alert>
                  ) : (
                    <Select
                      label="Source upload"
                      options={snapshots.map((s) => ({
                        id: s.id,
                        label: `${new Date(s.createdAt).toLocaleString()} — ${s.fileCount} files`,
                      }))}
                      value={snapshotId ?? null}
                      onChange={setSnapshotId}
                    />
                  )}
                  {kind === "compose" ? (
                    <TextField
                      label="Compose file"
                      value={composePath}
                      onChange={setComposePath}
                      isRequired
                      description={`Its path inside the upload, such as ${composeNames.join(", ")}.`}
                    />
                  ) : (
                    <TextField
                      label="Folder (optional)"
                      value={sourceDir}
                      onChange={setSourceDir}
                      description="A folder inside the upload, such as services/orders; the whole upload when empty."
                    />
                  )}
                  {imageOptions.length > 0 && (
                    <Select
                      label="Replace one service's image with a Samuori build (optional)"
                      options={[
                        { id: "", label: "Keep the Compose images" },
                        ...imageOptions,
                      ]}
                      value={buildRunId}
                      onChange={setBuildRunId}
                      description="The published digest of that build is used; an unpublished build is reported as an error to fix."
                    />
                  )}
                  {buildRunId && (
                    <TextField
                      label="Service to replace"
                      value={service}
                      onChange={(v) => setService(correctName(v))}
                      isRequired
                    />
                  )}
                </>
              )}
              {kind === "build" && (
                <Select
                  label="Built image"
                  options={imageOptions}
                  value={buildRunId || null}
                  onChange={setBuildRunId}
                />
              )}
              {kind === "publication" &&
                (pubOptions.length === 0 ? (
                  <Alert tone="info">
                    No image of this project is published yet.
                  </Alert>
                ) : (
                  <Select
                    label="Published image"
                    options={pubOptions}
                    value={publicationId || null}
                    onChange={setPublicationId}
                  />
                ))}
              {usesImage && (
                <TextField
                  label="Service name"
                  value={service}
                  onChange={(v) => setService(correctName(v))}
                  description="Names the Deployment and Service; app when empty."
                />
              )}
            </>
          )}
          {step === 1 && (
            <>
              <RadioGroup
                label="Provider"
                value={provider}
                onChange={(v) => setProvider(v as Create["provider"])}
                options={providers.map((p) => ({
                  id: p.name,
                  label: p.displayName,
                  description: !p.available
                    ? (p.reason ?? "not enabled")
                    : !p.inputs.includes(kind)
                      ? `cannot convert this input; it reads ${p.inputs.join(", ")}`
                      : (providerText[p.name] ?? ""),
                  isDisabled: !p.available || !p.inputs.includes(kind),
                }))}
              />
              {chosen?.maintenance && (
                <Alert tone="warning">
                  {chosen.displayName}: {chosen.maintenance} Review what it
                  generates with particular care.
                </Alert>
              )}
              {provider === "move2kube" && kind === "compose" && (
                <Alert tone="info">
                  Move2Kube reads only Compose files with{" "}
                  <code>version: &quot;3.8&quot;</code> (or 1, 2, 3) and no
                  top-level <code>name:</code>; Samuori refuses other Compose
                  files for it. Kompose reads both.
                </Alert>
              )}
              <RadioGroup
                label="Format"
                value={opts.format ?? "manifests"}
                onChange={(v) => set({ format: v as Options["format"] })}
                options={[
                  {
                    id: "manifests",
                    label: "Manifests",
                    description: "one YAML file per object",
                  },
                  {
                    id: "helm",
                    label: "Helm chart",
                    description: "the manifests as a chart's templates",
                  },
                  {
                    id: "kustomize",
                    label: "Kustomize",
                    description:
                      provider === "move2kube"
                        ? "Move2Kube's base and dev, staging and prod overlays"
                        : "the manifests with a kustomization.yml Samuori writes",
                  },
                ]}
              />
            </>
          )}
          {step === 2 && (
            <>
              <TextField
                label="Name"
                value={name}
                onChange={(v) => setName(correctName(v))}
                isRequired
                description="Names the conversion and, for Helm, the chart. Lowercase letters, digits and dashes."
              />
              <TextField
                label="Namespace"
                value={opts.namespace ?? ""}
                onChange={(v) =>
                  set({ namespace: correctName(v) || undefined })
                }
                description="Leave empty to use the namespace you apply into."
              />
            </>
          )}
          {step === 3 && (
            <>
              <p className="samuori-muted">
                Ports come from the Compose file: a service gets a Kubernetes
                Service only when it publishes ports.
              </p>
              <Select
                label="Expose a service"
                options={[
                  { id: "cluster-ip", label: "Inside the cluster only" },
                  { id: "node-port", label: "On every node's port" },
                  { id: "load-balancer", label: "Through a load balancer" },
                  { id: "ingress", label: "Through an Ingress" },
                ]}
                value={opts.exposure?.type ?? "cluster-ip"}
                onChange={(v) =>
                  set({
                    exposure: {
                      ...opts.exposure,
                      type: v as NonNullable<Options["exposure"]>["type"],
                    },
                  })
                }
              />
              {opts.exposure?.type !== "cluster-ip" && (
                <TextField
                  label="Service"
                  value={opts.exposure?.service ?? ""}
                  onChange={(v) =>
                    set({
                      exposure: { ...opts.exposure, service: correctName(v) },
                    })
                  }
                  isRequired
                />
              )}
              {opts.exposure?.type === "ingress" && (
                <>
                  <TextField
                    label="Host"
                    value={opts.exposure?.host ?? ""}
                    onChange={(v) =>
                      set({ exposure: { ...opts.exposure, host: v } })
                    }
                    isRequired
                    description="Such as shop.example.com. TLS and the ingress controller are yours to set up."
                  />
                  <TextField
                    label="Path"
                    value={opts.exposure?.path ?? "/"}
                    onChange={(v) =>
                      set({ exposure: { ...opts.exposure, path: v } })
                    }
                  />
                </>
              )}
            </>
          )}
          {step === 4 && (
            <>
              <TextField
                label="Replicas"
                value={String(opts.replicas ?? 1)}
                inputMode="numeric"
                onChange={(v) => set({ replicas: Number(v) || 1 })}
              />
              <p className="samuori-muted">
                Requests and limits apply only to containers the Compose file
                gives none; leave them empty to keep what it says.
              </p>
              {(
                [
                  ["cpuRequest", "CPU request", "such as 250m"],
                  ["memoryRequest", "Memory request", "such as 256Mi"],
                  ["cpuLimit", "CPU limit", "such as 1"],
                  ["memoryLimit", "Memory limit", "such as 512Mi"],
                ] as const
              ).map(([key, label, hint]) => (
                <TextField
                  key={key}
                  label={label}
                  value={opts.resources?.[key] ?? ""}
                  onChange={(v) =>
                    set({
                      resources: { ...opts.resources, [key]: v || undefined },
                    })
                  }
                  description={hint}
                />
              ))}
            </>
          )}
          {step === 5 && (
            <>
              <Select
                label="Volumes become"
                options={[
                  {
                    id: "persistentVolumeClaim",
                    label: "Persistent volume claims",
                  },
                  { id: "emptyDir", label: "Empty directories (data is lost)" },
                ]}
                value={opts.storage?.volumes ?? "persistentVolumeClaim"}
                onChange={(v) =>
                  set({
                    storage: {
                      ...opts.storage,
                      volumes: v as "persistentVolumeClaim" | "emptyDir",
                    },
                  })
                }
                description="Host paths are never offered: they expose the node's files."
              />
              <TextField
                label="Claim size"
                value={opts.storage?.size ?? ""}
                onChange={(v) =>
                  set({ storage: { ...opts.storage, size: v || undefined } })
                }
                description="Such as 1Gi; Kompose's default is 100Mi."
              />
              <TextField
                label="Storage class"
                value={opts.storage?.storageClass ?? ""}
                onChange={(v) =>
                  set({
                    storage: {
                      ...opts.storage,
                      storageClass: correctName(v) || undefined,
                    },
                  })
                }
              />
            </>
          )}
          {step === 6 && (
            <Alert tone="info">
              Environment values that look like passwords, tokens or keys never
              reach the generated files: Samuori replaces each with a reference
              to a Kubernetes Secret and lists them for you to create in the
              cluster. Values of Secret objects are removed the same way. You
              review them after generation, under Secrets & environment.
            </Alert>
          )}
          {step === 7 && (
            <Review
              rows={[
                ["Input", describeInput(kind, composePath, sourceDir, service)],
                ["Provider", providerName],
                ["Format", opts.format ?? "manifests"],
                ["Name", name || "(required)"],
                ["Namespace", opts.namespace ?? "(none)"],
                [
                  "Exposure",
                  `${opts.exposure?.type ?? "cluster-ip"} ${opts.exposure?.service ?? ""} ${opts.exposure?.host ?? ""}`,
                ],
                ["Replicas", String(opts.replicas ?? 1)],
                ["Volumes", opts.storage?.volumes ?? "persistentVolumeClaim"],
              ]}
            >
              {error && <Alert>{error}</Alert>}
              <div className="samuori-actions">
                <Button
                  onPress={() => void generate()}
                  isDisabled={
                    busy ||
                    !name ||
                    !chosen?.available ||
                    !chosen.inputs.includes(kind)
                  }
                >
                  Generate
                </Button>
              </div>
            </Review>
          )}
        </div>
        <div className="samuori-actions mt-4">
          {step > 0 && (
            <Button variant="secondary" onPress={() => setStep(step - 1)}>
              Back
            </Button>
          )}
          {step < steps.length - 1 && (
            <Button onPress={() => setStep(step + 1)}>Next</Button>
          )}
        </div>
      </section>
    </>
  );
}

function describeInput(
  kind: InputKind,
  path: string,
  dir: string,
  service: string,
) {
  if (kind === "compose") return `Compose file ${path}`;
  if (kind === "source") return `Source in ${dir || "the whole upload"}`;
  return `${kind === "build" ? "Built" : "Published"} image as service ${service || "app"}`;
}

function Review({
  rows,
  children,
}: {
  rows: [string, string][];
  children: ReactNode;
}) {
  return (
    <>
      <dl className="samuori-details">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt>{k}</dt>
            <dd>{v}</dd>
          </div>
        ))}
      </dl>
      {children}
    </>
  );
}
