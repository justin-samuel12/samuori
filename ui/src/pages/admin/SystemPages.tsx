import { CustomRecipes } from "./CustomRecipes";
import { RecipeRuntimes } from "./RecipeRuntimes";
import { EgressGrants } from "./EgressGrants";
import { WorkerStatus } from "./WorkerStatus";
import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import { Dialog } from "../../components/Dialog";
import { Checkbox, Select, TextField } from "../../components/Field";
import {
  Alert,
  Explain,
  Loading,
  PageHeader,
  Section,
  StateBadge,
} from "../../components/Feedback";
import { recipeStatus } from "../display";
import { Table } from "../../components/Table";
import { bytes } from "../../format";
import { useLoader } from "../../hooks";
import { isOrgAdmin, useMe } from "../../session/session";

export function EnginesPage() {
  const [st, setSt] = useState<Schemas["EngineStatus"] | null>();
  const load = useCallback(
    async () => setSt((await api.GET("/system/engines")).data ?? null),
    [],
  );
  useLoader(load);
  if (st === undefined) return <Loading />;
  if (st === null) return <Alert>Engine status could not be loaded.</Alert>;
  return (
    <>
      <PageHeader
        title="Build engines"
        description="Engines are reported by connected workers; nothing is assumed to be installed."
      />
      <WorkerStatus />
      <Table
        label="Build engines"
        rows={st.engines.map((e) => ({ ...e, id: e.name }))}
        columns={[
          {
            id: "name",
            name: "Engine",
            isRowHeader: true,
            render: (e) => e.name,
          },
          {
            id: "status",
            name: "Status",
            render: (e) => (
              <span
                className={`samuori-status samuori-status--${e.available ? "ok" : "unavailable"}`}
              >
                {e.available
                  ? `Available on ${e.workers} worker(s)`
                  : "Unavailable"}
              </span>
            ),
          },
          { id: "reason", name: "Details", render: (e) => e.reason ?? "" },
        ]}
      />
    </>
  );
}

const backendText: Record<string, string> = {
  local: "Local filesystem (data directory)",
  s3: "S3 or S3-compatible object storage",
  oci: "OCI registry",
};

const classText: Record<string, string> = {
  source: "Source snapshots",
  image: "Built images",
  report: "SBOMs and reports",
  other: "Other",
};

export function StoragePage() {
  const [st, setSt] = useState<Schemas["StorageStatus"] | null>();
  const [gc, setGc] = useState<Schemas["GarbageCollection"]>();
  const [busy, setBusy] = useState(false);
  const load = useCallback(
    async () => setSt((await api.GET("/system/storage")).data ?? null),
    [],
  );
  useLoader(load);
  async function collect() {
    setBusy(true);
    const { data } = await api.POST("/system/storage/collections");
    setGc(data);
    setBusy(false);
    await load();
  }
  if (st === undefined) return <Loading />;
  if (st === null) return <Alert>Storage status could not be loaded.</Alert>;
  return (
    <>
      <PageHeader
        title="Storage"
        description="Source snapshots and build outputs, stored by content digest and verified on every read."
        actions={
          <Button
            variant="secondary"
            onPress={() => void collect()}
            isDisabled={busy}
          >
            Collect garbage now
          </Button>
        }
      />
      <dl className="samuori-details">
        <dt>Backend</dt>
        <dd>
          {st.backend === "mixed"
            ? "Depends on the data class (below)"
            : `${backendText[st.backend]}${st.location ? `, ${st.location}` : ""}`}
        </dd>
        <dt>Objects</dt>
        <dd>
          {st.blobs} ({bytes(st.bytes)})
        </dd>
        <dt>Active holds</dt>
        <dd>{st.held ?? 0}</dd>
        <dt>Measured</dt>
        <dd>{new Date(st.measuredAt).toLocaleString()}</dd>
      </dl>
      {st.routes && (
        <Table
          label="Where each data class is kept"
          rows={st.routes.map((r) => ({ ...r, id: r.class }))}
          columns={[
            {
              id: "class",
              name: "Data",
              isRowHeader: true,
              render: (r) => classText[r.class],
            },
            {
              id: "backend",
              name: "Kept in",
              render: (r) => backendText[r.backend],
            },
            { id: "where", name: "Location", render: (r) => r.location ?? "" },
          ]}
        />
      )}
      {gc && (
        <Alert tone="success">
          Removed {gc.deleted} unreferenced object(s); kept {gc.referenced}{" "}
          referenced and {gc.keptForGrace} recent object(s).
        </Alert>
      )}
      <BuildCaches />
      <p className="samuori-muted">
        Backends and the route for each data class are set in the controller
        configuration. With a remote backend the data directory is a verified
        cache; NFS and EFS are mounted at the data directory. Routing by
        location, capacity or retention is not available yet.
      </p>
    </>
  );
}

