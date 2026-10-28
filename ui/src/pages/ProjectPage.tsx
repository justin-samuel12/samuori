import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type FormEvent,
} from "react";
import { useQuery } from "@tanstack/react-query";
import { Form, Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { TextField } from "../components/Field";
import { Alert, Loading, PageHeader } from "../components/Feedback";
import { Table } from "../components/Table";
import { DeleteUploadDialog, isUploader } from "../components/DeleteUpload";
import { bytes, correctName, shortDigest } from "../format";
import { useLoader } from "../hooks";
import { navigate } from "../router";
import { isForbidden, useCan, useMe } from "../session/session";
import type { Scope } from "../session/can";
import { teamName, useTeams } from "./useTeams";
import { publicationModes } from "./ProjectsPage";
import { ConversionTable, GenerateButton } from "../kube/KubeConversionPage";
import { BaseImageRequests, RequestBaseImageDialog } from "./BaseImageRequests";
import { EgressAllowances } from "./EgressAllowances";
import { WorkflowRail } from "../components/WorkflowRail";
import type { OperationId } from "../api/generated/roles";
import {
  ArchiveProjectButton,
  ArchivedBanner,
  isArchived,
} from "./ProjectLifecycle";

export function ProjectPage({ id }: { id: string }) {
  const can = useCan();
  const { teams } = useTeams();
  const [project, setProject] = useState<Schemas["Project"] | null>();

  useEffect(() => {
    void api
      .GET("/projects/{projectId}", { params: { path: { projectId: id } } })
      .then(({ data }) => setProject(data ?? null));
  }, [id]);

  if (project === undefined) return <Loading />;
  if (project === null)
    return (
      <>
        <PageHeader title="Project not found" />
        <Alert>
          This project does not exist, or you do not have access to it.
        </Alert>
        <Link href="/projects" className="samuori-link">
          Back to projects
        </Link>
      </>
    );
  const mode =
    publicationModes.find((m) => m.id === project.publicationMode) ??
    publicationModes[0];
  const scope = {
    teamId: project.teamId,
    projectId: project.id,
    archived: isArchived(project),
  };
  return (
    <>
      <nav aria-label="Breadcrumb" className="samuori-breadcrumb">
        <Link href="/projects" className="samuori-link">
          Projects
        </Link>{" "}
        / {project.name}
      </nav>
      <PageHeader
        title={project.name}
        actions={
          can("archiveProject", scope) && (
            <ArchiveProjectButton project={project} scope={scope} />
          )
        }
      />
      <ArchivedBanner project={project} scope={scope} onChange={setProject} />
      <dl className="samuori-details">
        <dt>Team</dt>
        <dd>{project.teamName ?? teamName(teams, project.teamId)}</dd>
        <dt>Publishing</dt>
        <dd>
          {mode?.label} — {mode?.description}
          {can("updateProject", scope) && (
            <PublicationMode project={project} onChange={setProject} />
          )}
        </dd>
        <dt>Created</dt>
        <dd>{new Date(project.createdAt).toLocaleString()}</dd>
        <dt>Project ID</dt>
        <dd>
          <code>{project.id}</code>
        </dd>
      </dl>
      <ProjectWorkflow projectId={project.id} scope={scope} />
      <Sources projectId={project.id} scope={scope} />
      <KubeSection projectId={project.id} scope={scope} />
      <RequestsAndExceptions projectId={project.id} scope={scope} />
    </>
  );
}

const countLimit = 100;

function useCount(
  op: OperationId,
  scope: Scope,
  key: string,
  fetch: () => Promise<{ data?: { items: unknown[] } }>,
) {
  const can = useCan();
  // A failed request leaves the count unknown rather than showing zero.
  return useQuery({
    queryKey: ["project-count", key],
    queryFn: async () => {
      const { data } = await fetch();
      if (!data) throw new Error(`${key} could not be counted`);
      return data.items.length;
    },
    enabled: can(op, scope),
  }).data;
}

function counted(n: number | undefined, one: string, many: string) {
  if (n === undefined) return undefined;
  const v = n >= countLimit ? `${countLimit}+` : String(n);
  return `${v} ${n === 1 ? one : many}`;
}

/** The project's place on the path from source to image, as counts of what exists; links go to each stage. */
function ProjectWorkflow({
  projectId,
  scope,
}: {
  projectId: string;
  scope: Scope;
}) {
  const can = useCan();
  const path = { projectId };
  const query = { limit: countLimit };
  const uploads = useCount(
    "listSnapshots",
    scope,
    `${projectId}/snapshots`,
    () =>
      api.GET("/projects/{projectId}/snapshots", { params: { path, query } }),
  );
  const specs = useCount("listSpecs", scope, `${projectId}/specs`, () =>
    api.GET("/projects/{projectId}/specs", { params: { path, query } }),
  );
  const builds = useCount("listBuilds", scope, `${projectId}/builds`, () =>
    api.GET("/builds", { params: { query: { ...query, projectId } } }),
  );
  const images = useCount(
    "listProjectImages",
    scope,
    `${projectId}/images`,
    () => api.GET("/projects/{projectId}/images", { params: { path, query } }),
  );
  const kube = useCount("listKubeConversions", scope, `${projectId}/kube`, () =>
    api.GET("/projects/{projectId}/kube-conversions", {
      params: { path, query },
    }),
  );
  const base = `/projects/${projectId}`;
  return (
    <section className="samuori-section" aria-labelledby="workflow-h">
      <div className="samuori-section-head">
        <h2 id="workflow-h">From source to image</h2>
      </div>
      <WorkflowRail
        label="This project from source to image"
        info={{
          upload: {
            href: can("createUploadSession", scope)
              ? `${base}/upload`
              : undefined,
            meta: counted(uploads, "upload", "uploads"),
          },
          discover: {
            meta: uploads ? "Open an upload for its analysis" : undefined,
          },
          configure: {
            href: can("createSpec", scope) ? `${base}/new-image` : undefined,
            meta: counted(specs, "image definition", "image definitions"),
          },
          build: {
            href: can("listBuilds", scope) ? "/builds" : undefined,
            meta: counted(builds, "build run", "build runs"),
          },
          validate: { href: can("listBuilds", scope) ? "/builds" : undefined },
          publish: {
            href: can("listProjectImages", scope) ? "/images" : undefined,
            meta: counted(images, "built image", "built images"),
          },
          kubernetes: {
            href: can("listKubeConversions", scope)
              ? `${base}/kubernetes`
              : undefined,
            meta: counted(kube, "conversion", "conversions"),
          },
        }}
      />
    </section>
  );
}

function Sources({ projectId, scope }: { projectId: string; scope: Scope }) {
  const me = useMe();
  const can = useCan();
  const [snaps, setSnaps] = useState<Schemas["SourceSnapshot"][]>();
  const [deleting, setDeleting] = useState<Schemas["SourceSnapshot"]>();
  const [forbidden, setForbidden] = useState(false);
  const load = useCallback(async () => {
    const res = await api.GET("/projects/{projectId}/snapshots", {
      params: { path: { projectId }, query: { limit: 20 } },
    });
    setForbidden(isForbidden(res));
    setSnaps(res.data?.items ?? []);
  }, [projectId]);
  useLoader(load);
  const canUpload = can("createUploadSession", scope);
  const canCreateSpec = can("createSpec", scope);
  if (forbidden) return null;
  return (
    <section className="samuori-section" aria-labelledby="sources-h">
      <div className="samuori-section-head">
        <div>
          <h2 id="sources-h">Source uploads</h2>
          <p className="samuori-muted">
            Each upload is a fixed copy of your files that builds can use.
          </p>
        </div>
        {(canUpload || canCreateSpec) && (
          <div className="samuori-actions">
            {canUpload && (
              <Button
                variant="secondary"
                onPress={() => navigate(`/projects/${projectId}/upload`)}
              >
                Upload source
              </Button>
            )}
            {canCreateSpec && (
              <Button
                onPress={() => navigate(`/projects/${projectId}/new-image`)}
              >
                New image
              </Button>
            )}
          </div>
        )}
      </div>
      {!snaps ? (
        <Loading />
      ) : (
        <Table
          label="Source uploads"
          rows={snaps}
          empty={
            canUpload
              ? "No uploads yet. Upload your project folder to get started."
              : "No uploads yet."
          }
          columns={[
            {
              id: "when",
              name: "Uploaded",
              isRowHeader: true,
              render: (s) => (
                <Link
                  href={`/projects/${projectId}/snapshots/${s.id}`}
                  className="samuori-link"
                >
                  {new Date(s.createdAt).toLocaleString()}
                </Link>
              ),
            },
            {
              id: "files",
              name: "Files",
              render: (s) => `${s.fileCount} (${bytes(s.totalBytes)})`,
            },
            {
              id: "warnings",
              name: "Things to check",
              render: (s) => (s.warnings?.length ?? 0) || "None",
            },
            {
              id: "digest",
              name: "Content ID",
              render: (s) => <code>{shortDigest(s.digest)}</code>,
            },
            {
              id: "actions",
              name: "Actions",
              render: (s) =>
                isUploader(s, me.principal.id) ? (
                  <Button
                    variant="quiet"
                    aria-label={`Delete the upload from ${new Date(s.createdAt).toLocaleString()}`}
                    onPress={() => setDeleting(s)}
                  >
                    Delete
                  </Button>
                ) : null,
            },
          ]}
        />
      )}
      <DeleteUploadDialog
        key={deleting?.id ?? "none"}
        snapshot={deleting}
        onClose={() => setDeleting(undefined)}
        onDone={() => {
          setDeleting(undefined);
          void load();
        }}
      />
    </section>
  );
}

function PublicationMode({
  project,
  onChange,
}: {
  project: Schemas["Project"];
  onChange: (p: Schemas["Project"]) => void;
}) {
  const [error, setError] = useState<string>();
  const next = project.publicationMode === "automatic" ? "manual" : "automatic";
  async function change() {
    setError(undefined);
    const { data, error } = await api.PATCH("/projects/{projectId}", {
      params: { path: { projectId: project.id } },
      body: { publicationMode: next },
    });
    if (error || !data) setError(describeError(error));
    else onChange(data);
  }
  return (
    <div className="samuori-inline">
      <Button variant="quiet" onPress={() => void change()}>
        {next === "automatic" ? "Publish automatically" : "Publish manually"}
      </Button>
      {next === "automatic" && (
        <span className="samuori-muted">
          Automatic publication also needs the build specification to ask for it
          and name a destination open to this project.
        </span>
      )}
      {error && <Alert>{error}</Alert>}
    </div>
  );
}

type OutputException = Schemas["OutputException"];

function OutputExceptions({
  projectId,
  scope,
  onCount,
}: {
  projectId: string;
  scope: Scope;
  onCount: (n: number) => void;
}) {
  const can = useCan();
  const [forbidden, setForbidden] = useState(false);
  const [items, setItems] = useState<OutputException[]>();
  const [error, setError] = useState<string>();
  const load = useCallback(async () => {
    const res = await api.GET("/projects/{projectId}/output-exceptions", {
      params: { path: { projectId } },
    });
    setForbidden(isForbidden(res));
    setItems(res.data?.items ?? []);
  }, [projectId]);
  useLoader(load);
  const count = forbidden ? 0 : items?.length;
  useEffect(() => {
    if (count !== undefined) onCount(count);
  }, [count, onCount]);
  const canRevoke = can("revokeOutputException", scope);
  async function revoke(x: OutputException) {
    const { error } = await api.POST(
      "/output-exceptions/{exceptionId}/revoke",
      {
        params: {
          path: { exceptionId: x.id },
        },
        body: { reason: "revoked from the project page" },
      },
    );
    setError(error ? describeError(error) : undefined);
    void load();
  }
  if (!count || !items) return null;
  return (
    <div>
      <h3>Output exceptions</h3>
      <p className="samuori-muted">
        An approved exception lets findings that need one pass validation for
        the listed paths until it expires. Images that pass this way are
        restricted to destinations that accept them. Blocking findings are never
        excepted.
      </p>
      {error && <Alert>{error}</Alert>}
      {items && (
        <Table
          label="Output exceptions"
          rows={items}
          columns={[
            {
              id: "paths",
              name: "Paths",
              isRowHeader: true,
              render: (x) => <code>{x.pathPatterns.join(", ")}</code>,
            },
            {
              id: "classes",
              name: "Classes",
              render: (x) => (x.classes.length ? x.classes.join(", ") : "any"),
            },
            { id: "state", name: "State", render: (x) => x.state },
            {
              id: "why",
              name: "Justification",
              render: (x) => x.justification,
            },
            {
              id: "expires",
              name: "Expires",
              render: (x) => new Date(x.expiresAt).toLocaleString(),
            },
            {
              id: "actions",
              name: "Actions",
              render: (x) =>
                canRevoke &&
                (x.state === "approved" || x.state === "requested") && (
                  <Button
                    variant="secondary"
                    onPress={() => void revoke(x)}
                    aria-label={`Revoke exception for ${x.pathPatterns.join(", ")}`}
                  >
                    Revoke
                  </Button>
                ),
            },
          ]}
        />
      )}
    </div>
  );
}

function RequestException({
  projectId,
  isOpen,
  onClose,
  onDone,
}: {
  projectId: string;
  isOpen: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const [paths, setPaths] = useState("");
  const [justification, setJustification] = useState("");
  const [days, setDays] = useState("30");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    const { error } = await api.POST(
      "/projects/{projectId}/output-exceptions",
      {
        params: {
          path: { projectId },
        },
        body: {
          pathPatterns: paths
            .split(/[\s,]+/)
            .map((p) => p.trim())
            .filter(Boolean),
          justification,
          expiresInDays: Number(days),
        },
      },
    );
    if (error) setError(describeError(error));
    else {
      setPaths("");
      setJustification("");
      setError(undefined);
      onDone();
    }
  }
  return (
    <Dialog
      title="Request an output exception"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          label="Paths"
          value={paths}
          onChange={setPaths}
          isRequired
          description="Image paths without a leading slash, separated by commas or spaces; ** matches any depth, for example var/opt/app/secrets/**. A pattern that matches everything is refused."
        />
        <TextField
          label="Justification"
          value={justification}
          onChange={setJustification}
          isRequired
          minLength={20}
          description="Why this output belongs in the image; at least 20 characters."
        />
        <TextField
          label="Expires after (days)"
          value={days}
          onChange={setDays}
          isRequired
          inputMode="numeric"
          description="At most 90 days."
        />
        <Alert tone="info">
          An administrator other than you decides the request.
        </Alert>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Request</Button>
        </div>
      </Form>
    </Dialog>
  );
}

type RepositoryRequest = Schemas["RepositoryRequest"];

const requestStateText: Record<RepositoryRequest["state"], string> = {
  pending: "Waiting for an administrator",
  approved: "Ready",
  rejected: "Rejected",
  expired: "Expired",
};

function RepositoryRequests({
  projectId,
  onCount,
}: {
  projectId: string;
  onCount: (n: number) => void;
}) {
  const [forbidden, setForbidden] = useState(false);
  const [items, setItems] = useState<RepositoryRequest[]>();
  const load = useCallback(async () => {
    const res = await api.GET("/projects/{projectId}/repository-requests", {
      params: { path: { projectId } },
    });
    setForbidden(isForbidden(res));
    setItems(res.data?.items ?? []);
  }, [projectId]);
  useLoader(load);
  const count = forbidden ? 0 : items?.length;
  useEffect(() => {
    if (count !== undefined) onCount(count);
  }, [count, onCount]);
  if (!count || !items) return null;
  return (
    <div>
      <h3>Registry repositories</h3>
      <p className="samuori-muted">
        Repositories asked of an administrator to publish this project&apos;s
        images to. When a request is fulfilled, its destination appears in the
        publication choices.
      </p>
      {items && (
        <Table
          label="Repository requests"
          rows={items}
          columns={[
            {
              id: "repo",
              name: "Repository",
              isRowHeader: true,
              render: (r) => <code>{r.repository}</code>,
            },
            {
              id: "state",
              name: "State",
              render: (r) =>
                r.destinationName
                  ? `${requestStateText[r.state]}: destination ${r.destinationName}`
                  : requestStateText[r.state],
            },
            { id: "why", name: "Reason", render: (r) => r.justification },
            {
              id: "when",
              name: "Requested",
              render: (r) => new Date(r.createdAt).toLocaleString(),
            },
          ]}
        />
      )}
    </div>
  );
}

type RequestGroup = "repositories" | "baseImages" | "network" | "exceptions";
const requestGroups: RequestGroup[] = [
  "repositories",
  "baseImages",
  "network",
  "exceptions",
];

/**
 * What the project asked administrators for: repositories, base images, network access and output exceptions. Only
 * groups with entries are shown; with none, only the request actions the user may start remain, and nothing at all
 * renders before every group has loaded.
 */
function RequestsAndExceptions({
  projectId,
  scope,
}: {
  projectId: string;
  scope: Scope;
}) {
  const can = useCan();
  const [counts, setCounts] = useState<Partial<Record<RequestGroup, number>>>(
    {},
  );
  const [generation, setGeneration] = useState(0);
  const [dialog, setDialog] = useState<
    "repository" | "baseImage" | "exception"
  >();
  const report = useMemo(
    () =>
      Object.fromEntries(
        requestGroups.map((g) => [
          g,
          (n: number) => setCounts((c) => (c[g] === n ? c : { ...c, [g]: n })),
        ]),
      ) as Record<RequestGroup, (n: number) => void>,
    [],
  );
  const done = () => {
    setDialog(undefined);
    setGeneration((g) => g + 1);
  };
  const actions = [
    can("requestRepository", scope) && (
      <Button
        key="repository"
        variant="secondary"
        onPress={() => setDialog("repository")}
      >
        Request a repository
      </Button>
    ),
    can("requestBaseImage", scope) && (
      <Button
        key="baseImage"
        variant="secondary"
        onPress={() => setDialog("baseImage")}
      >
        Request a base image
      </Button>
    ),
    can("requestOutputException", scope) && (
      <Button
        key="exception"
        variant="secondary"
        onPress={() => setDialog("exception")}
      >
        Request exception
      </Button>
    ),
  ].filter(Boolean);
  const known = requestGroups.every((g) => counts[g] !== undefined);
  const total = requestGroups.reduce((n, g) => n + (counts[g] ?? 0), 0);
  const groups = (
    <>
      <RepositoryRequests
        key={`r${generation}`}
        projectId={projectId}
        onCount={report.repositories}
      />
      <BaseImageRequests
        key={`b${generation}`}
        projectId={projectId}
        onCount={report.baseImages}
      />
      <EgressAllowances projectId={projectId} onCount={report.network} />
      <OutputExceptions
        key={`e${generation}`}
        projectId={projectId}
        scope={scope}
        onCount={report.exceptions}
      />
    </>
  );
  const dialogs = (
    <>
      <RequestRepositoryDialog
        projectId={projectId}
        isOpen={dialog === "repository"}
        onClose={() => setDialog(undefined)}
        onDone={done}
      />
      <RequestBaseImageDialog
        projectId={projectId}
        isOpen={dialog === "baseImage"}
        onClose={() => setDialog(undefined)}
        onDone={done}
      />
      <RequestException
        projectId={projectId}
        isOpen={dialog === "exception"}
        onClose={() => setDialog(undefined)}
        onDone={done}
      />
    </>
  );
  if (known && total === 0) {
    return (
      <>
        {groups}
        {actions.length > 0 && (
          <div className="samuori-actions samuori-actions--start">
            <span className="samuori-muted">
              Need something from an administrator?
            </span>
            {actions}
          </div>
        )}
        {dialogs}
      </>
    );
  }
  return (
    <section
      className="samuori-section"
      aria-labelledby="requests-h"
      hidden={!known}
    >
      <div className="samuori-section-head">
        <div>
          <h2 id="requests-h">Requests &amp; exceptions</h2>
          <p className="samuori-muted">
            What this project asked administrators for. Network access is
            requested from an upload&apos;s analysis or a blocked build.
          </p>
        </div>
        {actions.length > 0 && <div className="samuori-actions">{actions}</div>}
      </div>
      {groups}
      {dialogs}
    </section>
  );
}

function RequestRepositoryDialog({
  projectId,
  isOpen,
  onClose,
  onDone,
}: {
  projectId: string;
  isOpen: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const [repository, setRepository] = useState("");
  const [justification, setJustification] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    const { error } = await api.POST(
      "/projects/{projectId}/repository-requests",
      {
        params: { path: { projectId } },
        body: {
          repository: correctName(repository, {
            extra: "/._",
            done: true,
            max: 255,
          }),
          justification,
        },
      },
    );
    if (error) setError(describeError(error));
    else {
      setRepository("");
      setJustification("");
      onDone();
    }
  }
  return (
    <Dialog
      title="Request a repository"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          label="Repository"
          value={repository}
          onChange={(v) => setRepository(correctName(v, { extra: "/._" }))}
          isRequired
          description="Saved in lowercase, such as payments/orders-api."
        />
        <TextField
          label="Why it is needed"
          value={justification}
          onChange={setJustification}
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

function KubeSection({
  projectId,
  scope,
}: {
  projectId: string;
  scope: Scope;
}) {
  const can = useCan();
  const [forbidden, setForbidden] = useState(false);
  const [items, setItems] = useState<Schemas["KubeConversion"][]>();
  const load = useCallback(async () => {
    const res = await api.GET("/projects/{projectId}/kube-conversions", {
      params: { path: { projectId }, query: { limit: 5 } },
    });
    setForbidden(isForbidden(res));
    setItems(res.data?.items ?? []);
  }, [projectId]);
  useLoader(load);
  // The workflow's optional Kubernetes card links to the conversions page, which starts the first one.
  if (forbidden || !items?.length) return null;
  return (
    <section className="samuori-section" aria-labelledby="kube-h">
      <div className="samuori-section-head">
        <div>
          <h2 id="kube-h">Kubernetes configuration</h2>
          <p className="samuori-muted">
            Manifests, Helm charts or Kustomize generated from a Compose file or
            an image, checked by Samuori, for you to review and apply. Samuori
            never deploys them.{" "}
            <Link
              href={`/projects/${projectId}/kubernetes`}
              className="samuori-link"
            >
              All conversions
            </Link>
          </p>
        </div>
        {can("createKubeConversion", scope) && (
          <div className="samuori-actions">
            <GenerateButton projectId={projectId} variant="secondary" />
          </div>
        )}
      </div>
      <ConversionTable projectId={projectId} items={items} />
    </section>
  );
}
