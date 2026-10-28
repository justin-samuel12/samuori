import { useCallback, useEffect, useState } from "react";
import { Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { RadioGroup } from "../components/Field";
import { Alert, Loading, PageHeader } from "../components/Feedback";
import { bytes, shortDigest } from "../format";
import { useLoader } from "../hooks";
import { navigate } from "../router";
import {
  comboKey,
  comboPrefix,
  findingKey,
  frontendBackendPairs,
} from "../spec/model";
import { waitFor } from "../upload/uploader";
import { DeleteUploadDialog, isUploader } from "../components/DeleteUpload";
import { ReadinessList } from "../components/Readiness";
import { useEgressRequests } from "../components/EgressAccess";
import { useMe } from "../session/session";

type Discovery = Schemas["Discovery"];
type Finding = Schemas["DetectorFinding"];

const kindText: Record<string, string> = {
  "http-service": "Web service or API",
  "static-site": "Static website",
  worker: "Background worker",
  "cli-task": "Command-line tool or task",
  database: "Database",
  other: "Other",
};

const confidenceText: Record<string, string> = {
  high: "Detected",
  medium: "Likely — please confirm",
  low: "Unsure — please tell us",
};

export function SnapshotPage({
  projectId,
  snapshotId,
}: {
  projectId: string;
  snapshotId: string;
}) {
  const me = useMe();
  const [deleting, setDeleting] = useState(false);
  const [snap, setSnap] = useState<Schemas["SourceSnapshot"] | null>();
  const [discovery, setDiscovery] = useState<Discovery | null>();
  const [error, setError] = useState<string>();
  const [running, setRunning] = useState(false);

  const load = useCallback(async () => {
    const [s, list] = await Promise.all([
      api.GET("/snapshots/{snapshotId}", { params: { path: { snapshotId } } }),
      api.GET("/snapshots/{snapshotId}/discoveries", {
        params: { path: { snapshotId } },
      }),
    ]);
    setSnap(s.data ?? null);
    const latest = list.data?.items[0];
    if (!latest) {
      setDiscovery(null);
      return;
    }
    const d = await api.GET("/discoveries/{discoveryId}", {
      params: { path: { discoveryId: latest.id } },
    });
    setDiscovery(d.data ?? null);
  }, [snapshotId]);
  useLoader(load);

  useEffect(() => {
    if (discovery?.status !== "running") return;
    const t = setTimeout(() => void load(), 1000);
    return () => clearTimeout(t);
  }, [discovery, load]);

  async function analyse() {
    setRunning(true);
    setError(undefined);
    const { data, error } = await api.POST(
      "/snapshots/{snapshotId}/discoveries",
      { params: { path: { snapshotId } } },
    );
    if (data) await waitFor(data.id);
    else setError(describeError(error));
    setRunning(false);
    await load();
  }

  if (snap === undefined) return <Loading />;
  if (snap === null)
    return (
      <Alert>
        This upload does not exist, or you do not have access to it.
      </Alert>
    );
  const warnings = snap.warnings ?? [];
  return (
    <>
      <nav aria-label="Breadcrumb" className="samuori-breadcrumb">
        <Link href="/projects" className="samuori-link">
          Projects
        </Link>{" "}
        /{" "}
        <Link href={`/projects/${projectId}`} className="samuori-link">
          Project
        </Link>{" "}
        / Upload {shortDigest(snap.digest)}
      </nav>
      <PageHeader
        help="upload-source"
        title="Uploaded source"
        description={`Uploaded ${new Date(snap.createdAt).toLocaleString()}. This copy never changes; upload again to use newer files.`}
        actions={
          !snap.deletedAt && isUploader(snap, me.principal.id) ? (
            <Button variant="secondary" onPress={() => setDeleting(true)}>
              Delete upload
            </Button>
          ) : undefined
        }
      />
      <DeleteUploadDialog
        snapshot={deleting ? snap : undefined}
        onClose={() => setDeleting(false)}
        onDone={() => navigate(`/projects/${projectId}`)}
      />
      {snap.deletedAt && (
        <Alert tone="warning">
          This upload was deleted on {new Date(snap.deletedAt).toLocaleString()}{" "}
          by the person who uploaded it. Its files are gone; builds made from it
          stay in the history. Upload the source again to build.
        </Alert>
      )}
      <dl className="samuori-details">
        <dt>Files</dt>
        <dd>
          {snap.fileCount} ({bytes(snap.totalBytes)})
        </dd>
        <dt>Left out</dt>
        <dd>{snap.excludedEntries ?? 0} by exclusion rules</dd>
        <dt>Refused</dt>
        <dd>
          {snap.rejectedEntries ?? 0} links, special files or unsafe paths
        </dd>
        <dt>Content ID</dt>
        <dd>
          <code>{snap.digest}</code>
        </dd>
      </dl>
      {warnings.length > 0 && (
        <section className="samuori-section" aria-labelledby="warn-h">
          <h2 id="warn-h">Things to check</h2>
          <p className="samuori-muted">
            These checks are best effort and cannot prove the files are safe.
          </p>
          <ul className="samuori-warning-list">
            {warnings.map((w, i) => (
              <li key={i}>
                {w.message} {w.path && <code>{w.path}</code>}
              </li>
            ))}
          </ul>
        </section>
      )}
      <section className="samuori-section" aria-labelledby="disc-h">
        <div className="samuori-section-head">
          <h2 id="disc-h">What we found</h2>
          <Button
            variant="secondary"
            onPress={() => void analyse()}
            isDisabled={running || discovery?.status === "running"}
          >
            {discovery ? "Analyse again" : "Analyse source"}
          </Button>
        </div>
        {error && <Alert>{error}</Alert>}
        {(running || discovery?.status === "running") && (
          <Loading label="Looking at your files…" />
        )}
        {discovery === null && !running && (
          <p>This upload has not been analysed yet.</p>
        )}
        {discovery?.status === "failed" && (
          <Alert>{discovery.error?.message ?? "Analysis failed."}</Alert>
        )}
        {discovery?.status === "completed" && (
          <DiscoveryResults d={discovery} />
        )}
      </section>
    </>
  );
}

function DiscoveryResults({ d }: { d: Discovery }) {
  const egress = useEgressRequests(d.projectId);
  const findings = d.findings ?? [];
  const apps = findings.filter((f) => f.applicationKind);
  const [choice, setChoice] = useState(
    apps.length === 1 ? key(apps[0] as Finding) : "",
  );
  return (
    <>
      {(d.warnings ?? []).map((w, i) => (
        <Alert
          key={i}
          tone={w.code === "sandbox-degraded" ? "warning" : "info"}
        >
          {w.message}
        </Alert>
      ))}
      {apps.length > 1 && (
        <RadioGroup
          label="Which application do you want to build?"
          value={choice}
          onChange={setChoice}
          options={[
            ...frontendBackendPairs(apps).map(({ fe, be }) => ({
              id: comboKey(fe, be),
              label: `${fe.sourceSubpath} + ${be.sourceSubpath} in one image`,
              description:
                "Samuori's web launcher serves the frontend and forwards /api to the backend",
            })),
            ...apps.map((f) => ({
              id: key(f),
              label:
                f.sourceSubpath === "." ? "The whole folder" : f.sourceSubpath,
              description: `${f.framework ?? f.language} ${kindText[f.applicationKind ?? ""]?.toLowerCase() ?? ""}`,
            })),
          ]}
        />
      )}
      <div className="samuori-findings">
        {findings.map((f) => (
          <FindingCard
            key={key(f)}
            f={f}
            selected={
              choice === key(f) ||
              (choice.startsWith(comboPrefix) &&
                choice.slice(comboPrefix.length).split("|").includes(key(f)))
            }
          />
        ))}
      </div>
      {(d.dockerfiles ?? []).map((df) => (
        <article
          key={df.path}
          className="samuori-card"
          aria-label={`Dockerfile ${df.path}`}
        >
          <h3>
            Dockerfile <code>{df.path}</code>
          </h3>
          <p>
            Samuori can build this Dockerfile exactly as written instead of
            generating one from a recipe.
          </p>
          <dl className="samuori-details">
            <dt>Stages</dt>
            <dd>{df.stages.join(", ") || "—"}</dd>
            <dt>Base images</dt>
            <dd>{(df.baseImages ?? []).join(", ") || "—"}</dd>
            <dt>Secrets used</dt>
            <dd>{df.secretMounts.map((m) => m.id).join(", ") || "None"}</dd>
            {(df.exposedPorts ?? []).length > 0 && (
              <>
                <dt>Ports</dt>
                <dd>{df.exposedPorts?.join(", ")}</dd>
              </>
            )}
          </dl>
          {(df.refusedFeatures ?? []).map((w, i) => (
            <Alert key={`r${i}`}>{w.message}</Alert>
          ))}
          {(df.ambiguities ?? []).map((w, i) => (
            <Alert key={`a${i}`} tone="warning">
              {w.message}
            </Alert>
          ))}
          <ReadinessList
            items={df.readiness}
            access={
              d.projectId
                ? {
                    projectId: d.projectId,
                    method: "dockerfile",
                    evidence: { discoveryId: d.id },
                    requests: egress.items,
                    onRequested: () => void egress.reload(),
                  }
                : undefined
            }
          />
          {d.projectId && (
            <div className="samuori-actions samuori-actions--start">
              <Button
                variant="secondary"
                onPress={() =>
                  navigate(
                    `/projects/${d.projectId}/new-image?snapshot=${d.snapshotId}&dockerfile=${encodeURIComponent(df.path)}`,
                  )
                }
              >
                Build with this Dockerfile
              </Button>
            </div>
          )}
        </article>
      ))}
      {d.projectId && (
        <div className="samuori-actions samuori-actions--start">
          <Button
            onPress={() =>
              navigate(
                `/projects/${d.projectId}/new-image?snapshot=${d.snapshotId}${choice ? `&app=${encodeURIComponent(choice)}` : ""}`,
              )
            }
          >
            {choice ? "Build this app" : "Continue: describe your app"}
          </Button>
        </div>
      )}
    </>
  );
}

const key = findingKey;

function FindingCard({ f, selected }: { f: Finding; selected: boolean }) {
  return (
    <article
      className={`samuori-card${selected ? " samuori-card--selected" : ""}`}
      aria-label={`${f.language} in ${f.sourceSubpath}`}
    >
      <h3>
        {[f.framework, f.language].filter(Boolean).join(" · ")}{" "}
        {f.sourceSubpath !== "." && <code>{f.sourceSubpath}</code>}
      </h3>
      <p>
        <span
          className={`samuori-confidence samuori-confidence--${f.confidence}`}
        >
          {confidenceText[f.confidence]}
        </span>
        {f.applicationKind && <> — {kindText[f.applicationKind]}</>}
      </p>
      <dl className="samuori-details">
        {f.packageManager && (
          <>
            <dt>Package manager</dt>
            <dd>{f.packageManager}</dd>
          </>
        )}
        {Object.entries(f.runtimeConstraints ?? {}).map(([k, v]) => (
          <div key={k} className="samuori-contents">
            <dt>{k === "modules" ? "Modules" : `${k} version`}</dt>
            <dd>{v}</dd>
          </div>
        ))}
        {Object.entries(f.suggestedCommands ?? {}).map(([k, v]) => (
          <div key={k} className="samuori-contents">
            <dt>{k.charAt(0).toUpperCase() + k.slice(1)} command</dt>
            <dd>
              <code>{v}</code>
            </dd>
          </div>
        ))}
        {(f.requiredPlatforms ?? []).length > 0 && (
          <>
            <dt>Needs</dt>
            <dd>{f.requiredPlatforms?.join(", ")}</dd>
          </>
        )}
      </dl>
      {(f.warnings ?? []).map((w, i) => (
        <Alert key={i} tone="warning">
          {w.message}
        </Alert>
      ))}
      <ReadinessList items={f.readiness} />
      <details className="samuori-details-toggle">
        <summary>Why we think so ({f.evidence.length})</summary>
        <ul>
          {f.evidence.map((e, i) => (
            <li key={i}>
              {e.observation} <code>{e.path}</code>
              {e.weight < 0 && <strong> (conflicts)</strong>}
            </li>
          ))}
        </ul>
      </details>
    </article>
  );
}