type Cache = Schemas["BuildCacheList"]["items"][number];

function BuildCaches() {
  const admin = isOrgAdmin(useMe());
  const [list, setList] = useState<Schemas["BuildCacheList"]>();
  const [error, setError] = useState<string>();
  const load = useCallback(async () => {
    const { data, error } = await api.GET("/build-caches");
    if (data) setList(data);
    else setError(describeError(error));
  }, []);
  useLoader(load);
  const [confirmDialog, ask] = useConfirm();
  async function purge(c: Cache) {
    const { error } = await api.POST("/build-caches/{scope}/purge", {
      params: { path: { scope: c.scope } },
    });
    if (error) return describeError(error);
    await load();
    return undefined;
  }
  return (
    <section className="samuori-section" aria-labelledby="caches-h">
      <h2 id="caches-h">Shared build caches</h2>
      <Explain summary="Builds without secrets reuse their own scope's cache; builds with secrets use none.">
        <p>
          A build without secrets imports the newest cache of its own scope
          (project, trust domain, binding and policy versions) and exports a new
          one. Secret-bearing and clean builds use none. Caches older than{" "}
          {list ? `${list.ttlHours} hours` : "the limit"} or larger than{" "}
          {list ? bytes(list.maxScopeBytes) : "the limit"} are removed.
        </p>
      </Explain>
      {error && <Alert>{error}</Alert>}
      {!list && !error && <Loading />}
      {list && (
        <Table
          label="Shared build caches"
          rows={list.items.map((c) => ({ ...c, id: c.scope }))}
          empty="No shared build caches yet."
          dependencies={[admin]}
          columns={[
            {
              id: "project",
              name: "Project",
              isRowHeader: true,
              render: (c) => c.projectName ?? c.projectId,
            },
            {
              id: "size",
              name: "Size",
              render: (c) => `${bytes(c.bytes)} in ${c.entries} cache(s)`,
            },
            {
              id: "newest",
              name: "Newest",
              render: (c) => new Date(c.newestAt).toLocaleString(),
            },
            {
              id: "used",
              name: "Last used",
              render: (c) =>
                c.lastUsedAt
                  ? new Date(c.lastUsedAt).toLocaleString()
                  : "Never",
            },
            {
              id: "actions",
              name: "Actions",
              render: (c) =>
                admin ? (
                  <Button
                    variant="quiet-danger"
                    onPress={() =>
                      ask({
                        title: `Purge the build cache of ${c.projectName ?? c.projectId}?`,
                        message:
                          "Its next build runs without a cache. The purge is recorded in the audit log.",
                        confirmLabel: "Purge",
                        run: () => purge(c),
                      })
                    }
                  >
                    Purge
                  </Button>
                ) : null,
            },
          ]}
        />
      )}
      {confirmDialog}
    </section>
  );
}

