import { useCallback, useEffect, useRef, useState } from "react";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { TextField } from "../components/Field";
import { EgressAccess, useEgressRequests } from "../components/EgressAccess";
import { Alert, Badge, Loading, PageHeader } from "../components/Feedback";
import { Table } from "../components/Table";
import { useLoader } from "../hooks";
import { navigate } from "../router";
import { useCan, useCanDownload } from "../session/session";
import { badgeTone, stateText, terminal } from "./builds";

type Run = Schemas["BuildRun"];
type Attempt = Schemas["BuildAttempt"];

interface TimelineEntry {
  seq: number;
  state: string;
  reason?: string;
}

export function BuildPage({ id }: { id: string }) {
  const can = useCan();
  const mayDownload = useCanDownload();
  const [run, setRun] = useState<Run | null>();
  const [attempts, setAttempts] = useState<Attempt[]>([]);
  const [timeline, setTimeline] = useState<TimelineEntry[]>([]);
  const [confirm, setConfirm] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteReason, setDeleteReason] = useState("");
  const [error, setError] = useState<string>();
  const reloadedFor = useRef<string | null | undefined>(null);

  const load = useCallback(async () => {
    const [r, a] = await Promise.all([
      api.GET("/builds/{buildId}", { params: { path: { buildId: id } } }),
      api.GET("/builds/{buildId}/attempts", {
        params: { path: { buildId: id } },
      }),
    ]);
    setRun(r.data ?? null);
    setAttempts(a.data?.items ?? []);
  }, [id]);
  useLoader(load);

  useEffect(() => {
    const source = new EventSource(`/api/v1/builds/${id}/events`);
    source.addEventListener("state", (ev) => {
      const e = ev as MessageEvent<string>;
      try {
        const d = JSON.parse(e.data) as { state: string; reason?: string };
        setTimeline((t) =>
          t.some((x) => x.seq === Number(e.lastEventId))
            ? t
            : [
                ...t,
                {
                  seq: Number(e.lastEventId),
                  state: d.state,
                  reason: d.reason,
                },
              ],
        );
        void load();
      } catch {
        // Malformed events are ignored; the run summary is reloaded from the API.
      }
    });
    source.onerror = () => source.close();
    return () => source.close();
  }, [id, load]);

  async function cancel() {
    setConfirm(false);
    const { error } = await api.POST("/builds/{buildId}/cancel", {
      params: {
        path: { buildId: id },
        header: { "Idempotency-Key": crypto.randomUUID() },
      },
    });
    if (error) setError(describeError(error));
    await load();
  }

  async function remove() {
    const { error } = await api.POST("/builds/{buildId}/deletion", {
      params: { path: { buildId: id } },
      body: { reason: deleteReason || undefined },
    });
    setDeleting(false);
    if (error) setError(describeError(error));
    else void load();
  }

  async function rebuild() {
    if (!run) return;
    const { data, error } = await api.POST("/builds", {
      params: { header: { "Idempotency-Key": crypto.randomUUID() } },
      body: {
        specId: run.specId,
        specRevision: run.specRevision,
        snapshotId: run.snapshotId,
        cleanBuild: false,
      },
    });
    if (data) navigate(`/builds/${data.id}`);
    else setError(describeError(error, "The build could not be started."));
  }

  if (run === undefined) return <Loading />;
  if (run === null)
    return (
      <Alert>This build does not exist, or you do not have access to it.</Alert>
    );
  const finished = terminal.has(run.state);
  const downloadable =
    !run.deletion &&
    run.state === "succeeded" &&
    !!run.artifactId &&
    run.validation?.verdict === "passed" &&
    mayDownload;
  return (
    <>
      <PageHeader
        help="builds"
        title="Build"
        crumbs={[
          { label: "Build runs", href: "/builds" },
          { label: new Date(run.createdAt).toLocaleString() },
        ]}
        description={`Spec revision ${run.specRevision}, started ${new Date(run.createdAt).toLocaleString()}.`}
        actions={
          finished ? (
            <>
              {downloadable && (
                <a
                  className="samuori-button samuori-button--secondary no-underline"
                  href={`/api/v1/artifacts/${run.artifactId}/download`}
                  download
                >
                  Download image
                </a>
              )}
              {can("createBuild") && (
                <Button variant="secondary" onPress={() => void rebuild()}>
                  Build again
                </Button>
              )}
              {can("deleteBuild") && !run.deletion && (
                <Button variant="danger" onPress={() => setDeleting(true)}>
                  Delete build
                </Button>
              )}
            </>
          ) : (
            can("cancelBuild") && (
              <Button variant="danger" onPress={() => setConfirm(true)}>
                Cancel build
              </Button>
            )
          )
        }
      />
      {error && <Alert>{error}</Alert>}
      {run.deletion && (
        <Alert tone="info">
          <strong>Deleted</strong> by {run.deletion.deletedBy} on{" "}
          {new Date(run.deletion.deletedAt).toLocaleString()}
          {run.deletion.reason ? `: ${run.deletion.reason}` : "."} The image,
          logs and cache were removed; the build, its checks and its history
          remain.
        </Alert>
      )}
      <div className="samuori-card mb-6">
        <div className="flex flex-wrap items-center gap-3">
          <span className="text-sm text-muted">Status</span>
          <Badge
            tone={badgeTone(run.state)}
            live={!finished}
            testId="run-state"
          >
            {stateText[run.state] ?? run.state}
          </Badge>
        </div>
        <dl className="mt-4 mb-0 grid grid-cols-2 gap-4 border-t border-line pt-4 lg:grid-cols-5">
          {[
            [
              "Isolation",
              attempts.length > 0
                ? (run.isolationProfile ?? "—")
                : "Chosen when a worker takes it",
            ],
            ["Attempts", `${run.attemptsUsed ?? 0} of ${run.maxAttempts ?? 0}`],
            ["Version", run.version ?? "Given when the build succeeds"],
            ["Spec revision", String(run.specRevision)],
            ["Started", new Date(run.createdAt).toLocaleString()],
          ].map(([k, v]) => (
            <div key={k} className="min-w-0">
              <dt className="text-xs font-semibold tracking-wider text-muted uppercase">
                {k}
              </dt>
              <dd className="m-0 mt-1 truncate text-sm font-medium">{v}</dd>
            </div>
          ))}
        </dl>
      </div>
      {run.state === "blocked" && (
        <Alert tone="warning">
          <strong>This build cannot run yet.</strong> {run.blockedReason}
        </Alert>
      )}
      {run.state === "failed" && (
        <Alert>
          The build failed (
          {run.failureClass === "infrastructure"
            ? "a problem with the build infrastructure"
            : "a problem in the application build"}
          ). See the attempts below.
        </Alert>
      )}
      {attempts[0] && (
        <EgressDenials
          attempt={attempts[0]}
          failed={run.state === "failed"}
          run={run}
        />
      )}
      <section className="samuori-card" aria-labelledby="digests-h">
        <h2 id="digests-h">Inputs and output</h2>
        <dl className="samuori-details mb-0">
          <dt>Source</dt>
          <dd>
            <code>{run.snapshotDigest}</code>
          </dd>
          <dt>Spec</dt>
          <dd>
            <code>{run.specDigest}</code>
          </dd>
          {run.planDigest && (
            <>
              <dt>Build plan</dt>
              <dd>
                <code>{run.planDigest}</code>
              </dd>
            </>
          )}
          {(run.baseImagePins ?? []).length > 0 && (
            <>
              <dt>Base images</dt>
              <dd>
                <ul className="samuori-plain-list">
                  {run.baseImagePins?.map((p) => (
                    <li key={p.ref}>
                      <code>{p.ref}</code> → <code>{p.image}</code> (pinned at
                      admission)
                    </li>
                  ))}
                </ul>
              </dd>
            </>
          )}
          <MetadataRows term="Image labels" values={run.imageLabels} />
          <MetadataRows
            term="Image annotations"
            values={run.imageAnnotations}
          />
          {run.artifactDigest && (
            <>
              <dt>Image</dt>
              <dd>
                <code>{run.artifactDigest}</code>
              </dd>
            </>
          )}
        </dl>
      </section>
      <section className="samuori-section" aria-labelledby="timeline-h">
        <h2 id="timeline-h">Timeline</h2>
        <ol className="samuori-timeline" aria-live="polite">
          {timeline.map((t) => (
            <li key={t.seq}>
              {stateText[t.state] ?? t.state}
              {t.reason ? ` — ${t.reason}` : ""}
            </li>
          ))}
        </ol>
      </section>
      <section className="samuori-section" aria-labelledby="attempts-h">
        <h2 id="attempts-h">Attempts</h2>
        <Table
          label="Attempts"
          rows={attempts}
          empty="No attempt has started."
          columns={[
            {
              id: "n",
              name: "Attempt",
              isRowHeader: true,
              render: (a) => `#${a.number}`,
            },
            { id: "state", name: "State", render: (a) => a.state },
            {
              id: "detail",
              name: "Details",
              render: (a) =>
                a.detail ? `${a.failureClass ?? ""}: ${a.detail}` : "",
            },
            {
              id: "worker",
              name: "Worker",
              render: (a) => <code>{a.workerId}</code>,
            },
            {
              id: "cleanup",
              name: "Environment",
              render: (a) => a.cleanup?.outcome ?? "pending",
            },
            {
              id: "started",
              name: "Started",
              render: (a) =>
                a.startedAt ? new Date(a.startedAt).toLocaleTimeString() : "—",
            },
          ]}
        />
      </section>
      {run.validation && (
        <ValidationSection
          id={run.validation.id}
          onVerdict={(verdict) => {
            if (
              verdict === run.validation?.verdict ||
              reloadedFor.current === verdict
            )
              return;
            reloadedFor.current = verdict;
            void load();
          }}
        />
      )}
      <RailpackSection key={`rp-${String(finished)}`} id={id} />
      <BuildpacksSection key={`cnb-${String(finished)}`} id={id} />
      {!run.deletion && (
        <LogsSection key={String(finished)} id={id} finished={finished} />
      )}
      <Dialog
        title="Delete this build?"
        size="confirm"
        isOpen={deleting}
        onOpenChange={setDeleting}
      >
        <p>
          This removes the build&apos;s image, logs and cache. The build, its
          checks and its history stay, marked as deleted. It cannot be undone.
        </p>
        <TextField
          label="Reason (optional)"
          value={deleteReason}
          onChange={setDeleteReason}
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => setDeleting(false)}>
            Keep it
          </Button>
          <Button variant="danger" onPress={() => void remove()}>
            Delete build
          </Button>
        </div>
      </Dialog>
      <Dialog
        title="Cancel this build?"
        size="confirm"
        isOpen={confirm}
        onOpenChange={setConfirm}
      >
        <p>The worker stops the build and destroys its environment.</p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => setConfirm(false)}>
            Keep building
          </Button>
          <Button variant="danger" onPress={() => void cancel()}>
            Cancel build
          </Button>
        </div>
      </Dialog>
    </>
  );
}

