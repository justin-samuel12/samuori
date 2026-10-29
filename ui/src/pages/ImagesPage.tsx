import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Form, Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button, buttonClass } from "../components/Button";
import { DeploymentFiles } from "../components/DeploymentFiles";
import { Dialog } from "../components/Dialog";
import { Checkbox, Select, TextField } from "../components/Field";
import {
  Alert,
  Badge,
  EmptyState,
  Loading,
  PageHeader,
} from "../components/Feedback";
import { Table } from "../components/Table";
import { shortDigest as short } from "../format";
import { useLoader, useProject, useSpec } from "../hooks";
import { GenerateButton } from "../kube/KubeConversionPage";
import { useCan, useCanDownload } from "../session/session";
import { checkOutcome, imageName, publicationState } from "./display";

type Image = Schemas["ImageSummary"];
type Publication = Schemas["Publication"];

const finishedPublication = new Set([
  "published",
  "blocked",
  "failed",
  "rejected",
  "expired",
  "cancelled",
  "retired",
]);

export function Verdict({
  v,
}: {
  v?: { state: string; verdict?: string | null } | null;
}) {
  if (!v) return <span className="samuori-muted">Not validated</span>;
  if (!v.verdict)
    return v.state === "completed" ? (
      <Badge>Done</Badge>
    ) : (
      <Badge tone="info" live>
        Checking
      </Badge>
    );
  const o = checkOutcome(v.verdict);
  return <Badge tone={o.tone}>{o.text}</Badge>;
}

export function ImagesPage() {
  const can = useCan();
  const canDownload = useCanDownload();
  const [projects, setProjects] = useState<{ id: string; label: string }[]>();
  const [project, setProject] = useState<string | null>(null);
  const [images, setImages] = useState<Image[]>();
  const loadProjects = useCallback(async () => {
    const { data } = await api.GET("/projects", {
      params: { query: { limit: 100 } },
    });
    const list = (data?.items ?? []).map((p) => ({ id: p.id, label: p.name }));
    setProjects(list);
    if (list.length === 1 && list[0]) setProject(list[0].id);
  }, []);
  useLoader(loadProjects);
  const loadImages = useCallback(async () => {
    if (!project) return;
    const { data } = await api.GET("/projects/{projectId}/images", {
      params: { path: { projectId: project } },
    });
    setImages(data?.items ?? []);
  }, [project]);
  useLoader(loadImages);
  if (!projects) return <Loading />;
  const projectName = projects.find((p) => p.id === project)?.label;
  return (
    <>
      <PageHeader
        help="publishing"
        title="Images"
        description="Images your builds produced, with their validation and where they are published."
      />
      {projects.length === 0 ? (
        <EmptyState title="No projects yet">
          Ask an administrator to add you to a team with a project.
        </EmptyState>
      ) : (
        <div className="samuori-toolbar">
          <Select
            label="Project"
            options={projects}
            value={project}
            onChange={setProject}
            placeholder="Choose a project"
          />
        </div>
      )}
      {project && images && (
        <Table
          label="Images"
          rows={images.map((i) => ({ ...i, id: i.artifactId }))}
          empty="No image has been built for this project yet."
          columns={[
            {
              id: "image",
              name: "Image",
              isRowHeader: true,
              render: (i) => (
                <span className="flex flex-col">
                  <Link
                    href={`/images/${i.artifactId}`}
                    className="samuori-link font-medium"
                  >
                    {imageName({
                      name: i.app ?? projectName,
                      version: i.version,
                      digest: i.digest,
                    })}
                  </Link>
                  <code className="text-xs text-muted">{short(i.digest)}</code>
                </span>
              ),
              sortValue: (i) => i.version ?? "",
            },
            {
              id: "built",
              name: "Built",
              render: (i) => new Date(i.createdAt).toLocaleString(),
            },
            {
              id: "validation",
              name: "Validation",
              render: (i) => <Verdict v={i.validation} />,
            },
            {
              id: "published",
              name: "Published to",
              render: (i) =>
                i.publications
                  .filter((p) => p.state === "published")
                  .map((p) => p.destinationName ?? p.destinationId)
                  .join(", ") || "—",
            },
            {
              id: "actions",
              name: "Actions",
              render: (i) => (
                <div className="flex flex-wrap gap-2">
                  {canDownload && i.validation?.verdict === "passed" && (
                    <a
                      className={buttonClass("quiet")}
                      href={`/api/v1/artifacts/${i.artifactId}/download`}
                      download
                      aria-label={`Download ${imageName({ name: i.app ?? projectName, version: i.version, digest: i.digest })}`}
                    >
                      Download
                    </a>
                  )}
                  {can("getDeploymentFiles") && (
                    <DeploymentFiles
                      artifactId={i.artifactId}
                      variant="quiet"
                    />
                  )}
                </div>
              ),
            },
          ]}
        />
      )}
    </>
  );
}