export function MessagingPage() {
  const [st, setSt] = useState<Schemas["MessagingStatus"] | null>();
  const load = useCallback(
    async () => setSt((await api.GET("/system/messaging")).data ?? null),
    [],
  );
  useLoader(load);
  if (st === undefined) return <Loading />;
  if (st === null) return <Alert>Messaging status could not be loaded.</Alert>;
  return (
    <>
      <PageHeader
        title="Messaging"
        description="Embedded NATS JetStream wakes the scheduler; the database stays the source of truth."
      />
      <dl className="samuori-details">
        <dt>Mode</dt>
        <dd>{st.embedded ? "Embedded in the controller" : "External"}</dd>
        <dt>Network listener</dt>
        <dd>
          {st.listensOnNetwork
            ? "Listening (unexpected)"
            : "None; only the controller can connect"}
        </dd>
        <dt>Connection</dt>
        <dd>{st.connected ? "Connected" : "Disconnected"}</dd>
        <dt>Messages in stream</dt>
        <dd>{st.messages}</dd>
        <dt>Outbox waiting to publish</dt>
        <dd>{st.outboxPending}</dd>
      </dl>
      <Table
        label="Consumers"
        rows={st.consumers.map((c) => ({ ...c, id: c.name }))}
        columns={[
          {
            id: "name",
            name: "Consumer",
            isRowHeader: true,
            render: (c) => c.name,
          },
          { id: "pending", name: "Pending", render: (c) => c.pending },
          {
            id: "ack",
            name: "Awaiting acknowledgement",
            render: (c) => c.ackPending,
          },
          {
            id: "redelivered",
            name: "Redelivered",
            render: (c) => c.redelivered,
          },
        ]}
      />
    </>
  );
}

type Recipe = Schemas["Recipe"];

export function RecipesPage() {
  const [recipes, setRecipes] = useState<Recipe[]>();
  const [editing, setEditing] = useState<Recipe>();
  const [viewing, setViewing] = useState<Recipe>();
  const [runtimesOf, setRuntimesOf] = useState<Recipe>();
  const load = useCallback(
    async () => setRecipes((await api.GET("/recipes")).data?.items ?? []),
    [],
  );
  useLoader(load);
  if (!recipes) return <Loading />;
  return (
    <>
      <PageHeader
        title="Recipes & runtimes"
        description="Built-in recipes pin every base image by digest. Deprecated recipes warn; withdrawn recipes are refused."
      />
      <h2>Built-in recipes</h2>
      <Table
        label="Recipes"
        rows={recipes
          .filter((r) => !r.custom)
          .map((r) => ({
            ...r,
            id: `${r.id}@${r.version}`,
            recipeId: r.id,
          }))}
        columns={[
          {
            id: "name",
            name: "Recipe",
            isRowHeader: true,
            render: (r) => `${r.displayName} ${r.version}`,
          },
          {
            id: "runtimes",
            name: "Runtimes",
            render: (r) =>
              r.runtimes
                .map((x) =>
                  x.source === "organization"
                    ? `${x.version} (added)`
                    : x.status === "approved"
                      ? x.version
                      : `${x.version} (${recipeStatus(x.status).text.toLowerCase()})`,
                )
                .join(", "),
          },
          {
            id: "platforms",
            name: "Platforms",
            render: (r) => r.platforms.join(", "),
          },
          {
            id: "status",
            name: "Status",
            render: (r) => (
              <>
                <StateBadge state={recipeStatus(r.status)} />
                {r.statusReason && (
                  <div className="samuori-muted text-xs">{r.statusReason}</div>
                )}
              </>
            ),
          },
          {
            id: "actions",
            name: "Actions",
            render: (r) => (
              <div className="samuori-actions">
                <Button
                  variant="secondary"
                  onPress={() => setViewing({ ...r, id: r.recipeId })}
                  aria-label={`Details of ${r.displayName} ${r.version}`}
                >
                  Details
                </Button>
                <Button
                  variant="secondary"
                  onPress={() => setRuntimesOf({ ...r, id: r.recipeId })}
                  aria-label={`Runtimes of ${r.displayName} ${r.version}`}
                >
                  Runtimes
                </Button>
                <Button
                  variant="secondary"
                  onPress={() => setEditing({ ...r, id: r.recipeId })}
                  aria-label={`Change status of ${r.displayName} ${r.version}`}
                >
                  Change status
                </Button>
              </div>
            ),
          },
        ]}
      />
      {viewing && (
        <RecipeDetails recipe={viewing} onClose={() => setViewing(undefined)} />
      )}
      <BaseImagePolicy />
      {runtimesOf && (
        <RecipeRuntimes
          recipe={runtimesOf}
          onClose={() => setRuntimesOf(undefined)}
          onChange={() => void load()}
        />
      )}
      <CustomRecipes onChange={() => void load()} />
      <StatusDialog
        recipe={editing}
        onClose={() => setEditing(undefined)}
        onDone={() => {
          setEditing(undefined);
          void load();
        }}
      />
    </>
  );
}