type Validation = Schemas["ValidationRun"];

const verdictText: Record<string, string> = {
  passed: "Passed",
  failed: "Failed",
  blocked: "Blocked",
  incomplete: "Incomplete",
};

const vulnOutcome: Record<string, string> = {
  fail: "Fails policy",
  report: "Reported",
  ignored: "Ignored by policy",
};

const verdictTone: Record<string, string> = {
  passed: "ok",
  failed: "error",
  blocked: "error",
  incomplete: "warning",
};

function ValidationSection({
  id: first,
  onVerdict,
}: {
  id: string;
  // The page header offers actions, such as downloading, that depend on the verdict.
  onVerdict: (verdict: Validation["verdict"]) => void;
}) {
  const can = useCan();
  const [id, setId] = useState(first);
  const [v, setV] = useState<Validation | null>();
  const [error, setError] = useState<string>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/validations/{validationId}", {
      params: { path: { validationId: id } },
    });
    setV(data ?? null);
  }, [id]);

  // Runs every check again on the same image, for example after an administrator changed a policy or scanner.
  async function revalidate() {
    if (!v) return;
    setError(undefined);
    const { data, error } = await api.POST(
      "/artifacts/{artifactId}/validations",
      {
        params: {
          path: { artifactId: v.artifactId },
          header: { "Idempotency-Key": crypto.randomUUID() },
        },
      },
    );
    if (data) {
      setV(data);
      setId(data.id);
    } else setError(describeError(error, "Validation could not be started."));
  }
  useLoader(load);
  useEffect(() => {
    if (v && v.state !== "pending" && v.state !== "running") return;
    const t = setTimeout(() => void load(), 2000);
    return () => clearTimeout(t);
  }, [v, load]);
  const verdict = v?.verdict;
  useEffect(() => {
    if (verdict) onVerdict(verdict);
  }, [verdict, onVerdict]);
  if (v === undefined) return <Loading label="Loading validation…" />;
  if (v === null) return null;
  const findings = v.checks.flatMap((c) =>
    (c.sensitiveFindings ?? []).map((f, i) => ({
      ...f,
      id: `${c.checkId}-${i}`,
      check: c.checkId,
    })),
  );
  const vulns = v.checks.flatMap((c) =>
    (c.vulnerabilities ?? []).map((x, i) => ({
      ...x,
      vulnId: x.id,
      id: `${c.checkId}-v${i}`,
    })),
  );
  return (
    <section className="samuori-section" aria-labelledby="validation-h">
      <h2 id="validation-h">Validation</h2>
      {error && <Alert>{error}</Alert>}
      <p>
        Verdict:{" "}
        <span
          className={`samuori-status samuori-status--${verdictTone[v.verdict ?? ""] ?? "pending"}`}
          data-testid="validation-verdict"
        >
          {v.verdict ? verdictText[v.verdict] : "Checking…"}
        </span>
        {can("startRevalidation") &&
          v.verdict &&
          v.verdict !== "passed" &&
          v.state !== "pending" &&
          v.state !== "running" && (
            <>
              {" "}
              <Button variant="secondary" onPress={() => void revalidate()}>
                Validate again
              </Button>
            </>
          )}
        {v.classification === "restricted" && (
          <>
            {" "}
            <span className="samuori-status samuori-status--warning">
              Restricted: passed only through an approved exception
            </span>
          </>
        )}
      </p>
      <Table
        label="Checks"
        rows={v.checks.map((c) => ({ ...c, id: c.checkId }))}
        empty="No checks have run yet."
        columns={[
          {
            id: "check",
            name: "Check",
            isRowHeader: true,
            render: (c) => c.checkId,
          },
          {
            id: "required",
            name: "Required",
            render: (c) => (c.required ? "Yes" : "No"),
          },
          { id: "outcome", name: "Outcome", render: (c) => c.outcome },
          {
            id: "reason",
            name: "Details",
            render: (c) =>
              [
                c.reason,
                c.tool?.name && `${c.tool.name} ${c.tool.version ?? ""}`,
              ]
                .filter(Boolean)
                .join(" · "),
          },
        ]}
      />
      {vulns.length > 0 && (
        <>
          <h3>Vulnerabilities</h3>
          <Table
            label="Vulnerabilities"
            rows={vulns}
            columns={[
              {
                id: "vuln",
                name: "Vulnerability",
                isRowHeader: true,
                render: (x) => x.vulnId,
              },
              { id: "severity", name: "Severity", render: (x) => x.severity },
              {
                id: "package",
                name: "Package",
                render: (x) => `${x.package} ${x.version}`,
              },
              {
                id: "fixed",
                name: "Fixed in",
                render: (x) => x.fixedVersion ?? "",
              },
              {
                id: "outcome",
                name: "Outcome",
                render: (x) => vulnOutcome[x.outcome],
              },
            ]}
          />
        </>
      )}
      {findings.length > 0 && (
        <>
          <h3>Findings</h3>
          <p className="samuori-muted">
            Findings show where something was found, never its contents.
          </p>
          <Table
            label="Findings"
            rows={findings}
            columns={[
              {
                id: "path",
                name: "Path",
                isRowHeader: true,
                render: (f) => <code>{f.path}</code>,
              },
              { id: "rule", name: "Rule", render: (f) => f.rule },
              {
                id: "outcome",
                name: "Outcome",
                render: (f) =>
                  f.outcome === "excepted" && f.exceptionId
                    ? `excepted (${f.exceptionId})`
                    : f.outcome,
              },
              {
                id: "secret",
                name: "Secret",
                render: (f) => f.logicalSecretId ?? "",
              },
              {
                id: "expected",
                name: "Declared as expected",
                render: (f) => f.expectedReason ?? "",
              },
            ]}
          />
        </>
      )}
    </section>
  );
}