export function ImagePage({ id }: { id: string }) {
  const can = useCan();
  const [art, setArt] = useState<Schemas["ImageArtifact"] | null>();
  const [run, setRun] = useState<Schemas["BuildRun"]>();
  const [pubs, setPubs] = useState<Publication[]>([]);
  const [publishing, setPublishing] = useState(false);
  const [promoting, setPromoting] = useState<Publication>();
  const [changing, setChanging] = useState<{
    pub: Publication;
    action: LifecycleAction;
  }>();
  const project = useProject(run?.projectId);
  const spec = useSpec(run?.specId);
  const load = useCallback(async () => {
    const a = await api.GET("/artifacts/{artifactId}", {
      params: { path: { artifactId: id } },
    });
    setArt(a.data ?? null);
    if (!a.data) return;
    const r = await api.GET("/builds/{buildId}", {
      params: { path: { buildId: a.data.buildRunId } },
    });
    setRun(r.data);
    const p = await api.GET("/publications", {
      params: { query: { limit: 100 } },
    });
    setPubs((p.data?.items ?? []).filter((x) => x.artifactId === id));
  }, [id]);
  useLoader(load);
  useEffect(() => {
    if (pubs.every((p) => finishedPublication.has(p.state))) return;
    const t = setTimeout(() => void load(), 2000);
    return () => clearTimeout(t);
  }, [pubs, load]);
  if (art === undefined) return <Loading />;
  if (art === null)
    return (
      <Alert>This image does not exist, or you do not have access to it.</Alert>
    );
  const name = imageName({
    name: spec?.name ?? project?.name,
    version: art.version,
    digest: art.digest,
  });
  return (
    <>
      <PageHeader
        help="publishing"
        title={name}
        crumbs={[
          { label: "Images", href: "/images" },
          ...(run
            ? [
                {
                  label: project?.name ?? "Project",
                  href: `/projects/${run.projectId}`,
                },
              ]
            : []),
          { label: name },
        ]}
        description={
          <>
            <code>{short(art.digest)}</code> · Images are published by digest;
            publishing never rebuilds them.
          </>
        }
        actions={
          run && (
            <>
              {can("createKubeConversion") && (
                <GenerateButton
                  projectId={run.projectId}
                  build={run.id}
                  variant="secondary"
                />
              )}
              {can("requestPublication") && (
                <Button onPress={() => setPublishing(true)}>
                  Publish image
                </Button>
              )}
            </>
          )
        }
      />
      <dl className="samuori-details">
        {art.version && (
          <>
            <dt>Version</dt>
            <dd>
              <code>{art.version}</code>
            </dd>
          </>
        )}
        <dt>Stored image</dt>
        <dd>
          <code>{art.digest}</code>
        </dd>
        <dt>Build</dt>
        <dd>
          <Link href={`/builds/${art.buildRunId}`} className="samuori-link">
            View build and validation
          </Link>
        </dd>
        <dt>Validation</dt>
        <dd>
          <Verdict v={run?.validation} />
        </dd>
        <dt>Distribution</dt>
        <dd>
          {art.sensitivity === "restricted" ? (
            <Badge tone="warn">
              Restricted: only destinations that accept restricted images
            </Badge>
          ) : (
            "Standard"
          )}
        </dd>
      </dl>
      <section className="samuori-section" aria-labelledby="pubs-h">
        <h2 id="pubs-h">Publications</h2>
        <Table
          label="Publications"
          rows={pubs}
          empty="This image has not been published."
          columns={[
            {
              id: "state",
              name: "State",
              isRowHeader: true,
              render: (p) => {
                const s = publicationState(p.state);
                return (
                  <span className="flex flex-wrap gap-1">
                    <Badge tone={s.tone}>{s.text}</Badge>
                    {p.deprecatedAt && <Badge tone="warn">Deprecated</Badge>}
                  </span>
                );
              },
            },
            { id: "tags", name: "Tags", render: (p) => p.tags?.join(", ") },
            {
              id: "ref",
              name: "Destination",
              render: (p) =>
                p.destinationRef ? <code>{p.destinationRef}</code> : "",
            },
            {
              id: "kind",
              name: "Kind",
              render: (p) => kindText[p.kind],
            },
            {
              id: "signed",
              name: "Signed",
              render: (p) =>
                p.signatureRef
                  ? p.attestationRef
                    ? "Signature and provenance"
                    : "Signature"
                  : "",
            },
            {
              id: "reason",
              name: "Details",
              render: (p) =>
                [p.reason, p.deprecationReason, p.retirementReason]
                  .filter(Boolean)
                  .join(" · "),
            },
            {
              id: "actions",
              name: "Actions",
              render: (p) => {
                if (p.state !== "published") return null;
                const where = p.destinationRef ?? p.id;
                const act = (action: LifecycleAction) =>
                  setChanging({ pub: p, action });
                const publish = can("requestPublication");
                const deprecate = can("setPublicationDeprecation");
                const retire = can("retirePublication");
                if (!publish && !deprecate && !retire) return null;
                return (
                  <div className="samuori-actions">
                    {publish && (
                      <Button
                        variant="secondary"
                        onPress={() => setPromoting(p)}
                        aria-label={`Promote from ${where}`}
                      >
                        Promote
                      </Button>
                    )}
                    {publish && (p.tags?.length ?? 0) > 0 && (
                      <Button
                        variant="secondary"
                        onPress={() => act("rollback")}
                        aria-label={`Roll back tags to ${where}`}
                      >
                        Roll back
                      </Button>
                    )}
                    {deprecate && (
                      <Button
                        variant="secondary"
                        onPress={() =>
                          act(p.deprecatedAt ? "undeprecate" : "deprecate")
                        }
                        aria-label={`${p.deprecatedAt ? "Remove deprecation of" : "Deprecate"} ${where}`}
                      >
                        {p.deprecatedAt ? "Undeprecate" : "Deprecate"}
                      </Button>
                    )}
                    {retire && (
                      <Button
                        variant="secondary"
                        onPress={() => act("retire")}
                        aria-label={`Retire ${where}`}
                      >
                        Retire
                      </Button>
                    )}
                  </div>
                );
              },
            },
            {
              id: "requested",
              name: "Requested",
              render: (p) => new Date(p.createdAt).toLocaleString(),
            },
          ]}
        />
      </section>
      {changing && (
        <LifecycleDialog
          pub={changing.pub}
          action={changing.action}
          onClose={() => setChanging(undefined)}
          onDone={() => {
            setChanging(undefined);
            void load();
          }}
        />
      )}
      {run && promoting && (
        <PublishDialog
          isOpen
          artifactId={id}
          projectId={run.projectId}
          source={promoting}
          onClose={() => setPromoting(undefined)}
          onDone={() => {
            setPromoting(undefined);
            void load();
          }}
        />
      )}
      {run && (
        <PublishDialog
          isOpen={publishing}
          artifactId={id}
          projectId={run.projectId}
          defaultTags={art?.defaultTags}
          onClose={() => setPublishing(false)}
          onDone={() => {
            setPublishing(false);
            void load();
          }}
        />
      )}
    </>
  );
}

