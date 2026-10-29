import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Label,
  Link,
  TextArea,
  TextField as AriaTextField,
} from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button, buttonClass } from "../components/Button";
import { Select } from "../components/Field";
import {
  Alert,
  StateBadge,
  Loading,
  PageHeader,
  Section,
} from "../components/Feedback";
import { Table } from "../components/Table";
import { useLoader, useProject } from "../hooks";
import { navigate } from "../router";
import { useCan } from "../session/session";
import { conversionState, findingSeverity } from "../pages/display";
import { KubeQuestions } from "./KubeQuestions";
import { ConversionManage } from "./ConversionManage";

type Conversion = Schemas["KubeConversion"];
type Revision = Schemas["KubeConversionRevision"];
type Finding = Schemas["KubeConversionFinding"];
type RevisionItem = Schemas["KubeConversionRevisionList"]["items"][number];

const active = new Set(["queued", "assigned", "running"]);

export function KubeConversionPage({ id }: { id: string }) {
  const can = useCan();
  const [conv, setConv] = useState<Conversion | null>();
  const [revisions, setRevisions] = useState<RevisionItem[]>([]);
  const [runs, setRuns] = useState<Schemas["KubeConversionRun"][]>([]);
  const [selected, setSelected] = useState<number>();
  const [rev, setRev] = useState<Revision>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const project = useProject(conv?.projectId);

  const load = useCallback(async () => {
    const [c, r, runList] = await Promise.all([
      api.GET("/kube-conversions/{conversionId}", {
        params: { path: { conversionId: id } },
      }),
      api.GET("/kube-conversions/{conversionId}/revisions", {
        params: { path: { conversionId: id } },
      }),
      api.GET("/kube-conversions/{conversionId}/runs", {
        params: { path: { conversionId: id } },
      }),
    ]);
    setConv(c.data ?? null);
    setRevisions(r.data?.items ?? []);
    setRuns(runList.data?.items ?? []);
  }, [id]);
  useLoader(load);

  const current = selected ?? conv?.currentRevision ?? 0;
  useEffect(() => {
    if (!current) return;
    void api
      .GET("/kube-conversions/{conversionId}/revisions/{revision}", {
        params: { path: { conversionId: id, revision: current } },
      })
      .then(({ data }) => setRev(data));
  }, [id, current, conv?.currentRevision]);

  useEffect(() => {
    if (!conv || !active.has(conv.state)) return;
    const t = setTimeout(() => void load(), 2000);
    return () => clearTimeout(t);
  }, [conv, load]);

  if (conv === undefined) return <Loading />;
  if (conv === null)
    return (
      <Alert>
        This conversion does not exist, or you do not have access to it.
      </Alert>
    );
  const canEdit = can("rerunKubeConversion") || can("editKubeConversionFile");
  const run = conv.latestRun;

  async function rerun(answers?: Record<string, string>) {
    setError(undefined);
    setBusy(true);
    const { error } = await api.POST("/kube-conversions/{conversionId}/runs", {
      params: {
        path: { conversionId: id },
        header: { "Idempotency-Key": crypto.randomUUID() },
      },
      body: answers ? { answers } : {},
    });
    setBusy(false);
    if (error) setError(describeError(error));
    else void load();
  }
  const asked = runs.find((r) => (r.questions?.length ?? 0) > 0);
  const open =
    asked?.questions?.filter((q) => asked.answers?.[q.id] === undefined)
      .length ?? 0;

  return (
    <>
      <PageHeader
        title={conv.name}
        help="kubernetes"
        crumbs={[
          { label: "Projects", href: "/projects" },
          {
            label: project?.name ?? "Project",
            href: `/projects/${conv.projectId}`,
          },
          {
            label: "Kubernetes",
            href: `/projects/${conv.projectId}/kubernetes`,
          },
          { label: conv.name },
        ]}
        description="Generated for review. Samuori never applies these files to a cluster."
        actions={
          <>
            {canEdit && (
              <Button
                variant="secondary"
                onPress={() => void rerun()}
                isDisabled={active.has(conv.state)}
              >
                Run again
              </Button>
            )}
            <ConversionManage
              conversion={conv}
              busy={active.has(conv.state)}
              onChange={(c) => setConv(c)}
            />
          </>
        }
      />
      {error && <Alert>{error}</Alert>}
      <dl className="samuori-details">
        <dt>State</dt>
        <dd>
          <StateBadge
            state={conversionState(conv.state)}
            live={active.has(conv.state)}
          />
          {run?.detail && <span className="samuori-muted"> {run.detail}</span>}
        </dd>
        <dt>Provider</dt>
        <dd>
          {conv.provider} {run?.providerVersion}
          {run?.toolDigest && (
            <>
              {" "}
              <code>{run.toolDigest.slice(0, 19)}</code>
            </>
          )}
        </dd>
        <dt>Input</dt>
        <dd>
          {conv.input.kind}
          {conv.input.composePath && <code> {conv.input.composePath}</code>}
          {conv.input.sourceDir && <code> {conv.input.sourceDir}</code>}
          {conv.input.service && ` (service ${conv.input.service})`}
        </dd>
        <dt>Format</dt>
        <dd>{conv.options.format}</dd>
      </dl>
      {conv.state === "queued" && (
        <Alert tone="info">
          Waiting for a worker that has the approved provider version, in a pool
          verified for uploaded source. If this lasts, ask an administrator to
          check Build & conversion providers.
        </Alert>
      )}
      {conv.state === "needs-answers" && (
        <Alert tone="warning">
          {conv.provider === "move2kube" ? "Move2Kube" : "The provider"}{" "}
          answered {open} questions with its defaults and generated revision{" "}
          {conv.currentRevision} from them. Review the questions below, then run
          again with your answers.
        </Alert>
      )}
      {asked?.questions && asked.questions.length > 0 && (
        <KubeQuestions
          key={asked.id}
          questions={asked.questions}
          answers={asked.answers}
          open={open}
          canEdit={canEdit && !active.has(conv.state)}
          busy={busy}
          onSubmit={(a) => void rerun(a)}
        />
      )}
      {rev && (
        <RevisionView
          conv={conv}
          rev={rev}
          canEdit={canEdit}
          onSaved={(n) => {
            setSelected(n);
            void load();
          }}
        />
      )}
      <Section title="History">
        <Table
          label="Revisions"
          rows={revisions.map((r) => ({ ...r, id: String(r.number) }))}
          empty="No revision yet."
          dependencies={[current]}
          columns={[
            {
              id: "n",
              name: "Revision",
              isRowHeader: true,
              render: (r) => (
                <Button variant="quiet" onPress={() => setSelected(r.number)}>
                  {r.number === current ? `${r.number} (shown)` : r.number}
                </Button>
              ),
            },
            {
              id: "origin",
              name: "From",
              render: (r) =>
                r.origin === "edit" ? `edit of ${r.parent}` : "provider run",
            },
            { id: "errors", name: "Errors", render: (r) => r.errors },
            { id: "findings", name: "Findings", render: (r) => r.findings },
            {
              id: "when",
              name: "Created",
              render: (r) => new Date(r.createdAt).toLocaleString(),
            },
          ]}
        />
        <Table
          label="Runs"
          rows={runs}
          empty="No runs."
          columns={[
            {
              id: "n",
              name: "Run",
              isRowHeader: true,
              render: (r) => r.number,
            },
            { id: "state", name: "State", render: (r) => r.state },
            {
              id: "version",
              name: "Version",
              render: (r) => r.providerVersion,
            },
            { id: "detail", name: "Detail", render: (r) => r.detail ?? "" },
            {
              id: "when",
              name: "Started",
              render: (r) => new Date(r.createdAt).toLocaleString(),
            },
          ]}
        />
      </Section>
    </>
  );
}