function MetadataRows({
  term,
  values,
}: {
  term: string;
  values?: Record<string, string>;
}) {
  const entries = Object.entries(values ?? {}).sort(([a], [b]) =>
    a.localeCompare(b),
  );
  if (entries.length === 0) return null;
  return (
    <>
      <dt>{term}</dt>
      <dd>
        <ul className="samuori-plain-list">
          {entries.map(([k, v]) => (
            <li key={k}>
              <code>{k}</code> = <code>{v}</code>
            </li>
          ))}
        </ul>
      </dd>
    </>
  );
}

// Denials come from the worker's egress proxy report, never from the build log (docs/architecture/egress.md).
function EgressDenials({
  attempt,
  failed,
  run,
}: {
  attempt: Attempt;
  failed: boolean;
  run: Run;
}) {
  const egress = useEgressRequests(run.projectId);
  const denied = attempt.egressDenied ?? [];
  if (denied.length === 0) return null;
  const requestable = denied
    .filter((d) => d.requestable)
    .map((d) => `${d.host}:${d.port}`);
  return (
    <Alert tone="warning">
      <strong>Network access was blocked during this build.</strong>{" "}
      {failed
        ? "The build may have failed because a step could not reach these destinations."
        : "Steps could not reach these destinations."}
      <ul
        className="mt-2 mb-0 list-disc pl-5"
        aria-label="Blocked destinations"
      >
        {denied.map((d) => (
          <li key={`${d.host}:${d.port}`} className="mt-1">
            <code>
              {d.host}:{d.port}
            </code>{" "}
            ({d.count === 1 ? "1 attempt" : `${d.count} attempts`}): {d.hint}
          </li>
        ))}
      </ul>
      {(attempt.egressUnrecorded ?? 0) > 0 && (
        <p className="mt-2 mb-0">
          {attempt.egressUnrecorded} further network decisions were not
          recorded, so other destinations may also have been blocked.
        </p>
      )}
      <EgressAccess
        context={{
          projectId: run.projectId,
          evidence: { buildId: run.id },
          requests: egress.items,
          onRequested: () => void egress.reload(),
        }}
        presets={[]}
        destinations={requestable}
        internalOnly={denied
          .filter((d) => d.requestable && d.network === "internal")
          .map((d) => `${d.host}:${d.port}`)}
      />
    </Alert>
  );
}