const kindText: Record<string, string> = {
  publish: "Publish",
  promote: "Promotion",
  rollback: "Rollback",
};

type LifecycleAction = "rollback" | "deprecate" | "undeprecate" | "retire";

const lifecycleText: Record<
  LifecycleAction,
  { title: string; submit: string; help: string }
> = {
  rollback: {
    title: "Roll back tags",
    submit: "Roll back",
    help: "The chosen tags at this destination point back to this image. It is validated again against current policy first. Protected tags may move back; immutable tags never move.",
  },
  deprecate: {
    title: "Deprecate publication",
    submit: "Deprecate",
    help: "The image stays published and usable; people see that it is discouraged and why.",
  },
  undeprecate: {
    title: "Remove deprecation",
    submit: "Remove deprecation",
    help: "The publication is no longer marked as deprecated.",
  },
  retire: {
    title: "Retire publication",
    submit: "Retire",
    help: "A retired publication cannot be promoted or rolled back to. Samuori does not delete tags at the registry; remove them there if needed.",
  },
};

function LifecycleDialog({
  pub,
  action,
  onClose,
  onDone,
}: {
  pub: Publication;
  action: LifecycleAction;
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [tags, setTags] = useState<string[]>(pub.tags ?? []);
  const [error, setError] = useState<string>();
  const text = lifecycleText[action];
  async function submit(e: FormEvent) {
    e.preventDefault();
    const path = { publicationId: pub.id };
    const { error } =
      action === "rollback"
        ? await api.POST("/publications", {
            params: { header: { "Idempotency-Key": crypto.randomUUID() } },
            body: {
              kind: "rollback",
              sourcePublicationId: pub.id,
              destinationId: pub.destinationId,
              tags,
            },
          })
        : action === "retire"
          ? await api.POST("/publications/{publicationId}/retire", {
              params: { path },
              body: { reason },
            })
          : await api.POST("/publications/{publicationId}/deprecation", {
              params: { path },
              body:
                action === "deprecate"
                  ? { deprecated: true, reason }
                  : { deprecated: false },
            });
    if (error) setError(describeError(error));
    else onDone();
  }
  return (
    <Dialog title={text.title} isOpen onOpenChange={(o) => !o && onClose()}>
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          <code>{pub.destinationRef}</code>
        </p>
        <Alert tone="info">{text.help}</Alert>
        {action === "rollback" &&
          (pub.tags ?? []).map((t) => (
            <Checkbox
              key={t}
              isSelected={tags.includes(t)}
              onChange={(on) =>
                setTags(on ? [...tags, t] : tags.filter((x) => x !== t))
              }
            >
              {t}
            </Checkbox>
          ))}
        {(action === "deprecate" || action === "retire") && (
          <TextField
            label="Reason"
            value={reason}
            onChange={setReason}
            isRequired
            minLength={10}
            description="At least 10 characters; recorded and shown with the publication."
          />
        )}
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            isDisabled={action === "rollback" && tags.length === 0}
          >
            {text.submit}
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

export function PublishDialog({
  isOpen,
  artifactId,
  projectId,
  source,
  defaultTags,
  onClose,
  onDone,
}: {
  isOpen: boolean;
  artifactId: string;
  projectId: string;
  source?: Publication;
  defaultTags?: string[];
  onClose: () => void;
  onDone: () => void;
}) {
  const [dests, setDests] = useState<Schemas["DestinationSummary"][]>();
  const [dest, setDest] = useState<string | null>(null);
  const [tags, setTags] = useState(
    source ? "" : (defaultTags ?? []).join(", "),
  );
  const [error, setError] = useState<string>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/projects/{projectId}/destinations", {
      params: { path: { projectId } },
    });
    setDests(data?.items ?? []);
  }, [projectId]);
  useLoader(load);
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!dest) return;
    setError(undefined);
    const list = tags
      .split(",")
      .map((t) => t.trim())
      .filter(Boolean);
    const { error } = await api.POST("/publications", {
      params: { header: { "Idempotency-Key": crypto.randomUUID() } },
      body: source
        ? {
            kind: "promote",
            sourcePublicationId: source.id,
            destinationId: dest,
            tags: list,
          }
        : { kind: "publish", artifactId, destinationId: dest, tags: list },
    });
    if (error) setError(describeError(error));
    else onDone();
  }
  const chosen = dests?.find((d) => d.id === dest);
  return (
    <Dialog
      title={source ? "Promote image" : "Publish image"}
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        {dests && dests.length === 0 ? (
          <Alert tone="info">
            No destination is open to this project. Ask an administrator to add
            one under Registries.
          </Alert>
        ) : (
          <Select
            label="Destination"
            options={(dests ?? []).map((d) => ({
              id: d.id,
              label: `${d.name} (${d.repository})`,
            }))}
            value={dest}
            onChange={setDest}
            isRequired
            placeholder="Choose a destination"
          />
        )}
        {source && (
          <Alert tone="info">
            Promotion copies the same image by digest. It is validated again
            against current policy first, and the source signature is checked.
          </Alert>
        )}
        {chosen?.requiresApproval && (
          <Alert tone="info">
            This destination needs an approval by someone other than you before
            the image is published.
          </Alert>
        )}
        <TextField
          placeholder="1.4.0, stable"
          label="Tags"
          value={tags}
          onChange={setTags}
          description="Comma-separated, for example 1.4.0, stable. Filled in from the image's default tags; leave empty to use them. A unique samuori- reference tag is always added."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit" isDisabled={!dest}>
            {source ? "Promote" : "Publish"}
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}