type BaseImagesDocument = {
  allowedRepositories: string[];
  requireDigest: boolean;
  pinTags?: boolean;
};

function BaseImagePolicy() {
  const [policy, setPolicy] = useState<Schemas["Policy"] | null>();
  const [requireDigest, setRequireDigest] = useState(true);
  const [pinTags, setPinTags] = useState(false);
  const [confirming, setConfirming] = useState<{
    summary: string;
    token: string;
  }>();
  const [message, setMessage] = useState<{
    tone: "info" | "error";
    text: string;
  }>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/policies");
    const p = data?.items.find((x) => x.kind === "base-images") ?? null;
    setPolicy(p);
    if (p) {
      const d = p.document as BaseImagesDocument;
      setRequireDigest(d.requireDigest);
      setPinTags(!!d.pinTags);
    }
  }, []);
  useLoader(load);
  if (!policy) return null;
  const current = policy.document as BaseImagesDocument;
  const policyDocument = () => ({
    allowedRepositories: current.allowedRepositories,
    requireDigest,
    pinTags: requireDigest && pinTags,
  });

  async function save() {
    if (!policy) return;
    setMessage(undefined);
    const { data, error } = await api.POST("/policies/{policyId}/dry-run", {
      params: { path: { policyId: policy.id } },
      body: { kind: "base-images", document: policyDocument() },
    });
    if (error) {
      setMessage({ tone: "error", text: describeError(error) });
      return;
    }
    if (data.confirmation) {
      setConfirming({ summary: data.summary ?? "", token: data.confirmation });
      return;
    }
    await apply();
  }

  async function apply(token?: string, reason?: string) {
    if (!policy) return;
    const { error } = await api.PUT("/policies/{policyId}", {
      params: {
        path: { policyId: policy.id },
        header: {
          "If-Match": `"${policy.version}"`,
          ...(token ? { "X-Samuori-Confirm": token } : {}),
        },
      },
      body: { kind: "base-images", document: policyDocument(), reason },
    });
    if (error) setMessage({ tone: "error", text: describeError(error) });
    else {
      setConfirming(undefined);
      setMessage({
        tone: "info",
        text: "Base-image policy saved as a new version.",
      });
      void load();
    }
  }

  return (
    <Section
      title="Base-image policy"
      description="Applies to Dockerfile, recipe and provider builds. Repositories are added through base-image requests."
    >
      {message && <Alert tone={message.tone}>{message.text}</Alert>}
      <p>
        {current.allowedRepositories.length} approved repositor
        {current.allowedRepositories.length === 1 ? "y" : "ies"} (version{" "}
        {policy.version}).
      </p>
      <Checkbox isSelected={requireDigest} onChange={setRequireDigest}>
        Require base images pinned by digest
      </Checkbox>
      <Checkbox
        isSelected={requireDigest && pinTags}
        isDisabled={!requireDigest}
        onChange={setPinTags}
      >
        Pin tags to their current digest at admission
      </Checkbox>
      <Explain summary="Pinning resolves a public tag to its digest when a build is admitted.">
        <p>
          With pinning, a Dockerfile can keep <code>FROM node:18-alpine</code>:
          Samuori looks up the tag&apos;s digest on its public registry when the
          build is admitted, builds exactly that image and records it on the
          build. Samuori does not update base images. Tags from private image
          sources must still be pinned by digest.
        </p>
      </Explain>
      <div className="samuori-actions samuori-actions--start">
        <Button
          isDisabled={
            requireDigest === current.requireDigest &&
            (requireDigest && pinTags) === !!current.pinTags
          }
          onPress={() => void save()}
        >
          Save base-image policy
        </Button>
      </div>
      <ConfirmPolicy
        change={confirming}
        onClose={() => setConfirming(undefined)}
        onConfirm={(reason) => void apply(confirming?.token, reason)}
      />
    </Section>
  );
}