function LogsSection({ id, finished }: { id: string; finished: boolean }) {
  const [text, setText] = useState<string>();
  const [live, setLive] = useState(false);
  const [copied, setCopied] = useState(false);
  const logRef = useRef<HTMLPreElement>(null);
  const load = useCallback(async () => {
    const res = await fetch(`/api/v1/builds/${id}/logs`, {
      credentials: "same-origin",
    });
    const el = logRef.current;
    const atEnd = !el || el.scrollHeight - el.scrollTop - el.clientHeight < 40;
    setText(res.ok ? await res.text() : "");
    setLive(res.headers.get("X-Samuori-Log-Live") === "true");
    if (atEnd)
      requestAnimationFrame(() => {
        const pre = logRef.current;
        if (pre) pre.scrollTop = pre.scrollHeight;
      });
  }, [id]);
  useLoader(load);
  // While the build runs, the log is read again every few seconds; builds without secrets stream their output.
  useEffect(() => {
    if (finished) return;
    const t = window.setInterval(() => void load(), 3000);
    return () => window.clearInterval(t);
  }, [finished, load]);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(t);
  }, [copied]);

  function selectLog() {
    const el = logRef.current;
    const sel = window.getSelection();
    if (!el || !sel) return;
    const range = document.createRange();
    range.selectNodeContents(el);
    sel.removeAllRanges();
    sel.addRange(range);
    el.focus();
  }

  async function copy() {
    if (!text) return;
    try {
      if (!navigator.clipboard) throw new Error("no clipboard");
      await navigator.clipboard.writeText(text);
      setCopied(true);
    } catch {
      selectLog();
    }
  }

  return (
    <section className="samuori-section" aria-labelledby="logs-h">
      <div className="flex items-center justify-between gap-3">
        <h2 id="logs-h" className="flex items-center gap-2">
          Logs
          {live && !finished && (
            <Badge tone="info" live>
              Live
            </Badge>
          )}
        </h2>
        {text && (
          <Button variant="secondary" onPress={() => void copy()}>
            {copied ? "Copied" : "Copy"}
          </Button>
        )}
      </div>
      <span className="sr-only" role="status">
        {copied ? "Log copied to the clipboard" : ""}
      </span>
      {text ? (
        <pre
          ref={logRef}
          className="samuori-log"
          tabIndex={0}
          aria-label="Build log"
        >
          {text}
        </pre>
      ) : (
        <p className="samuori-muted">
          {finished
            ? "No log was stored for this build."
            : "Waiting for the build to start. Output appears here as it runs."}
        </p>
      )}
    </section>
  );
}