function FindingsTable({ label, items }: { label: string; items: Finding[] }) {
  return (
    <Table
      label={label}
      rows={items.map((f, i) => ({ ...f, id: String(i) }))}
      empty="None."
      columns={[
        {
          id: "sev",
          name: "Severity",
          isRowHeader: true,
          render: (f) => <StateBadge state={findingSeverity(f.severity)} />,
        },
        { id: "msg", name: "Finding", render: (f) => f.message },
        {
          id: "where",
          name: "Where",
          render: (f) => (
            <code>{[f.file, f.object, f.field].filter(Boolean).join(" ")}</code>
          ),
        },
      ]}
    />
  );
}

function RevisionView({
  conv,
  rev,
  canEdit,
  onSaved,
}: {
  conv: Conversion;
  rev: Revision;
  canEdit: boolean;
  onSaved: (n: number) => void;
}) {
  const [path, setPath] = useState<string>();
  const [text, setText] = useState<string>();
  const [original, setOriginal] = useState("");
  const [error, setError] = useState<string>();
  const shown = path ?? rev.files[0]?.path;

  useEffect(() => {
    if (!shown) return;
    void api
      .GET("/kube-conversions/{conversionId}/revisions/{revision}/file", {
        params: {
          path: { conversionId: conv.id, revision: rev.number },
          query: { path: shown },
        },
      })
      .then(({ data }) => {
        setText(data?.content ?? "");
        setOriginal(data?.content ?? "");
      });
  }, [conv.id, rev.number, shown]);

  const secrets = useMemo(
    () =>
      rev.findings.filter((f) =>
        [
          "secret-reference",
          "secret-values-removed",
          "secret-in-configmap",
        ].includes(f.code),
      ),
    [rev.findings],
  );
  const warnings = useMemo(
    () =>
      rev.findings.filter(
        (f) => f.source === "provider" || f.source === "options",
      ),
    [rev.findings],
  );
  const policy = useMemo(
    () =>
      rev.findings.filter(
        (f) =>
          (f.source === "policy" || f.source === "schema") &&
          !secrets.includes(f),
      ),
    [rev.findings, secrets],
  );

  async function save() {
    if (!shown || text === undefined) return;
    setError(undefined);
    const { data, error } = await api.PUT(
      "/kube-conversions/{conversionId}/revisions/{revision}/file",
      {
        params: {
          path: { conversionId: conv.id, revision: rev.number },
          query: { path: shown },
        },
        body: { content: text },
      },
    );
    if (data) onSaved(data.number);
    else setError(describeError(error, "The file could not be saved."));
  }

  return (
    <>
      <Section
        title={`Revision ${rev.number}`}
        description={
          rev.errors > 0
            ? `${rev.errors} errors: policy violations or objects that are not valid for Kubernetes ${rev.lineage.kubernetesVersion ?? ""}. Fix them before you apply the files.`
            : "No errors. Review the remaining findings before you apply the files."
        }
        actions={
          <Link
            href={`/api/v1/kube-conversions/${conv.id}/revisions/${rev.number}/bundle`}
            className={buttonClass("primary")}
            download
          >
            Download ZIP
          </Link>
        }
      >
        <Table
          label="Objects"
          rows={rev.files.map((f) => ({ ...f, id: f.path }))}
          columns={[
            {
              id: "path",
              name: "File",
              isRowHeader: true,
              render: (f) => (
                <Button variant="quiet" onPress={() => setPath(f.path)}>
                  {f.path === shown ? `${f.path} (shown)` : f.path}
                </Button>
              ),
            },
            { id: "size", name: "Size", render: (f) => `${f.size} B` },
          ]}
          dependencies={[shown]}
        />
      </Section>
      <Section
        title="Secrets & environment"
        description="Credentials never stay in the files. Create each listed Secret key in the cluster before you apply."
      >
        <FindingsTable label="Secret findings" items={secrets} />
      </Section>
      <Section
        title="Policy and validation"
        description="Settings Samuori policy forbids or asks you to review, and schema errors."
      >
        <FindingsTable label="Policy findings" items={policy} />
      </Section>
      <Section
        title="Warnings & unsupported"
        description="What the provider could not translate, and what Samuori changed from your options or its records."
      >
        <FindingsTable label="Provider findings" items={warnings} />
      </Section>
      {shown && (
        <Section
          title="Preview and edit"
          description="Saving creates a new revision; Samuori validates every file again."
        >
          <Select
            label="File"
            options={rev.files.map((f) => ({ id: f.path, label: f.path }))}
            value={shown}
            onChange={setPath}
          />
          {text === undefined ? (
            <Loading />
          ) : (
            <AriaTextField
              value={text}
              onChange={setText}
              className="samuori-field"
              isReadOnly={!canEdit}
            >
              <Label>{shown}</Label>
              <TextArea
                className="samuori-code min-h-[24rem]"
                spellCheck={false}
              />
            </AriaTextField>
          )}
          {error && <Alert>{error}</Alert>}
          {canEdit && (
            <div className="samuori-actions">
              <Button
                variant="secondary"
                onPress={() => setText(original)}
                isDisabled={text === original}
              >
                Discard changes
              </Button>
              <Button
                onPress={() => void save()}
                isDisabled={text === original}
              >
                Save as new revision
              </Button>
            </div>
          )}
        </Section>
      )}
    </>
  );
}