function RecipeDetails({
  recipe,
  onClose,
}: {
  recipe: Recipe;
  onClose: () => void;
}) {
  const defaults = Object.entries(recipe.defaults ?? {});
  return (
    <Dialog
      title={`${recipe.displayName} ${recipe.version}`}
      size="wide"
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <p>{recipe.description}</p>
      <dl className="samuori-details">
        <dt>Recipe ID</dt>
        <dd>
          <code>
            {recipe.id}@{recipe.version}
          </code>
        </dd>
        <dt>Status</dt>
        <dd>
          {recipe.status}
          {recipe.statusReason ? ` — ${recipe.statusReason}` : ""}
        </dd>
        <dt>Languages</dt>
        <dd>{recipe.languages.join(", ") || "—"}</dd>
        <dt>Application types</dt>
        <dd>{recipe.applicationTypes.join(", ") || "—"}</dd>
        <dt>Platforms</dt>
        <dd>{recipe.platforms.join(", ")}</dd>
        {defaults.length > 0 && (
          <>
            <dt>Defaults</dt>
            <dd>
              <ul className="samuori-plain-list">
                {defaults.map(([k, v]) => (
                  <li key={k}>
                    {k}: <code>{v}</code>
                  </li>
                ))}
              </ul>
            </dd>
          </>
        )}
        {(recipe.secrets ?? []).length > 0 && (
          <>
            <dt>Secrets it mounts</dt>
            <dd>{recipe.secrets?.join(", ")}</dd>
          </>
        )}
      </dl>
      <h3>Base images by runtime</h3>
      <p className="samuori-muted">
        Pinned by digest, so a build always uses exactly these images.
      </p>
      <Table
        label="Base images"
        rows={recipe.runtimes.map((r) => ({ ...r, id: r.version }))}
        columns={[
          {
            id: "version",
            name: "Runtime",
            isRowHeader: true,
            render: (r) =>
              r.source === "organization"
                ? `${r.version} (added by your organization)`
                : r.status === "approved"
                  ? r.version
                  : `${r.version} (${r.status}${r.statusReason ? `: ${r.statusReason}` : ""})`,
          },
          {
            id: "images",
            name: "Images",
            render: (r) => (
              <ul className="samuori-plain-list">
                {[...new Set(r.images)].map((i) => (
                  <li key={i}>
                    <code>{i}</code>
                  </li>
                ))}
              </ul>
            ),
          },
        ]}
      />
      {(recipe.parameters ?? []).length > 0 && (
        <>
          <h3>Parameters</h3>
          <Table
            label="Parameters"
            rows={(recipe.parameters ?? []).map((p) => ({ ...p, id: p.name }))}
            columns={[
              {
                id: "name",
                name: "Name",
                isRowHeader: true,
                render: (p) => <code>{p.name}</code>,
              },
              { id: "type", name: "Type", render: (p) => p.type },
              {
                id: "default",
                name: "Default",
                render: (p) => p.default ?? (p.required ? "required" : "—"),
              },
              {
                id: "description",
                name: "Description",
                render: (p) => p.description ?? "",
              },
            ]}
          />
        </>
      )}
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          Close
        </Button>
      </div>
    </Dialog>
  );
}

function StatusDialog({
  recipe,
  onClose,
  onDone,
}: {
  recipe?: Recipe;
  onClose: () => void;
  onDone: () => void;
}) {
  const [status, setStatus] = useState("deprecated");
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!recipe) return;
    const { error } = await api.PUT(
      "/recipes/{recipeId}/versions/{version}/status",
      {
        params: { path: { recipeId: recipe.id, version: recipe.version } },
        body: { status: status as "deprecated", reason },
      },
    );
    if (error) setError(describeError(error));
    else {
      setReason("");
      onDone();
    }
  }
  return (
    <Dialog
      title={`Status of ${recipe?.displayName ?? ""} ${recipe?.version ?? ""}`}
      isOpen={!!recipe}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <Select
          label="Status"
          options={[
            { id: "approved", label: "Approved" },
            { id: "deprecated", label: "Deprecated (builds warn)" },
            { id: "withdrawn", label: "Withdrawn (builds refused)" },
          ]}
          value={status}
          onChange={setStatus}
        />
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
          <Button type="submit">Save</Button>
        </div>
      </Form>
    </Dialog>
  );
}