type RailpackLineage = Schemas["RailpackLineage"]["items"][number];

interface RailpackInfo {
  railpackVersion?: string;
  detectedProviders?: string[];
  resolvedPackages?: Record<
    string,
    { resolvedVersion?: string; requestedVersion?: string; source?: string }
  >;
  logs?: { Level?: string; Msg?: string }[];
}

interface RailpackPlan {
  steps?: { name?: string }[];
  deploy?: { startCommand?: string };
  secrets?: string[];
}

// RailpackSection shows the plan each attempt built and Railpack's planning report; other methods show nothing.
function RailpackSection({ id }: { id: string }) {
  const [items, setItems] = useState<RailpackLineage[]>([]);
  const load = useCallback(async () => {
    const { data } = await api.GET("/builds/{buildId}/railpack", {
      params: { path: { buildId: id } },
    });
    setItems(data?.items ?? []);
  }, [id]);
  useLoader(load);
  if (items.length === 0) return null;
  return (
    <section className="samuori-section" aria-labelledby="railpack-h">
      <h2 id="railpack-h">Railpack plan</h2>
      {items.map((it) => {
        const info = (it.info ?? {}) as RailpackInfo;
        const plan = (it.plan ?? {}) as RailpackPlan;
        const packages = Object.entries(info.resolvedPackages ?? {});
        return (
          <div key={it.attempt} className="samuori-card">
            <h3>Attempt {it.attempt}</h3>
            <dl className="samuori-details">
              <dt>Railpack</dt>
              <dd>{info.railpackVersion ?? "unknown"}</dd>
              <dt>Detected</dt>
              <dd>{(info.detectedProviders ?? []).join(", ") || "nothing"}</dd>
              {packages.map(([name, p]) => (
                <div key={name} className="contents">
                  <dt>{name}</dt>
                  <dd>
                    {p.resolvedVersion ?? "unresolved"}
                    {p.source ? ` (from ${p.source})` : ""}
                  </dd>
                </div>
              ))}
              <dt>Steps</dt>
              <dd>
                {(plan.steps ?? []).map((s) => s.name).join(", ") ||
                  "no plan stored"}
              </dd>
              <dt>Start command</dt>
              <dd>
                <code>{plan.deploy?.startCommand ?? "none"}</code>
              </dd>
              <dt>Secrets (names only)</dt>
              <dd>{(plan.secrets ?? []).join(", ") || "none"}</dd>
            </dl>
            {(info.logs ?? []).length > 0 && (
              <ul className="samuori-issue-list">
                {(info.logs ?? []).map((l, i) => (
                  <li key={i}>
                    <strong>{l.Level ?? "info"}</strong> {l.Msg}
                  </li>
                ))}
              </ul>
            )}
            {it.plan && (
              <details className="samuori-details-toggle">
                <summary>Show the plan as built (JSON)</summary>
                <pre className="samuori-code">
                  {JSON.stringify(it.plan, null, 2)}
                </pre>
              </details>
            )}
          </div>
        );
      })}
    </section>
  );
}