/** The project's conversions, newest first, with the action that starts the wizard. */
export function KubeConversionsPage({ projectId }: { projectId: string }) {
  const project = useProject(projectId);
  const can = useCan();
  const [items, setItems] = useState<Conversion[]>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/projects/{projectId}/kube-conversions", {
      params: { path: { projectId } },
    });
    setItems(data?.items ?? []);
  }, [projectId]);
  useLoader(load);
  return (
    <>
      <PageHeader
        title="Kubernetes configuration"
        help="kubernetes"
        crumbs={[
          { label: "Projects", href: "/projects" },
          { label: project?.name ?? "Project", href: `/projects/${projectId}` },
          { label: "Kubernetes" },
        ]}
        description="Manifests, Helm charts or Kustomize for this project, to review and apply yourself. Samuori never deploys them."
        actions={
          can("createKubeConversion") && (
            <GenerateButton projectId={projectId} />
          )
        }
      />
      {!items ? (
        <Loading />
      ) : (
        <ConversionTable projectId={projectId} items={items} />
      )}
    </>
  );
}

export function GenerateButton({
  projectId,
  build,
  variant = "primary",
  label = "Generate Kubernetes configuration",
}: {
  projectId: string;
  build?: string;
  variant?: "primary" | "secondary";
  label?: string;
}) {
  return (
    <Button
      variant={variant}
      onPress={() =>
        navigate(
          `/projects/${projectId}/kubernetes/new${build ? `?build=${encodeURIComponent(build)}` : ""}`,
        )
      }
    >
      {label}
    </Button>
  );
}

export function ConversionTable({
  items,
}: {
  projectId: string;
  items: Conversion[];
}) {
  return (
    <Table
      label="Kubernetes conversions"
      rows={items}
      empty="No conversions yet. Choose Generate Kubernetes configuration to make one."
      columns={[
        {
          id: "name",
          name: "Name",
          isRowHeader: true,
          render: (c) => (
            <Link href={`/kube-conversions/${c.id}`} className="samuori-link">
              {c.name}
            </Link>
          ),
        },
        {
          id: "state",
          name: "State",
          render: (c) => <StateBadge state={conversionState(c.state)} />,
        },
        { id: "format", name: "Format", render: (c) => c.options.format },
        {
          id: "rev",
          name: "Revision",
          render: (c) => c.currentRevision || "none",
        },
        {
          id: "when",
          name: "Created",
          render: (c) => new Date(c.createdAt).toLocaleString(),
          sortValue: (c) => c.createdAt,
        },
      ]}
    />
  );
}