export function ConfirmPolicy({
  change,
  onClose,
  onConfirm,
}: {
  change?: { summary: string; token: string };
  onClose: () => void;
  onConfirm: (reason: string) => void;
}) {
  const [reason, setReason] = useState("");
  return (
    <Dialog
      title="Confirm the policy change"
      isOpen={!!change}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form
        className="samuori-form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          onConfirm(reason);
        }}
      >
        <Alert tone="warning">{change?.summary}</Alert>
        <TextField
          label="Reason"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={10}
          description="Recorded in the audit log with the change."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="danger">
            Apply the change
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

type Egress = Schemas["EgressStatus"];
type Destination = Egress["policy"]["destinations"][number];

export function EgressPage() {
  const [st, setSt] = useState<Egress | null>();
  const [allowed, setAllowed] = useState<string[]>([]);
  const [dockerfile, setDockerfile] = useState<string[]>([]);
  const [every, setEvery] = useState<string[]>([]);
  const [destinations, setDestinations] = useState<Destination[]>([]);
  const [adding, setAdding] = useState<Network>();
  const [confirming, setConfirming] = useState<{
    summary: string;
    token: string;
  }>();
  const [message, setMessage] = useState<{
    tone: "info" | "error";
    text: string;
  }>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/system/egress");
    setSt(data ?? null);
    if (data) {
      setAllowed(data.policy.allowedPresets);
      setDockerfile(data.policy.dockerfilePresets);
      setEvery(data.policy.everyBuildPresets ?? []);
      setDestinations(data.policy.destinations);
    }
  }, []);
  useLoader(load);
  if (st === undefined) return <Loading />;
  if (st === null) return <Alert>Egress settings could not be loaded.</Alert>;

  const toggle = (list: string[], id: string, on: boolean) =>
    on
      ? [...list.filter((x) => x !== id), id].sort()
      : list.filter((x) => x !== id);

  const reachOf = (id: string): PresetReach =>
    !allowed.includes(id)
      ? "none"
      : every.includes(id)
        ? "all"
        : dockerfile.includes(id)
          ? "need-dockerfile"
          : "need";
  // "All builds" also lists the preset for Dockerfile builds, so controllers that predate everyBuildPresets still
  // give it to Dockerfile builds during a rolling update.
  const setReach = (id: string, r: PresetReach) => {
    setAllowed(toggle(allowed, id, r !== "none"));
    setDockerfile(
      toggle(dockerfile, id, r === "need-dockerfile" || r === "all"),
    );
    setEvery(toggle(every, id, r === "all"));
  };

  const dirty =
    JSON.stringify([allowed, dockerfile, every, destinations]) !==
    JSON.stringify([
      st.policy.allowedPresets,
      st.policy.dockerfilePresets,
      st.policy.everyBuildPresets ?? [],
      st.policy.destinations,
    ]);

  const egressDocument = () => ({
    allowedPresets: allowed,
    dockerfilePresets: dockerfile.filter((d) => allowed.includes(d)),
    everyBuildPresets: every.filter((d) => allowed.includes(d)),
    destinations,
  });

  // A change that would block existing specs is previewed and confirmed with a reason before it applies (TM-92).
  async function save() {
    if (!st) return;
    setMessage(undefined);
    const { data, error } = await api.POST("/policies/{policyId}/dry-run", {
      params: { path: { policyId: st.policy.policyId } },
      body: { kind: "egress", document: egressDocument() },
    });
    if (error) {
      setMessage({ tone: "error", text: describeError(error) });
      return;
    }
    if (data.confirmation) {
      setConfirming({ summary: data.summary ?? "", token: data.confirmation });
      return;
    }
    await apply();
  }

  async function apply(token?: string, reason?: string) {
    if (!st) return;
    const { error } = await api.PUT("/policies/{policyId}", {
      params: {
        path: { policyId: st.policy.policyId },
        header: {
          "If-Match": `"${st.policy.version}"`,
          ...(token ? { "X-Samuori-Confirm": token } : {}),
        },
      },
      body: { kind: "egress", document: egressDocument(), reason },
    });
    if (error) setMessage({ tone: "error", text: describeError(error) });
    else {
      setConfirming(undefined);
      setMessage({
        tone: "info",
        text: "Egress policy saved as a new version.",
      });
      void load();
    }
  }

  const publicDests = destinations.filter((d) => d.network === "public");
  const internalDests = destinations.filter((d) => d.network !== "public");
  const remove = (name: string) =>
    setDestinations(destinations.filter((x) => x.name !== name));
  const change = (d: Destination) =>
    setDestinations(destinations.map((x) => (x.name === d.name ? d : x)));
  return (
    <>
      <PageHeader
        title="Egress"
        description="Builds reach the internet only through the egress proxy; anything not allowed here is refused."
      />
      <p>
        {st.workers.strictEgress} of the {st.workers.connected} worker(s)
        connected now enforce strict egress. Uploaded source builds only on
        workers that do.
      </p>
      {message && <Alert tone={message.tone}>{message.text}</Alert>}
      <Section
        title="Public internet"
        description="Builds that need it means recipes, Railpack and Buildpacks builds that use that ecosystem; Dockerfile builds get only what is set to reach them. Public hosts must resolve to public addresses."
        actions={
          <Button variant="secondary" onPress={() => setAdding("public")}>
            Add public destination
          </Button>
        }
        className="mt-6"
      >
        <Table
          label="Ecosystem presets"
          dependencies={[allowed, dockerfile, every]}
          rows={st.presets.map((p) => ({ ...p, id: p.id }))}
          columns={[
            {
              id: "name",
              name: "Preset",
              isRowHeader: true,
              render: (p) => `${p.displayName} (${p.id} ${p.version})`,
            },
            {
              id: "hosts",
              name: "Destinations",
              render: (p) => (
                <>
                  {p.destinations.join(", ")}
                  {p.note && <div className="samuori-muted">{p.note}</div>}
                </>
              ),
            },
            {
              id: "reach",
              name: "Who can reach it",
              render: (p) => (
                <div className="min-w-56">
                  <Select
                    label={`Who can reach ${p.displayName}`}
                    hideLabel
                    value={reachOf(p.id)}
                    onChange={(v) => setReach(p.id, v as PresetReach)}
                    options={presetReach}
                  />
                </div>
              ),
            },
          ]}
        />
        {publicDests.length > 0 && (
          <DestinationTable
            label="Public destinations"
            rows={publicDests}
            onRemove={remove}
            onChange={change}
          />
        )}
      </Section>
      <Section
        title="Internal destinations"
        description="Hosts inside your network, such as a private package repository. They may resolve to private addresses. Every build in the organization may reach them."
        actions={
          <Button variant="secondary" onPress={() => setAdding("internal")}>
            Add internal destination
          </Button>
        }
      >
        {internalDests.length > 0 && (
          <DestinationTable
            label="Internal destinations"
            rows={internalDests}
            onRemove={remove}
            onChange={change}
          />
        )}
      </Section>
      <div className="samuori-actions samuori-actions--start">
        <Button isDisabled={!dirty} onPress={() => void save()}>
          Save egress policy
        </Button>
      </div>
      <EgressGrants onPromoted={() => void load()} />
      <ConfirmPolicy
        change={confirming}
        onClose={() => setConfirming(undefined)}
        onConfirm={(reason) => void apply(confirming?.token, reason)}
      />
      <DestinationDialog
        network={adding}
        onClose={() => setAdding(undefined)}
        onAdd={(d) => {
          setDestinations([
            ...destinations.filter((x) => x.name !== d.name),
            d,
          ]);
          setAdding(undefined);
        }}
      />
    </>
  );
}

