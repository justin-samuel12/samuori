import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Form, Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { Checkbox, Select, TextField, type Option } from "../components/Field";
import { Alert, EmptyState, Loading, PageHeader } from "../components/Feedback";
import { Table } from "../components/Table";
import { useLoader } from "../hooks";
import { isOrgAdmin, useCan, useMe } from "../session/session";

type Pipeline = Schemas["Pipeline"];
type Run = Schemas["PipelineRun"];

const stateText: Record<Run["state"], string> = {
  queued: "Queued",
  fetching: "Fetching the commit",
  started: "Build started",
  failed: "Failed",
  skipped: "Skipped",
};

const triggerText: Record<Run["trigger"], string> = {
  webhook: "Git push",
  manual: "By hand",
  api: "API",
};

function newKey(): string {
  return `pipeline-run-${crypto.randomUUID()}`;
}

function branchOf(ref: string): string {
  return ref.replace(/^refs\/(heads|tags)\//, "");
}

export function PipelinesPage() {
  const me = useMe();
  const admin = isOrgAdmin(me);
  const [projects, setProjects] = useState<Option[]>([]);
  const [project, setProject] = useState<string>("");
  const [pipelines, setPipelines] = useState<Pipeline[]>();
  const [selected, setSelected] = useState<Pipeline>();
  const [error, setError] = useState<string>();
  const [creating, setCreating] = useState(false);
  const [secret, setSecret] = useState<{ name: string; value: string }>();

  useEffect(() => {
    void (async () => {
      const { data } = await api.GET("/projects", {
        params: { query: { limit: 100 } },
      });
      const list = (data?.items ?? []).map((p) => ({
        id: p.id,
        label: p.name,
      }));
      setProjects(list);
      if (!admin && list[0]) setProject(list[0].id);
    })();
  }, [admin]);

  const load = useCallback(async () => {
    if (!admin && !project) return;
    const { data, error } = await api.GET("/pipelines", {
      params: { query: { projectId: project || undefined } },
    });
    if (data) setPipelines(data.items);
    else setError(describeError(error));
  }, [admin, project]);
  useLoader(load);

  async function rotate(p: Pipeline) {
    const { data, error } = await api.POST(
      "/pipelines/{pipelineId}/hook-secret",
      { params: { path: { pipelineId: p.id } } },
    );
    if (data) setSecret({ name: p.name, value: data.secret });
    else setError(describeError(error));
  }

  async function setEnabled(p: Pipeline, enabled: boolean) {
    const { error } = await api.POST("/pipelines/{pipelineId}/state", {
      params: { path: { pipelineId: p.id } },
      body: { enabled },
    });
    if (error) setError(describeError(error));
    else await load();
  }

  const projectName = (id: string) =>
    projects.find((p) => p.id === id)?.label ?? "Project";

  return (
    <>
      <PageHeader
        title="Pipelines"
        description="Build an image when a branch or tag changes in Git. A run fetches the commit, stores it as a snapshot and starts a build of the pipeline's spec; checks, approvals and publication then follow the project's settings."
        actions={
          admin ? (
            <Button onPress={() => setCreating(true)}>New pipeline</Button>
          ) : undefined
        }
      />
      {error && <Alert>{error}</Alert>}
      {secret && (
        <Alert tone="warning">
          <p>
            Webhook secret for {secret.name}. It is shown only once; enter it in
            the repository&apos;s webhook settings.
          </p>
          <code className="samuori-secret">{secret.value}</code>
          <div className="samuori-actions">
            <Button variant="secondary" onPress={() => setSecret(undefined)}>
              I have stored it
            </Button>
          </div>
        </Alert>
      )}
      <div className="samuori-toolbar">
        <Select
          label="Project"
          value={project || (admin ? "all" : null)}
          onChange={(v) => {
            setProject(v === "all" ? "" : v);
            setSelected(undefined);
          }}
          options={
            admin
              ? [{ id: "all", label: "All projects" }, ...projects]
              : projects
          }
        />
      </div>
      {!pipelines && !error && (admin || project) && <Loading />}
      {pipelines && pipelines.length === 0 && (
        <EmptyState title="No pipelines">
          {admin
            ? "Add a pipeline to build from a Git repository."
            : "An administrator can add a pipeline for this project."}
        </EmptyState>
      )}
      {pipelines && pipelines.length > 0 && (
        <Table
          label="Pipelines"
          rows={pipelines}
          dependencies={[projects, admin]}
          columns={[
            {
              id: "name",
              name: "Name",
              isRowHeader: true,
              render: (p) => (
                <Button variant="quiet" onPress={() => setSelected(p)}>
                  {p.name}
                </Button>
              ),
            },
            {
              id: "project",
              name: "Project",
              render: (p) => projectName(p.projectId),
            },
            { id: "repo", name: "Repository", render: (p) => p.repoUrl },
            {
              id: "refs",
              name: "Builds",
              render: (p) => p.refs.map(branchOf).join(", "),
            },
            {
              id: "state",
              name: "State",
              render: (p) => (p.enabled ? "Enabled" : "Disabled"),
            },
            {
              id: "actions",
              name: "Actions",
              render: (p) =>
                admin ? (
                  <div className="samuori-actions">
                    <Button
                      variant="quiet"
                      onPress={() => void setEnabled(p, !p.enabled)}
                    >
                      {p.enabled ? "Disable" : "Enable"}
                    </Button>
                    <Button variant="quiet" onPress={() => void rotate(p)}>
                      New secret
                    </Button>
                  </div>
                ) : null,
            },
          ]}
        />
      )}
      {selected && <PipelineRuns pipeline={selected} />}
      {admin && (
        <CreateDialog
          isOpen={creating}
          projects={projects}
          onClose={() => setCreating(false)}
          onDone={(p) => {
            setCreating(false);
            setSecret({ name: p.name, value: p.secret });
            void load();
          }}
        />
      )}
    </>
  );
}

function PipelineRuns({ pipeline }: { pipeline: Pipeline }) {
  const canRun = useCan()("startPipelineRun");
  const [runs, setRuns] = useState<Run[]>();
  const [error, setError] = useState<string>();
  const [ref, setRef] = useState(
    pipeline.refs.find((r) => !/[*?]/.test(r)) ?? "",
  );
  const load = useCallback(async () => {
    const { data, error } = await api.GET("/pipelines/{pipelineId}/runs", {
      params: { path: { pipelineId: pipeline.id }, query: { limit: 25 } },
    });
    if (data) setRuns(data.items);
    else setError(describeError(error));
  }, [pipeline.id]);
  useLoader(load);

  async function start(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    const { error } = await api.POST("/pipelines/{pipelineId}/runs", {
      params: {
        path: { pipelineId: pipeline.id },
        header: { "Idempotency-Key": newKey() },
      },
      body: { ref },
    });
    if (error) setError(describeError(error));
    else await load();
  }

  const hookUrl = `${window.location.origin}${pipeline.hookPath}`;
  return (
    <section className="samuori-section" aria-labelledby="runs-h">
      <h2 id="runs-h">{pipeline.name}</h2>
      <dl className="samuori-details">
        <dt>Webhook URL</dt>
        <dd>
          <code>{hookUrl}</code>
        </dd>
        <dt>Webhook verification</dt>
        <dd>
          {pipeline.hookScheme === "gitlab-token"
            ? "GitLab secret token (X-Gitlab-Token)"
            : "HMAC-SHA256 signature (X-Hub-Signature-256); content type application/json"}
        </dd>
        <dt>Spec revision</dt>
        <dd>
          {pipeline.specRevision === 0
            ? "Latest"
            : `Revision ${pipeline.specRevision}`}
        </dd>
      </dl>
      {error && <Alert>{error}</Alert>}
      {canRun && (
        <Form onSubmit={start} className="samuori-inline">
          <TextField
            label="Ref to build"
            value={ref}
            onChange={setRef}
            isRequired
            description="A full ref the pipeline builds, such as refs/heads/main."
          />
          <Button type="submit" isDisabled={!pipeline.enabled}>
            Run now
          </Button>
        </Form>
      )}
      {!runs && !error && <Loading />}
      {runs && (
        <Table
          label={`Runs of ${pipeline.name}`}
          rows={runs}
          empty="No runs yet."
          columns={[
            {
              id: "when",
              name: "Started",
              isRowHeader: true,
              render: (r) => new Date(r.createdAt).toLocaleString(),
            },
            {
              id: "trigger",
              name: "Trigger",
              render: (r) => triggerText[r.trigger],
            },
            { id: "ref", name: "Ref", render: (r) => branchOf(r.ref) },
            {
              id: "commit",
              name: "Commit",
              render: (r) =>
                r.commit || r.requestedCommit ? (
                  <code>
                    {(r.commit ?? r.requestedCommit ?? "").slice(0, 12)}
                  </code>
                ) : (
                  ""
                ),
            },
            {
              id: "state",
              name: "State",
              render: (r) =>
                r.buildRunId ? (
                  <Link href={`/builds/${r.buildRunId}`}>
                    {stateText[r.state]}
                  </Link>
                ) : (
                  stateText[r.state]
                ),
            },
            {
              id: "detail",
              name: "Detail",
              render: (r) =>
                [
                  r.message,
                  r.skippedEntries > 0
                    ? `${r.skippedEntries} entries such as submodules were not included`
                    : "",
                ]
                  .filter(Boolean)
                  .join(" "),
            },
          ]}
        />
      )}
    </section>
  );
}

function CreateDialog({
  isOpen,
  projects,
  onClose,
  onDone,
}: {
  isOpen: boolean;
  projects: Option[];
  onClose: () => void;
  onDone: (p: Schemas["PipelineCreated"]) => void;
}) {
  const [name, setName] = useState("");
  const [project, setProject] = useState<string | null>(null);
  const [specs, setSpecs] = useState<Option[]>([]);
  const [spec, setSpec] = useState<string | null>(null);
  const [owners, setOwners] = useState<Option[]>([]);
  const [owner, setOwner] = useState<string | null>(null);
  const [repoUrl, setRepoUrl] = useState("");
  const [refs, setRefs] = useState("refs/heads/main");
  const [scheme, setScheme] = useState<"hmac-sha256" | "gitlab-token">(
    "hmac-sha256",
  );
  const [username, setUsername] = useState("");
  const [token, setToken] = useState("");
  const [allowPrivate, setAllowPrivate] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    if (!isOpen) return;
    void (async () => {
      const { data } = await api.GET("/service-identities");
      setOwners(
        (data?.items ?? [])
          .filter((s) => !s.disabled)
          .map((s) => ({ id: s.id, label: s.displayName || s.username })),
      );
    })();
  }, [isOpen]);

  useEffect(() => {
    if (!project) return;
    void (async () => {
      const { data } = await api.GET("/projects/{projectId}/specs", {
        params: { path: { projectId: project }, query: { limit: 100 } },
      });
      setSpecs((data?.items ?? []).map((s) => ({ id: s.id, label: s.name })));
    })();
  }, [project]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    if (!project || !spec || !owner) {
      setError("Choose a project, a spec and the service identity to run as.");
      return;
    }
    const { data, error } = await api.POST("/pipelines", {
      body: {
        name,
        projectId: project,
        specId: spec,
        repoUrl,
        refs: refs
          .split(/[\s,]+/)
          .map((r) => r.trim())
          .filter(Boolean),
        hookScheme: scheme,
        ownerId: owner,
        username: username || undefined,
        token: token || undefined,
        allowPrivateNetwork: allowPrivate,
      },
    });
    if (data) {
      setName("");
      setRepoUrl("");
      setToken("");
      onDone(data);
    } else setError(describeError(error));
  }

  return (
    <Dialog
      title="New pipeline"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField label="Name" value={name} onChange={setName} isRequired />
        <Select
          label="Project"
          value={project}
          onChange={(v) => {
            setProject(v);
            setSpec(null);
          }}
          options={projects}
          isRequired
        />
        <Select
          label="Spec"
          value={spec}
          onChange={setSpec}
          options={specs}
          isRequired
          description="Runs build the spec's latest revision."
        />
        <Select
          label="Run as"
          value={owner}
          onChange={setOwner}
          options={owners}
          isRequired
          description="A service identity with the developer or maintainer role on the project. Runs stop if it loses that role."
        />
        <TextField
          label="Repository URL"
          value={repoUrl}
          onChange={setRepoUrl}
          type="url"
          isRequired
          description="HTTPS clone URL, without credentials."
        />
        <TextField
          label="Refs to build"
          value={refs}
          onChange={setRefs}
          isRequired
          description="Full refs separated by commas; * and ? are wildcards, for example refs/heads/main, refs/tags/v*."
        />
        <Select
          label="Webhook verification"
          value={scheme}
          onChange={(v) => setScheme(v as "hmac-sha256" | "gitlab-token")}
          options={[
            {
              id: "hmac-sha256",
              label: "HMAC signature (GitHub, Gitea, Forgejo)",
            },
            { id: "gitlab-token", label: "GitLab secret token" },
          ]}
        />
        <TextField
          label="Username (optional)"
          value={username}
          onChange={setUsername}
        />
        <TextField
          label="Read-only access token (optional)"
          value={token}
          onChange={setToken}
          type="password"
          description="Stored on the server for fetching only and never shown again."
        />
        <Checkbox isSelected={allowPrivate} onChange={setAllowPrivate}>
          Allow private network addresses (internal Git servers only)
        </Checkbox>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Create pipeline</Button>
        </div>
      </Form>
    </Dialog>
  );
}