type BuildpacksLineage = Schemas["BuildpacksLineage"]["items"][number];

interface BuildpacksMetadata {
  builder?: string;
  runImage?: string;
  platformApi?: string;
  lifecycleVersion?: string;
  image?: { user?: string; entrypoint?: string[] };
  buildMetadata?: {
    buildpacks?: { id?: string; version?: string }[];
    processes?: {
      type?: string;
      command?: string[] | string;
      args?: string[];
      default?: boolean;
    }[];
  };
}

function processCommand(p: {
  command?: string[] | string;
  args?: string[];
}): string {
  const cmd = Array.isArray(p.command)
    ? p.command
    : p.command
      ? [p.command]
      : [];
  return [...cmd, ...(p.args ?? [])].join(" ");
}

// BuildpacksSection shows the buildpacks and processes of each attempt's image; other methods show nothing.
function BuildpacksSection({ id }: { id: string }) {
  const [items, setItems] = useState<BuildpacksLineage[]>([]);
  const load = useCallback(async () => {
    const { data } = await api.GET("/builds/{buildId}/buildpacks", {
      params: { path: { buildId: id } },
    });
    setItems(data?.items ?? []);
  }, [id]);
  useLoader(load);
  if (items.length === 0) return null;
  return (
    <section className="samuori-section" aria-labelledby="buildpacks-h">
      <h2 id="buildpacks-h">Buildpacks</h2>
      {items.map((it) => {
        const m = (it.metadata ?? {}) as BuildpacksMetadata;
        const bps = m.buildMetadata?.buildpacks ?? [];
        const procs = m.buildMetadata?.processes ?? [];
        return (
          <div key={it.attempt} className="samuori-card">
            <h3>Attempt {it.attempt}</h3>
            <dl className="samuori-details">
              <dt>Builder</dt>
              <dd>
                <code>{m.builder ?? "unknown"}</code>
              </dd>
              <dt>Run image</dt>
              <dd>
                <code>{m.runImage ?? "unknown"}</code>
              </dd>
              <dt>Lifecycle</dt>
              <dd>
                {m.lifecycleVersion ?? "unknown"}, platform API{" "}
                {m.platformApi ?? "unknown"}
              </dd>
              <dt>Buildpacks used</dt>
              <dd>
                {bps
                  .map((b) => `${b.id ?? "?"} ${b.version ?? ""}`)
                  .join(", ") || "none recorded"}
              </dd>
              <dt>Runs as</dt>
              <dd>{m.image?.user || "the image default"}</dd>
            </dl>
            {procs.length > 0 && (
              <Table
                label={`Processes of attempt ${it.attempt}`}
                rows={procs.map((p, i) => ({ id: String(i), ...p }))}
                columns={[
                  {
                    id: "type",
                    name: "Process",
                    isRowHeader: true,
                    render: (p) =>
                      `${p.type ?? "?"}${p.default ? " (default)" : ""}`,
                  },
                  {
                    id: "command",
                    name: "Command",
                    render: (p) => <code>{processCommand(p)}</code>,
                  },
                ]}
              />
            )}
            {it.sbom && (
              <p>
                <a
                  className="samuori-link"
                  href={`/api/v1/builds/${id}/buildpacks/sbom?attempt=${it.attempt}`}
                  download
                >
                  Download the lifecycle SBOM (tar,{" "}
                  {Math.ceil(it.sbom.sizeBytes / 1024)} KiB)
                </a>
              </p>
            )}
          </div>
        );
      })}
    </section>
  );
}
