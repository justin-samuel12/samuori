import { useMemo, useState } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "./Button";
import { Dialog } from "./Dialog";
import { Alert, Loading } from "./Feedback";
import { Select, TextField } from "./Field";

type Info = Schemas["ProjectDeploymentFiles"];
type Component = Schemas["ProjectDeploymentComponent"];
type Plan = Schemas["ProjectDeploymentPlan"];

interface Choice {
  artifactId?: string;
  publicationId?: string;
  replicas: string;
  links: { envName: string; target: string }[];
  volumes: { name: string; mountPath: string }[];
}

const emptyChoice = (): Choice => ({ replicas: "", links: [], volumes: [] });

async function fetchInfo(projectId: string, plan?: string): Promise<Info> {
  const { data, error } = await api.GET("/projects/{projectId}/deploy", {
    params: { path: { projectId }, query: plan ? { plan } : {} },
  });
  if (!data) throw new Error(describeError(error));
  return data;
}

/** Starting Compose and Helm files that run every image of a project (ADR 0061). Hidden until an image is built. */
export function ProjectDeploymentFiles({ projectId }: { projectId: string }) {
  const [open, setOpen] = useState(false);
  const { data } = useQuery({
    queryKey: ["project-deploy", projectId, ""],
    queryFn: () => fetchInfo(projectId),
  });
  if (!data?.components.some((c) => c.available)) return null;
  return (
    <>
      <Button variant="secondary" onPress={() => setOpen(true)}>
        Deployment files
      </Button>
      {open && (
        <ProjectDeploymentDialog
          projectId={projectId}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

function toPlan(choices: Record<string, Choice>): string {
  const components: NonNullable<Plan["components"]> = [];
  for (const [specId, c] of Object.entries(choices)) {
    const replicas = c.replicas.trim() === "" ? undefined : Number(c.replicas);
    components.push({
      specId,
      artifactId: c.artifactId,
      publicationId: c.publicationId,
      replicas: Number.isInteger(replicas) ? replicas : undefined,
      links: c.links.filter((l) => l.envName && l.target),
      volumes: c.volumes.filter((v) => v.name && v.mountPath),
    });
  }
  return components.length ? JSON.stringify({ components }) : "";
}

function ProjectDeploymentDialog({
  projectId,
  onClose,
}: {
  projectId: string;
  onClose: () => void;
}) {
  const [choices, setChoices] = useState<Record<string, Choice>>({});
  const plan = useMemo(() => toPlan(choices), [choices]);
  // The plan-free answer lays out the form; the answer for the current plan checks it and gives each reference.
  const baseQuery = useQuery({
    queryKey: ["project-deploy", projectId, ""],
    queryFn: () => fetchInfo(projectId),
  });
  const query = useQuery({
    queryKey: ["project-deploy", projectId, plan],
    queryFn: () => fetchInfo(projectId, plan),
    placeholderData: keepPreviousData,
    retry: false,
  });
  const info = baseQuery.data;
  const checked = query.isError ? undefined : query.data;
  const failed = query.isError && !query.isFetching;
  const update = (specId: string, change: (c: Choice) => Choice) =>
    setChoices((all) => ({
      ...all,
      [specId]: change(all[specId] ?? emptyChoice()),
    }));
  const targets =
    info?.components
      .filter((c) => c.available && c.linkable)
      .map((c) => ({ id: c.specId, label: c.name ?? c.specName })) ?? [];
  const suffix = plan ? `?plan=${encodeURIComponent(plan)}` : "";
  const base = `/api/v1/projects/${projectId}/deploy`;

  return (
    <Dialog
      title="Deployment files"
      size="wide"
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <div className="grid gap-4">
        <p className="samuori-muted m-0">
          Starting files that run every image of this project together. Review
          them before use; Samuori does not deploy anything. Nothing you choose
          here is saved.
        </p>
        <p className="samuori-muted m-0 text-sm">
          A link sets an environment variable to another image&apos;s address
          inside the deployment, such as http://api:8080. A volume keeps data
          across restarts; set its size in the Helm values. Rows with an empty
          field are left out.
        </p>
        {baseQuery.isPending && <Loading />}
        {baseQuery.isError && <Alert>{describeError(baseQuery.error)}</Alert>}
        {info?.components.map((c) => (
          <ComponentChoice
            key={c.specId}
            component={c}
            resolved={checked?.components.find((x) => x.specId === c.specId)}
            choice={choices[c.specId] ?? emptyChoice()}
            targets={targets.filter((t) => t.id !== c.specId)}
            onChange={(change) => update(c.specId, change)}
          />
        ))}
        {failed && <Alert>{describeError(query.error)}</Alert>}
        {info && (
          <div className="samuori-actions samuori-actions--start">
            {failed ? (
              <>
                <Button variant="secondary" isDisabled>
                  Docker Compose ({info.composeFilename})
                </Button>
                <Button variant="secondary" isDisabled>
                  Helm chart (.tgz)
                </Button>
              </>
            ) : (
              <>
                <a
                  className="samuori-button samuori-button--secondary no-underline"
                  href={`${base}/compose${suffix}`}
                  download={info.composeFilename}
                >
                  Docker Compose ({info.composeFilename})
                </a>
                <a
                  className="samuori-button samuori-button--secondary no-underline"
                  href={`${base}/helm${suffix}`}
                  download={info.chartFilename}
                >
                  Helm chart (.tgz)
                </a>
              </>
            )}
          </div>
        )}
        <div className="samuori-actions">
          <Button onPress={onClose}>Close</Button>
        </div>
      </div>
    </Dialog>
  );
}

function ComponentChoice({
  component: c,
  resolved,
  choice,
  targets,
  onChange,
}: {
  component: Component;
  resolved?: Component;
  choice: Choice;
  targets: { id: string; label: string }[];
  onChange: (change: (c: Choice) => Choice) => void;
}) {
  const label = c.name ?? c.specName;
  if (!c.available)
    return (
      <section className="grid gap-1">
        <h3 className="m-0 text-base">{c.specName}</h3>
        <p className="samuori-muted m-0 text-sm">{c.note}</p>
      </section>
    );
  const artifactId = choice.artifactId ?? c.artifactId;
  const build = c.builds.find((b) => b.artifactId === artifactId);
  const publications = build?.publications ?? [];
  const current = resolved?.artifactId === artifactId ? resolved : undefined;
  return (
    <section className="grid gap-3 border-t border-line pt-3">
      <h3 className="m-0 text-base">{label}</h3>
      <div className="grid gap-3 sm:grid-cols-3">
        {c.builds.length > 1 && (
          <Select
            label="Build"
            value={artifactId ?? null}
            onChange={(artifactId) =>
              onChange((x) => ({ ...x, artifactId, publicationId: undefined }))
            }
            options={c.builds.map((b) => ({
              id: b.artifactId,
              label: `${b.version ?? "Build"} · ${new Date(b.builtAt).toLocaleString()}`,
            }))}
          />
        )}
        {publications.length > 1 && (
          <Select
            label="Publication"
            value={
              choice.publicationId ??
              current?.publicationId ??
              publications[0]?.publicationId ??
              null
            }
            onChange={(publicationId) =>
              onChange((x) => ({ ...x, publicationId }))
            }
            options={publications.map((p) => ({
              id: p.publicationId,
              label: `${p.destinationName ?? p.reference} · ${new Date(p.publishedAt).toLocaleString()}`,
            }))}
          />
        )}
        {c.type !== "cli-task" && (
          <TextField
            label="Replicas"
            type="number"
            inputMode="numeric"
            placeholder="1"
            description="0 to 20"
            value={choice.replicas}
            onChange={(replicas) => onChange((x) => ({ ...x, replicas }))}
          />
        )}
      </div>
      {current && (
        <p className="m-0 text-sm">
          {current.note} <code className="break-all">{current.reference}</code>
        </p>
      )}
      {choice.links.map((l, i) => (
        <div key={`link-${i}`} className="grid items-end gap-3 sm:grid-cols-3">
          <TextField
            label="Environment variable"
            placeholder="API_URL"
            value={l.envName}
            onChange={(envName) =>
              onChange((x) => ({
                ...x,
                links: x.links.map((y, j) => (j === i ? { ...y, envName } : y)),
              }))
            }
          />
          <Select
            label="Points at"
            value={l.target || null}
            onChange={(target) =>
              onChange((x) => ({
                ...x,
                links: x.links.map((y, j) => (j === i ? { ...y, target } : y)),
              }))
            }
            options={targets}
          />
          <Button
            variant="quiet"
            onPress={() =>
              onChange((x) => ({
                ...x,
                links: x.links.filter((_, j) => j !== i),
              }))
            }
          >
            Remove link
          </Button>
        </div>
      ))}
      {choice.volumes.map((v, i) => (
        <div
          key={`volume-${i}`}
          className="grid items-end gap-3 sm:grid-cols-3"
        >
          <TextField
            label="Volume name"
            placeholder="data"
            value={v.name}
            onChange={(name) =>
              onChange((x) => ({
                ...x,
                volumes: x.volumes.map((y, j) =>
                  j === i ? { ...y, name } : y,
                ),
              }))
            }
          />
          <TextField
            label="Mount path"
            placeholder="/var/lib/data"
            value={v.mountPath}
            onChange={(mountPath) =>
              onChange((x) => ({
                ...x,
                volumes: x.volumes.map((y, j) =>
                  j === i ? { ...y, mountPath } : y,
                ),
              }))
            }
          />
          <Button
            variant="quiet"
            onPress={() =>
              onChange((x) => ({
                ...x,
                volumes: x.volumes.filter((_, j) => j !== i),
              }))
            }
          >
            Remove volume
          </Button>
        </div>
      ))}
      <div className="samuori-actions samuori-actions--start">
        {targets.length > 0 && (
          <Button
            variant="quiet"
            onPress={() =>
              onChange((x) => ({
                ...x,
                links: [...x.links, { envName: "", target: "" }],
              }))
            }
          >
            Add link
          </Button>
        )}
        <Button
          variant="quiet"
          onPress={() =>
            onChange((x) => ({
              ...x,
              volumes: [...x.volumes, { name: "", mountPath: "" }],
            }))
          }
        >
          Add volume
        </Button>
      </div>
    </section>
  );
}