type Network = "public" | "internal";

type PresetReach = "none" | "need" | "need-dockerfile" | "all";

const presetReach: { id: PresetReach; label: string }[] = [
  { id: "none", label: "No builds" },
  { id: "need", label: "Builds that need it" },
  {
    id: "need-dockerfile",
    label: "Builds that need it, and Dockerfile builds",
  },
  { id: "all", label: "All builds" },
];

type DestinationReach = "none" | "not-dockerfile" | "all";

const destinationReach: { id: DestinationReach; label: string }[] = [
  { id: "none", label: "No builds" },
  { id: "not-dockerfile", label: "All builds except Dockerfile" },
  { id: "all", label: "All builds" },
];

function addresses(d: Destination): string[] {
  return [`${d.host}:${d.port}`, ...(d.also ?? [])];
}

function DestinationTable({
  label,
  rows,
  onRemove,
  onChange,
}: {
  label: string;
  rows: Destination[];
  onRemove: (name: string) => void;
  onChange: (d: Destination) => void;
}) {
  const setReach = (name: string, r: DestinationReach) => {
    const d = rows.find((x) => x.name === name);
    if (d)
      onChange({
        ...d,
        disabled: r === "none",
        noDockerfile: r === "not-dockerfile",
      });
  };
  return (
    <Table
      label={label}
      rows={rows.map((d) => ({ ...d, id: d.name }))}
      dependencies={[rows]}
      columns={[
        { id: "name", name: "Name", isRowHeader: true, render: (d) => d.name },
        {
          id: "addr",
          name: "Addresses",
          render: (d) => addresses(d).join(", "),
        },
        { id: "reason", name: "Reason", render: (d) => d.reason || "—" },
        {
          id: "reach",
          name: "Who can reach it",
          render: (d) => (
            <div className="min-w-56">
              <Select
                label={`Who can reach ${d.name}`}
                hideLabel
                value={
                  d.disabled
                    ? "none"
                    : d.noDockerfile
                      ? "not-dockerfile"
                      : "all"
                }
                onChange={(v) => setReach(d.name, v as DestinationReach)}
                options={destinationReach}
              />
            </div>
          ),
        },
        {
          id: "remove",
          name: "Actions",
          render: (d) => (
            <Button
              variant="secondary"
              onPress={() => onRemove(d.name)}
              aria-label={`Remove ${d.name}`}
            >
              Remove
            </Button>
          ),
        },
      ]}
    />
  );
}

/** Parses one host or host:port per line; a missing port is 443. */
function parseAddresses(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((a) => a.trim().toLowerCase())
    .filter(Boolean)
    .map((a) => (/:\d+$/.test(a) ? a : `${a}:443`));
}

function DestinationDialog({
  network,
  onClose,
  onAdd,
}: {
  network?: Network;
  onClose: () => void;
  onAdd: (d: Destination) => void;
}) {
  const [name, setName] = useState("");
  const [text, setText] = useState("");
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  function submit(e: FormEvent) {
    e.preventDefault();
    const list = parseAddresses(text);
    const [first, ...rest] = list;
    const m = first?.match(/^(.+):(\d+)$/);
    if (!m) {
      setError("Enter at least one host, such as github.com.");
      return;
    }
    onAdd({
      name,
      host: m[1] ?? "",
      port: Number(m[2]),
      also: rest.length ? rest : undefined,
      reason,
      network,
    });
    setName("");
    setText("");
    setReason("");
    setError(undefined);
  }
  const internal = network === "internal";
  return (
    <Dialog
      title={internal ? "Add internal destination" : "Add public destination"}
      isOpen={network !== undefined}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          placeholder="package-mirror"
          label="Name"
          value={name}
          onChange={setName}
          isRequired
          description="Lower-case letters, digits and dashes, such as github-releases."
        />
        <TextField
          placeholder="packages.example.com"
          label="Addresses"
          value={text}
          onChange={setText}
          isRequired
          multiline
          description={
            internal
              ? "One host per line, with :port when it is not 443. Hosts inside your network; loopback, link-local and metadata addresses are refused."
              : "One host per line, with :port when it is not 443, such as github.com and objects.githubusercontent.com. Private addresses and internal names belong under Internal destinations."
          }
        />
        <TextField
          label="Reason (optional)"
          value={reason}
          onChange={setReason}
          description="Helps whoever reviews the policy later."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Add</Button>
        </div>
      </Form>
    </Dialog>
  );
}
