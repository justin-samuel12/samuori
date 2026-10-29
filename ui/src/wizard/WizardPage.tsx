import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { AlertTriangle, Check } from "lucide-react";
import { Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { Checkbox, RadioGroup, Select } from "../components/Field";
import { Alert, Loading, PageHeader } from "../components/Feedback";
import { useLoader } from "../hooks";
import { navigate } from "../router";
import {
  defaultProfile,
  formPaths,
  comboKey,
  comboPrefix,
  findingKey,
  appName,
  fromFinding,
  frontendBackendPairs,
  pickRecipe,
  withFrontend,
  get,
  migrateDraft,
  needsConfirmation,
  pathKey,
  set,
  slug,
  unrepresented,
  type Doc,
  type Mode,
  type Path,
  type Provenance,
} from "../spec/model";
import {
  BuildArgsAt,
  CheckAt,
  EditorContext,
  LinesAt,
  NumberAt,
  PairsAt,
  PlatformsAt,
  SelectAt,
  TextAt,
  useEditor,
} from "./fields";
import { SecretsPanel } from "./SecretsPanel";
import { YamlEditor } from "./YamlEditor";
import { RequestBaseImageDialog } from "../pages/BaseImageRequests";
import { ReadinessList } from "../components/Readiness";
import { useCan } from "../session/session";
import type { Scope } from "../session/can";
import {
  useEgressRequests,
  type AccessContext,
  type BuildMethod,
} from "../components/EgressAccess";
import { correctName } from "../format";
import { stepStatus, type StepStatus } from "../pages/display";

const stepStatusText: Record<StepStatus, string> = {
  current: "current step",
  done: "done",
  todo: "not reached yet",
  problem: "needs attention",
};

type Finding = Schemas["DetectorFinding"];
type Recipe = Schemas["Recipe"];
type Discovery = Schemas["Discovery"];
type BuildMethods = Schemas["BuildMethodOptions"];

interface State {
  step: number;
  mode: Mode;
  snapshotId?: string;
  appKey?: string;
  dockerfile?: string;
  // strategy is the build strategy chosen in "Choose how to build": auto, recipe, railpack, buildpacks or dockerfile:<path>.
  strategy?: string;
  doc: Doc;
  provenance: Provenance;
  specId?: string;
  revision?: number;
}

const steps = [
  "Upload source",
  "Choose app",
  "Describe your app",
  "Choose how to build",
  "Checks",
  "Image details",
  "Review & build",
];

const dockerfilePrefix = "dockerfile:";
const dockerfileKey = (path: string) => dockerfilePrefix + path;

const kindOptions = [
  { id: "http-service", label: "Web service or API" },
  { id: "worker", label: "Background worker" },
  { id: "cli-task", label: "Command-line tool or task" },
  { id: "static-site", label: "Static website" },
];

const profiles = [
  { id: "http-startup", label: "Starts and answers HTTP requests" },
  { id: "process-start", label: "Starts and keeps running" },
  { id: "cli-smoke", label: "Runs a command successfully" },
  { id: "static-serve", label: "Serves the built files" },
  { id: "none-requested", label: "No application check" },
];

type Saved = { specId: string; revision: number };
type SaveAction = "save" | "check" | "build";

export function WizardPage({ projectId }: { projectId: string }) {
  const [state, setState] = useState<State>();
  const [draftVersion, setDraftVersion] = useState(0);
  const [resumed, setResumed] = useState<string>();
  const [project, setProject] = useState<Schemas["Project"]>();
  const [snapshots, setSnapshots] = useState<Schemas["SourceSnapshot"][]>([]);
  const [discovery, setDiscovery] = useState<Discovery | null>();
  const [recipes, setRecipes] = useState<Recipe[]>([]);
  const [methods, setMethods] = useState<BuildMethods>();
  const [pending, setPending] = useState<{
    path: Path;
    value: unknown;
    last: Mode;
  } | null>(null);
  const [yamlDirty, setYamlDirty] = useState(false);
  const [leaveYaml, setLeaveYaml] = useState<Mode | null>(null);
  const [furthest, setFurthest] = useState(0);
  const [report, setReport] = useState<Schemas["AdmissionReport"]>();
  const egressRequests = useEgressRequests(projectId);
  const [existing, setExisting] = useState<{
    spec: Schemas["BuildSpec"];
    action: SaveAction;
  } | null>(null);
  const [message, setMessage] = useState<{
    tone: "error" | "success" | "info" | "warning";
    text: string;
  }>();

  const load = useCallback(async () => {
    const [pr, sn, rc, dr, bm] = await Promise.all([
      api.GET("/projects/{projectId}", { params: { path: { projectId } } }),
      api.GET("/projects/{projectId}/snapshots", {
        params: { path: { projectId }, query: { limit: 20 } },
      }),
      api.GET("/recipes"),
      api.GET("/projects/{projectId}/wizard-draft", {
        params: { path: { projectId } },
      }),
      api.GET("/projects/{projectId}/build-methods", {
        params: { path: { projectId } },
      }),
    ]);
    setProject(pr.data);
    setMethods(bm.data);
    setSnapshots(sn.data?.items ?? []);
    setRecipes((rc.data?.items ?? []).filter((r) => r.status !== "withdrawn"));
    const query = new URLSearchParams(window.location.search);
    const asked = query.get("snapshot");
    const draft = dr.data?.data as unknown as State | undefined;
    // A choice made on the analysis page (another upload, or a Dockerfile or app picked there) starts afresh.
    const fresh =
      !!asked &&
      (draft?.snapshotId !== asked ||
        query.has("dockerfile") ||
        query.has("app"));
    if (draft && dr.data && !fresh) {
      setState(migrateDraft(draft));
      setDraftVersion(dr.data.version);
      setResumed(new Date(dr.data.updatedAt).toLocaleString());
    } else {
      if (dr.data) setDraftVersion(dr.data.version);
      const wanted = asked ?? sn.data?.items[0]?.id;
      setState({
        step: 0,
        mode: "form",
        snapshotId: wanted ?? undefined,
        doc: {},
        provenance: {},
      });
    }
  }, [projectId]);
  useLoader(load);

  if (state && state.step > furthest) setFurthest(state.step);

  // Readiness is recomputed against the current egress policy, so reaching the checks step fetches it again.
  const [recheck, setRecheck] = useState(0);
  const onChecks = state?.step === 4;
  useEffect(() => {
    if (!state?.snapshotId) return;
    let live = true;
    void api
      .GET("/snapshots/{snapshotId}/discoveries", {
        params: { path: { snapshotId: state.snapshotId } },
      })
      .then(async ({ data }) => {
        const latest = data?.items.find((d) => d.status === "completed");
        const full = latest
          ? await api.GET("/discoveries/{discoveryId}", {
              params: { path: { discoveryId: latest.id } },
            })
          : null;
        if (live) setDiscovery(full?.data ?? null);
      });
    return () => {
      live = false;
    };
  }, [state?.snapshotId, onChecks, recheck]);

  const saveTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const versionRef = useRef(draftVersion);
  useEffect(() => {
    versionRef.current = draftVersion;
  }, [draftVersion]);
  useEffect(() => {
    if (!state) return;
    if (saveTimer.current) clearTimeout(saveTimer.current);
    saveTimer.current = setTimeout(() => {
      void api
        .PUT("/projects/{projectId}/wizard-draft", {
          params: { path: { projectId } },
          body: {
            version: versionRef.current,
            data: state as never,
          },
        })
        .then(({ data, error }) => {
          if (data) setDraftVersion(data.version);
          else if (error)
            setMessage({
              tone: "error",
              text: describeError(error, "Your progress could not be saved."),
            });
        });
    }, 800);
    return () => {
      if (saveTimer.current) clearTimeout(saveTimer.current);
    };
  }, [state, projectId]);

  const update = (fn: (s: State) => State) => setState((s) => (s ? fn(s) : s));

  const apply = useCallback(
    (path: Path, value: unknown, mode: Mode) =>
      update((s) => ({
        ...s,
        doc: set(s.doc, path, value),
        provenance: { ...s.provenance, [pathKey(path)]: mode },
        revision: s.revision,
      })),
    [],
  );

  const edit = useCallback(
    (path: Path, value: unknown) => {
      if (!state) return;
      const last = needsConfirmation(state.provenance, path, state.mode);
      if (last) setPending({ path, value, last });
      else apply(path, value, state.mode);
    },
    [state, apply],
  );

  const findings = discovery?.findings ?? [];
  const apps = findings.filter((f) => f.applicationKind);
  const combos = frontendBackendPairs(apps);
  const finding = findings.find((f) => findingKey(f) === state?.appKey);
  const dockerfiles = discovery?.dockerfiles ?? [];

  const compatible = useMemo(() => {
    const lang = get(state?.doc, [
      "spec",
      "application",
      "runtime",
      "language",
    ]) as string | undefined;
    const kind = get(state?.doc, ["spec", "application", "type"]) as
      string | undefined;
    // A custom recipe that lists no languages or application types fits any application.
    return recipes.filter(
      (r) =>
        (!lang ||
          (r.languages ?? []).includes(lang) ||
          (r.custom && !r.languages?.length)) &&
        (!kind ||
          (r.applicationTypes ?? []).includes(kind) ||
          (r.custom && !r.applicationTypes?.length)),
    );
  }, [recipes, state?.doc]);

  function chooseCombo(key: string) {
    const c = combos.find((x) => comboKey(x.fe, x.be) === key);
    if (!c) return false;
    chooseApp(findingKey(c.be), (d) => withFrontend(d, c.fe), key);
    return true;
  }

  function chooseApp(key: string, extend?: (d: Doc) => Doc, appKey = key) {
    const f = findings.find((x) => findingKey(x) === key);
    const name = appName(f, project?.name);
    const doc = fromFinding(
      name,
      f,
      pickRecipe(recipes, fromFinding(name, f, undefined)),
    );
    update((s) => ({
      ...s,
      appKey,
      doc: extend ? extend(doc) : doc,
      provenance: {},
      dockerfile: undefined,
    }));
  }

  // Points the spec at a Dockerfile in the upload; Samuori builds it exactly as written.
  function withDockerfile(doc: Doc, p: string): Doc {
    const df = dockerfiles.find((x) => x.path === p);
    const dir = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : ".";
    let d = set(doc, ["spec", "build", "recipe"], undefined);
    d = set(d, ["spec", "build", "railpack"], undefined);
    d = set(d, ["spec", "build", "buildpacks"], undefined);
    d = set(d, ["spec", "build", "method"], "dockerfile");
    return set(d, ["spec", "build", "dockerfile"], {
      context: dir,
      path: p.slice(p.lastIndexOf("/") + 1),
      ...(df?.stages.length ? { target: df.stages[df.stages.length - 1] } : {}),
    });
  }

  const dirOf = (p: string) =>
    p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : ".";
  // The application discovered in a Dockerfile's folder, which describes what the Dockerfile builds.
  const appAt = (dir: string) =>
    findings.find((f) => f.applicationKind && f.sourceSubpath === dir);

  // fillFrom copies what a finding says into fields still empty, and the port a Dockerfile exposes when it
  // exposes exactly one; nothing the user entered is replaced.
  function fillFrom(doc: Doc, f: Finding | undefined, dockerfile?: string) {
    let d = doc;
    if (f) {
      const found = fromFinding("", f, undefined);
      for (const path of [
        ["spec", "application", "type"],
        ["spec", "application", "runtime", "language"],
        ["spec", "application", "runtime", "framework"],
        ["spec", "application", "runtime", "versionConstraint"],
      ])
        if (get(d, path) === undefined && get(found, path) !== undefined)
          d = set(d, path, get(found, path));
    }
    // A profile chosen while the kind was unknown follows the kind once it is known.
    const kindNow = get(d, ["spec", "application", "type"]) as
      string | undefined;
    const profile = get(d, ["spec", "validation", "profile"]);
    if (kindNow && (profile === undefined || profile === "none-requested"))
      d = set(d, ["spec", "validation", "profile"], defaultProfile(kindNow));
    const ports =
      dockerfiles.find((x) => x.path === dockerfile)?.exposedPorts ?? [];
    const port = Number(ports[0]);
    if (
      ports.length === 1 &&
      Number.isInteger(port) &&
      get(d, ["spec", "application", "port"]) === undefined &&
      get(d, ["spec", "application", "type"]) === "http-service"
    )
      d = set(d, ["spec", "application", "port"], port);
    return d;
  }

  function chooseDockerfileApp(p: string) {
    const doc = withDockerfile(
      fillFrom(
        fromFinding(slug(project?.name ?? "app"), undefined, undefined),
        appAt(dirOf(p)),
        p,
      ),
      p,
    );
    update((s) => ({
      ...s,
      appKey: dockerfileKey(p),
      doc,
      provenance: {},
      dockerfile: p,
    }));
  }

  // ?dockerfile= or ?app= from the analysis page picks that build once its discovery has loaded; arriving from the
  // analysis page with neither skips the upload step, straight to describing the app when nothing was recognised.
  const preselect = useRef(new URLSearchParams(window.location.search));
  useEffect(() => {
    if (!discovery || !state) return;
    const q = preselect.current;
    const df = q.get("dockerfile");
    const app = q.get("app");
    if (!df && !app) {
      if (!q.has("snapshot") || state.step !== 0) return;
      q.delete("snapshot");
      if (apps.length + dockerfiles.length === 0) {
        // Runs once, when the discovery fetched from the server arrives.
        chooseApp("");
        // eslint-disable-next-line react-hooks/set-state-in-effect
        update((s) => ({ ...s, step: 2 }));
      } else update((s) => ({ ...s, step: 1 }));
      return;
    }
    q.delete("dockerfile");
    q.delete("app");
    if (df && dockerfiles.some((x) => x.path === df)) chooseDockerfileApp(df);
    else if (app?.startsWith(comboPrefix)) {
      if (!chooseCombo(app)) return;
    } else if (app && findings.some((f) => findingKey(f) === app))
      chooseApp(app);
    else return;
    update((s) => ({ ...s, step: 2 }));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [discovery, state === undefined]);

  const methodOption = (m: string) =>
    methods?.methods.find((x) => x.method === m);
  // Without the endpoint's answer only the methods every installation allows are offered.
  const allowed = (m: string) =>
    methodOption(m)?.available ?? (m === "recipe" || m === "dockerfile");

  // recommend picks a strategy from the static detectors' findings and the method policy; nothing runs.
  function recommend(): { strategy: string; why: string } {
    const recipeFits =
      allowed("recipe") && compatible.some((r) => r.status === "approved");
    const first = dockerfiles[0];
    const firstDockerfile =
      first && allowed("dockerfile") ? dockerfileKey(first.path) : undefined;
    switch (methods?.default) {
      case "dockerfile":
        if (firstDockerfile)
          return {
            strategy: firstDockerfile,
            why: "your organization's default, and the upload has a Dockerfile",
          };
        break;
      case "railpack":
        if (allowed("railpack"))
          return { strategy: "railpack", why: "your organization's default" };
        break;
      case "buildpacks":
        if (allowed("buildpacks"))
          return {
            strategy: "buildpacks",
            why: "your organization's default",
          };
        break;
      case "recipe":
        if (recipeFits)
          return { strategy: "recipe", why: "your organization's default" };
    }
    if (firstDockerfile)
      return { strategy: firstDockerfile, why: "the upload has a Dockerfile" };
    if (recipeFits)
      return {
        strategy: "recipe",
        why: "an approved recipe fits the detected application",
      };
    if (allowed("railpack"))
      return {
        strategy: "railpack",
        why: "no approved recipe fits, and Railpack plans the build on the worker",
      };
    return { strategy: "recipe", why: "no other method is available" };
  }

  function strategyLabel(st: string): string {
    if (st.startsWith(dockerfilePrefix))
      return `your Dockerfile (${st.slice(dockerfilePrefix.length)})`;
    if (st === "buildpacks") return "Paketo Buildpacks";
    return st === "railpack" ? "Railpack" : "a Samuori recipe";
  }

  function chooseStrategy(choice: string) {
    if (!state) return;
    const st = choice === "auto" ? recommend().strategy : choice;
    let d = state.doc;
    let dockerfile: string | undefined;
    if (st.startsWith(dockerfilePrefix)) {
      dockerfile = st.slice(dockerfilePrefix.length);
      d = withDockerfile(d, dockerfile);
    } else if (st === "railpack") {
      const start = get(d, ["spec", "build", "recipe", "start"]);
      const cmd = get(d, ["spec", "build", "recipe", "build"]);
      d = set(d, ["spec", "build", "recipe"], undefined);
      d = set(d, ["spec", "build", "dockerfile"], undefined);
      d = set(d, ["spec", "build", "args"], undefined);
      d = set(d, ["spec", "build", "buildpacks"], undefined);
      d = set(d, ["spec", "build", "method"], "railpack");
      if (typeof cmd === "string" && cmd)
        d = set(d, ["spec", "build", "railpack", "buildCommand"], cmd);
      if (typeof start === "string" && start)
        d = set(d, ["spec", "build", "railpack", "startCommand"], start);
    } else if (st === "buildpacks") {
      d = set(d, ["spec", "build", "recipe"], undefined);
      d = set(d, ["spec", "build", "dockerfile"], undefined);
      d = set(d, ["spec", "build", "railpack"], undefined);
      d = set(d, ["spec", "build", "args"], undefined);
      d = set(d, ["spec", "build", "method"], "buildpacks");
    } else {
      d = set(d, ["spec", "build", "dockerfile"], undefined);
      d = set(d, ["spec", "build", "railpack"], undefined);
      d = set(d, ["spec", "build", "buildpacks"], undefined);
      d = set(d, ["spec", "build", "method"], "recipe");
      // Coming from a Dockerfile, the commands and runtime come from the app found in its folder.
      const f =
        finding ??
        (state.dockerfile ? appAt(dirOf(state.dockerfile)) : undefined);
      d = fillFrom(d, f, state.dockerfile);
      const cmds = f?.suggestedCommands ?? {};
      for (const k of ["install", "build", "start", "test"])
        if (get(d, ["spec", "build", "recipe", k]) === undefined && cmds[k])
          d = set(d, ["spec", "build", "recipe", k], cmds[k]);
      const lang = get(d, ["spec", "application", "runtime", "language"]);
      const kindNow = get(d, ["spec", "application", "type"]);
      const recipe = recipes.find(
        (r) =>
          r.status === "approved" &&
          (!lang || (r.languages ?? []).includes(lang as string)) &&
          (!kindNow || (r.applicationTypes ?? []).includes(kindNow as string)),
      );
      if (recipe) {
        d = set(d, ["spec", "build", "recipe", "id"], recipe.id);
        d = set(d, ["spec", "build", "recipe", "version"], recipe.version);
      }
    }
    update((s) => ({ ...s, doc: d, dockerfile, strategy: choice }));
  }

  async function save(
    action: SaveAction = "save",
    into?: Saved,
  ): Promise<Saved | null> {
    if (!state) return null;
    setMessage(undefined);
    const body = {
      document: state.doc as Schemas["BuildSpecDocument"],
      // The API keeps its earlier enum; the single form records as "detailed".
      editorMode:
        state.mode === "yaml" ? ("yaml" as const) : ("detailed" as const),
    };
    const target =
      into ??
      (state.specId && state.revision
        ? { specId: state.specId, revision: state.revision }
        : undefined);
    const res = target
      ? await api.POST("/specs/{specId}/revisions", {
          params: {
            path: { specId: target.specId },
            header: { "If-Match": `"${target.revision}"` },
          },
          body,
        })
      : await api.POST("/projects/{projectId}/specs", {
          params: { path: { projectId } },
          body,
        });
    if (!res.data) {
      const err = res.error as Schemas["Problem"] | undefined;
      if (!target && err?.code === "spec_exists") {
        const name = get(state.doc, ["metadata", "name"]) as string;
        const found = await api.GET("/projects/{projectId}/specs", {
          params: { path: { projectId }, query: { name, limit: 1 } },
        });
        const spec = found.data?.items[0];
        if (spec) {
          setExisting({ spec, action });
          return null;
        }
      }
      const details = err?.details
        ?.map((d) => `${d.field}: ${d.reason}`)
        .join("; ");
      setMessage({
        tone: "error",
        text: `${describeError(err, "The spec could not be saved.")}${details ? ` ${details}` : ""}`,
      });
      return null;
    }
    const saved = { specId: res.data.specId, revision: res.data.revision };
    update((s) => ({ ...s, ...saved }));
    const warnings = res.data.warnings ?? [];
    setMessage({
      tone: warnings.length ? "warning" : "success",
      text: `Saved as revision ${saved.revision}.${warnings.map((w) => ` ${w.message}.`).join("")}`,
    });
    return saved;
  }

  async function check(into?: Saved) {
    const saved = await save("check", into);
    if (!saved || !state?.snapshotId) return;
    const { data, error } = await api.POST(
      "/specs/{specId}/revisions/{revision}/admission-checks",
      {
        params: { path: { specId: saved.specId, revision: saved.revision } },
        body: { snapshotId: state.snapshotId },
      },
    );
    if (data) setReport(data);
    else setMessage({ tone: "error", text: describeError(error) });
  }

  async function build(into?: Saved) {
    const saved = await save("build", into);
    if (!saved || !state?.snapshotId) return;
    const { data, error } = await api.POST("/builds", {
      params: { header: { "Idempotency-Key": crypto.randomUUID() } },
      body: {
        specId: saved.specId,
        specRevision: saved.revision,
        snapshotId: state.snapshotId,
        cleanBuild: false,
      },
    });
    if (!data) {
      setMessage({
        tone: "error",
        text: describeError(error, "The build could not be started."),
      });
      return;
    }
    void api.DELETE("/projects/{projectId}/wizard-draft", {
      params: { path: { projectId } },
    });
    navigate(`/builds/${data.id}`);
  }

  function updateExisting() {
    if (!existing) return;
    const into = {
      specId: existing.spec.id,
      revision: existing.spec.latestRevision,
    };
    const action = existing.action;
    setExisting(null);
    if (action === "check") void check(into);
    else if (action === "build") void build(into);
    else void save("save", into);
  }

  function switchMode(m: Mode) {
    if (!state || m === state.mode) return;
    if (state.mode === "yaml" && yamlDirty) setLeaveYaml(m);
    else update((s) => ({ ...s, mode: m }));
  }

  if (!state || project === undefined) return <Loading />;
  const kind = get(state.doc, ["spec", "application", "type"]) as
    string | undefined;
  const method = get(state.doc, ["spec", "build", "method"]) as
    string | undefined;
  const buildMethod = (
    ["dockerfile", "recipe", "railpack", "buildpacks"] as BuildMethod[]
  ).find((m) => m === (state.dockerfile ? "dockerfile" : method));
  const accessBase = {
    projectId,
    method: buildMethod,
    requests: egressRequests.items,
    onRequested: () => void egressRequests.reload(),
  };
  const discoveryAccess: AccessContext | undefined =
    discovery && buildMethod
      ? { ...accessBase, evidence: { discoveryId: discovery.id } }
      : undefined;
  const reportAccess: AccessContext | undefined =
    state.specId && state.revision && state.snapshotId
      ? {
          ...accessBase,
          evidence: {
            specId: state.specId,
            specRevision: state.revision,
            snapshotId: state.snapshotId,
          },
        }
      : undefined;
  const hidden =
    state.mode === "yaml" ? [] : unrepresented(state.doc, formPaths);
  const singlePlatform = method === "railpack" || method === "buildpacks";
  const selectedRecipe = recipes.find(
    (r) =>
      r.id === get(state.doc, ["spec", "build", "recipe", "id"]) &&
      r.version === get(state.doc, ["spec", "build", "recipe", "version"]),
  );
  const mountedSecrets =
    method === "dockerfile"
      ? (dockerfiles.find((d) => d.path === state.dockerfile)?.secretMounts ??
        [])
      : (selectedRecipe?.secrets ?? []).map((id) => ({ id }));
  const recipe = recipes.find(
    (r) =>
      r.id === get(state.doc, ["spec", "build", "recipe", "id"]) &&
      r.version === get(state.doc, ["spec", "build", "recipe", "version"]),
  );
  const languageOptions = [
    ...new Set(
      recipes
        .filter((r) => r.status === "approved")
        .flatMap((r) => r.languages ?? []),
    ),
  ]
    .sort()
    .map((l) => ({ id: l, label: l }));
  // Recipes need the language; the other methods detect it themselves.
  const needsLanguage =
    !method || !["dockerfile", "railpack", "buildpacks"].includes(method);
  // What the Describe step still needs before Next; the schema requires the same on save.
  const described = [
    !get(state.doc, ["metadata", "name"]) && "Image name",
    !kind && "Kind of application",
    needsLanguage &&
      !get(state.doc, ["spec", "application", "runtime", "language"]) &&
      "Language",
    kind === "http-service" &&
      !get(state.doc, ["spec", "application", "port"]) &&
      "Port",
    method === "recipe" &&
      kind === "http-service" &&
      !get(state.doc, ["spec", "build", "recipe", "start"]) &&
      "Start command",
  ].filter((x): x is string => !!x);
  const missing = state.step === 2 ? described : [];
  const readiness = state.dockerfile
    ? dockerfiles.find((d) => d.path === state.dockerfile)?.readiness
    : finding?.readiness;
  const problems = [
    !state.snapshotId,
    false,
    described.length > 0,
    false,
    (readiness ?? []).some((r) => r.severity === "will-fail"),
    false,
    !!report && !report.admissible,
  ];

  return (
    <EditorContext.Provider
      value={{ doc: state.doc, mode: state.mode, edit, readOnly: false }}
    >
      <PageHeader
        help="new-image"
        title="New image"
        crumbs={[
          { label: "Projects", href: "/projects" },
          { label: project?.name ?? "Project", href: `/projects/${projectId}` },
          { label: "New image" },
        ]}
        description="Describe your application; we turn it into a build specification you can review."
      />
      {resumed && (
        <Alert tone="info">
          Resumed your draft from {resumed}.{" "}
          <Button
            variant="quiet"
            onPress={() => {
              void api.DELETE("/projects/{projectId}/wizard-draft", {
                params: { path: { projectId } },
              });
              setDraftVersion(0);
              setResumed(undefined);
              setFurthest(0);
              setState({
                step: 0,
                mode: state.mode,
                snapshotId: snapshots[0]?.id,
                doc: {},
                provenance: {},
              });
            }}
          >
            Start over
          </Button>
        </Alert>
      )}
      {message && <Alert tone={message.tone}>{message.text}</Alert>}

      <ol className="samuori-stepper" aria-label="Steps">
        {steps.map((name, i) => {
          const status = stepStatus(i, state.step, furthest, !!problems[i]);
          return (
            <li key={name} data-status={status}>
              <Button
                variant="quiet"
                onPress={() => update((s) => ({ ...s, step: i }))}
                aria-current={i === state.step ? "step" : undefined}
              >
                {status === "done" && <Check size={14} aria-hidden="true" />}
                {status === "problem" && (
                  <AlertTriangle size={14} aria-hidden="true" />
                )}
                {i + 1}. {name}
                <span className="sr-only"> ({stepStatusText[status]})</span>
              </Button>
            </li>
          );
        })}
      </ol>

      <section className="samuori-section" aria-labelledby="step-h">
        <h2 id="step-h">
          {state.mode === "yaml"
            ? "Edit as YAML"
            : `Step ${state.step + 1}: ${steps[state.step]}`}
        </h2>
        {state.mode === "yaml" ? (
          <>
            <div className="samuori-actions samuori-actions--start">
              <Button variant="secondary" onPress={() => switchMode("form")}>
                Back to the form
              </Button>
            </div>
            <YamlEditor
              doc={state.doc}
              onDirty={setYamlDirty}
              onApply={(d) => {
                const prov = { ...state.provenance };
                for (const p of formPaths)
                  if (
                    JSON.stringify(get(d, p)) !==
                    JSON.stringify(get(state.doc, p))
                  )
                    prov[pathKey(p)] = "yaml";
                update((s) => ({ ...s, doc: d, provenance: prov }));
              }}
            />
          </>
        ) : (
          <>
            {hidden.length > 0 && (
              <Alert tone="info">
                These settings can only be edited as YAML and are kept as they
                are:{" "}
                {hidden.map((h) => (
                  <code key={h}>{h} </code>
                ))}
              </Alert>
            )}
            {state.step === 0 && (
              <StepBody>
                {snapshots.length === 0 ? (
                  <p>
                    No uploads yet.{" "}
                    <Link
                      href={`/projects/${projectId}/upload`}
                      className="samuori-link"
                    >
                      Upload your project
                    </Link>{" "}
                    first.
                  </p>
                ) : (
                  <Select
                    label="Source upload"
                    placeholder="Choose an upload"
                    options={snapshots.map((s) => ({
                      id: s.id,
                      label: `${new Date(s.createdAt).toLocaleString()} — ${s.fileCount} files`,
                    }))}
                    value={state.snapshotId ?? null}
                    onChange={(v) =>
                      update((s) => ({
                        ...s,
                        snapshotId: v,
                        appKey: undefined,
                      }))
                    }
                  />
                )}
                <Link
                  href={`/projects/${projectId}/upload`}
                  className="samuori-link"
                >
                  Upload newer files
                </Link>
              </StepBody>
            )}
            {state.step === 1 && (
              <StepBody>
                {discovery === undefined && <Loading />}
                {discovery === null && (
                  <Alert tone="warning">
                    This upload has not been analysed. Open it from the project
                    page and choose “Analyse source”, or describe your app
                    yourself.
                  </Alert>
                )}
                {apps.length === 0 && discovery && (
                  <Alert tone="info">
                    No application was recognised. You can still describe it in
                    the next step.
                  </Alert>
                )}
                {apps.length + dockerfiles.length > 0 && (
                  <RadioGroup
                    label={
                      apps.length + dockerfiles.length > 1
                        ? "What do you want to build?"
                        : apps.length > 0
                          ? "Detected application"
                          : "Your Dockerfile"
                    }
                    value={state.appKey ?? ""}
                    onChange={(k) =>
                      k.startsWith(dockerfilePrefix)
                        ? chooseDockerfileApp(k.slice(dockerfilePrefix.length))
                        : k.startsWith(comboPrefix)
                          ? chooseCombo(k)
                          : chooseApp(k)
                    }
                    options={[
                      ...dockerfiles.map((d) => ({
                        id: dockerfileKey(d.path),
                        label: `Build my Dockerfile (${d.path})`,
                        description:
                          "exactly as written; you describe the app in the next step",
                      })),
                      ...combos.map(({ fe, be }) => ({
                        id: comboKey(fe, be),
                        label: `Frontend + backend in one image (${fe.sourceSubpath} and ${be.sourceSubpath})`,
                        description:
                          "Samuori's web launcher serves the frontend and forwards /api to the backend",
                      })),
                      ...apps.map((f) => ({
                        id: findingKey(f),
                        label: `${[f.framework, f.language].filter(Boolean).join(" · ")}${f.sourceSubpath === "." ? "" : ` in ${f.sourceSubpath}`}`,
                        description:
                          f.confidence === "high"
                            ? "detected"
                            : f.confidence === "medium"
                              ? "likely, please confirm the details"
                              : "unsure, please check every detail",
                      })),
                    ]}
                  />
                )}
                {!state.appKey &&
                  apps.length === 0 &&
                  dockerfiles.length === 0 && (
                    <Button variant="secondary" onPress={() => chooseApp("")}>
                      Describe it myself
                    </Button>
                  )}
              </StepBody>
            )}
            {state.step === 2 && (
              <StepBody>
                {(finding?.warnings ?? []).map((w, i) => (
                  <Alert key={i} tone="warning">
                    {w.message}
                  </Alert>
                ))}
                <TextAt
                  path={["metadata", "name"]}
                  label="Image name"
                  isRequired
                  normalize={(t, done) => correctName(t, { done })}
                  description="Saved in lowercase with dashes, for example web-api."
                />
                <TextAt
                  path={["metadata", "description"]}
                  label="Description (optional)"
                />
                <SelectAt
                  path={["spec", "application", "type"]}
                  label="What kind of application is it?"
                  options={kindOptions}
                />
                {needsLanguage && (
                  <SelectAt
                    path={["spec", "application", "runtime", "language"]}
                    label="Language"
                    options={languageOptions}
                    description="Detected from your source when we recognise it; otherwise choose it."
                  />
                )}
                {method !== "dockerfile" &&
                  method !== "railpack" &&
                  method !== "buildpacks" && (
                    <>
                      <LinesAt
                        path={["spec", "build", "recipe", "install"]}
                        label="Install dependencies"
                        item="install command"
                        placeholder="npm ci"
                        addLabel="Add install command"
                        description="Commands run in order; the build stops at the first that fails."
                        commands
                      />
                      <LinesAt
                        path={["spec", "build", "recipe", "build"]}
                        label={
                          kind === "static-site"
                            ? "Build commands (produce the site)"
                            : "Build commands (optional)"
                        }
                        item="build command"
                        addLabel="Add build command"
                        commands
                      />
                      {kind !== "static-site" && (
                        <TextAt
                          path={["spec", "build", "recipe", "start"]}
                          label={
                            kind === "cli-task"
                              ? "Command to run"
                              : "Start command"
                          }
                          isRequired={kind === "http-service"}
                          description="One command: a container runs one main process. To start several processes, add a start script to your project and run it here."
                        />
                      )}
                      <LinesAt
                        path={["spec", "build", "recipe", "test"]}
                        label="Test commands (optional)"
                        item="test command"
                        addLabel="Add test command"
                        commands
                      />
                    </>
                  )}
                {method === "recipe" &&
                  get(state.doc, ["spec", "build", "recipe", "frontend"]) !==
                    undefined && (
                    <fieldset className="samuori-fieldset">
                      <legend>Frontend in the same image</legend>
                      <p className="samuori-muted samuori-line-help">
                        Samuori&apos;s web launcher serves the built frontend on
                        the image&apos;s port, forwards the API paths to your
                        backend, and starts the backend with its start command.
                      </p>
                      <TextAt
                        path={["spec", "build", "recipe", "path"]}
                        label="Backend folder"
                        isRequired
                      />
                      <TextAt
                        path={["spec", "build", "recipe", "frontend", "path"]}
                        label="Frontend folder"
                        isRequired
                      />
                      <LinesAt
                        path={[
                          "spec",
                          "build",
                          "recipe",
                          "frontend",
                          "install",
                        ]}
                        label="Install the frontend's dependencies"
                        item="frontend install command"
                        placeholder="npm ci"
                        addLabel="Add install command"
                        commands
                      />
                      <LinesAt
                        path={["spec", "build", "recipe", "frontend", "build"]}
                        label="Build the frontend"
                        item="frontend build command"
                        placeholder="npm run build"
                        addLabel="Add build command"
                        commands
                      />
                      <TextAt
                        path={["spec", "build", "recipe", "frontend", "output"]}
                        label="Folder the frontend build writes"
                        placeholder="dist"
                      />
                      <LinesAt
                        path={[
                          "spec",
                          "build",
                          "recipe",
                          "frontend",
                          "apiPrefixes",
                        ]}
                        label="API paths sent to the backend"
                        item="API path"
                        placeholder="/api"
                        addLabel="Add API path"
                      />
                      <NumberAt
                        path={[
                          "spec",
                          "build",
                          "recipe",
                          "frontend",
                          "backendPort",
                        ]}
                        label="Port the backend listens on"
                        description="Your start command must make the backend listen on this port, on 127.0.0.1 or all addresses."
                      />
                    </fieldset>
                  )}
                {kind === "http-service" && (
                  <>
                    <NumberAt
                      path={["spec", "application", "port"]}
                      label={
                        get(state.doc, [
                          "spec",
                          "build",
                          "recipe",
                          "frontend",
                        ]) !== undefined
                          ? "Port the image listens on"
                          : "Port the app listens on"
                      }
                      isRequired
                      description="Check your code or configuration; we do not guess it."
                    />
                    <TextAt
                      path={["spec", "application", "healthPath"]}
                      label="Health check path (optional)"
                      placeholder="/healthz"
                    />
                  </>
                )}
              </StepBody>
            )}
            {state.step === 3 && (
              <StepBody>
                <RadioGroup
                  label="How should we build it?"
                  value={
                    state.strategy ??
                    (method === "dockerfile" && state.dockerfile
                      ? dockerfileKey(state.dockerfile)
                      : method === "railpack" || method === "buildpacks"
                        ? method
                        : "recipe")
                  }
                  onChange={chooseStrategy}
                  options={[
                    {
                      id: "auto",
                      label: "Automatic (recommended)",
                      description: `uses ${strategyLabel(recommend().strategy)}: ${recommend().why}`,
                    },
                    {
                      id: "railpack",
                      label: "Railpack",
                      description: allowed("railpack")
                        ? "plans and builds from your project files on a build worker"
                        : (methodOption("railpack")?.reason ?? "not available"),
                      isDisabled: !allowed("railpack"),
                    },
                    {
                      id: "buildpacks",
                      label: "Paketo Buildpacks",
                      description: allowed("buildpacks")
                        ? "detects and builds your project with an approved builder on a build worker"
                        : (methodOption("buildpacks")?.reason ??
                          "not available"),
                      isDisabled: !allowed("buildpacks"),
                    },
                    {
                      id: "recipe",
                      label: "Samuori recipe",
                      description: allowed("recipe")
                        ? "an approved recipe for your language"
                        : (methodOption("recipe")?.reason ?? "not available"),
                      isDisabled: !allowed("recipe"),
                    },
                    ...(dockerfiles.length > 0
                      ? dockerfiles.map((d) => ({
                          id: dockerfileKey(d.path),
                          label: `Custom Dockerfile (${d.path})`,
                          description: allowed("dockerfile")
                            ? "builds exactly as written"
                            : (methodOption("dockerfile")?.reason ??
                              "not available"),
                          isDisabled: !allowed("dockerfile"),
                        }))
                      : [
                          {
                            id: "dockerfile:",
                            label: "Custom Dockerfile",
                            description: "the upload has no Dockerfile",
                            isDisabled: true,
                          },
                        ]),
                  ]}
                />
                {method && !allowed(method) && (
                  <Alert tone="warning">
                    {methodOption(method)?.reason ??
                      "This build method is not allowed here."}{" "}
                    Choose another way to build.
                  </Alert>
                )}
                {method === "buildpacks" && (
                  <>
                    <TextAt
                      path={["spec", "build", "buildpacks", "context"]}
                      label="Folder to build (optional)"
                      placeholder="."
                      description="Relative to the upload. The buildpacks see this folder only."
                    />
                    <p className="samuori-muted">
                      The administrator's approved builder detects and builds
                      the project on a build worker, inside the build's
                      isolation, as a non-root user, with network access only to
                      the sources an administrator allowed. Buildpack settings
                      such as BP_NODE_VERSION go under
                      spec.build.buildpacks.env, and each build secret needs a
                      binding under spec.build.buildpacks.bindings, for example
                      an npmrc binding for an npm token; edit them in the
                      BuildSpec editor.
                    </p>
                  </>
                )}
                {method === "railpack" && (
                  <>
                    <TextAt
                      path={["spec", "build", "railpack", "context"]}
                      label="Folder to build (optional)"
                      placeholder="."
                      description="Relative to the upload. Railpack reads this folder only."
                    />
                    <TextAt
                      path={["spec", "build", "railpack", "buildCommand"]}
                      label="Build command (optional)"
                      description="Leave empty to let Railpack choose. It runs during the build, never while planning."
                    />
                    <TextAt
                      path={["spec", "build", "railpack", "startCommand"]}
                      label="Start command (optional)"
                      description="Leave empty to let Railpack choose; the build fails if it finds none."
                    />
                    <p className="samuori-muted">
                      Railpack plans the build on a build worker, inside the
                      build's isolation, with network access only to the sources
                      an administrator allowed. The plan is kept with the build.
                      Secrets reach build steps as environment variables, for
                      example npm-token as NPM_TOKEN.
                    </p>
                  </>
                )}
                {method === "recipe" && (
                  <Select
                    label="Recipe"
                    placeholder="Choose a recipe"
                    options={compatible.map((r) => ({
                      id: `${r.id}@${r.version}`,
                      label: `${r.displayName} ${r.version}${r.status === "deprecated" ? " (deprecated)" : ""}`,
                    }))}
                    value={recipe ? `${recipe.id}@${recipe.version}` : null}
                    onChange={(v) => {
                      const [id, version] = v.split("@");
                      edit(["spec", "build", "recipe", "id"], id);
                      edit(["spec", "build", "recipe", "version"], version);
                    }}
                    description={
                      recipe?.description ??
                      (compatible.length === 0
                        ? "No approved recipe fits this application. Use a Dockerfile, or ask an administrator."
                        : undefined)
                    }
                  />
                )}
                {method === "recipe" &&
                  selectedRecipe?.custom &&
                  (selectedRecipe.parameters ?? []).map((p) => (
                    <TextAt
                      key={p.name}
                      path={["spec", "build", "recipe", "parameters", p.name]}
                      label={p.name}
                      isRequired={p.required}
                      placeholder={p.default}
                      description={[
                        p.description,
                        p.enum?.length
                          ? `One of ${p.enum.join(", ")}.`
                          : undefined,
                        p.default ? `Leave empty for ${p.default}.` : undefined,
                      ]
                        .filter(Boolean)
                        .join(" ")}
                    />
                  ))}
                {method === "recipe" && recipe && !recipe.custom && (
                  <>
                    <SelectAt
                      path={[
                        "spec",
                        "application",
                        "runtime",
                        "versionConstraint",
                      ]}
                      label="Runtime version"
                      options={runtimeOptions(
                        recipe,
                        get(state.doc, [
                          "spec",
                          "application",
                          "runtime",
                          "versionConstraint",
                        ]),
                      )}
                      description={
                        finding?.runtimeConstraints &&
                        Object.values(finding.runtimeConstraints)[0]
                          ? `Your project asks for ${Object.values(finding.runtimeConstraints)[0]}. Approved versions only.`
                          : "Approved versions only."
                      }
                    />
                    <LinesAt
                      path={[
                        "spec",
                        "build",
                        "recipe",
                        "systemPackages",
                        "build",
                      ]}
                      label="System packages for the build (optional)"
                      item="build package"
                      placeholder="build-essential"
                      addLabel="Add build package"
                      description="Operating system packages the build needs, such as compilers or git. Package names only, for example python3; installed with the base image's package manager."
                    />
                    <LinesAt
                      path={[
                        "spec",
                        "build",
                        "recipe",
                        "systemPackages",
                        "runtime",
                      ]}
                      label="System packages in the image (optional)"
                      item="image package"
                      placeholder="ca-certificates"
                      addLabel="Add image package"
                      description="Packages your app needs when it runs, such as shared libraries. Each one adds to the image's size and to what security scans report."
                    />
                  </>
                )}
                {method === "dockerfile" && (
                  <>
                    <SelectAt
                      path={["spec", "build", "dockerfile", "target"]}
                      label="Final stage"
                      options={(
                        dockerfiles.find((d) => d.path === state.dockerfile)
                          ?.stages ?? []
                      ).map((s) => ({ id: s, label: s }))}
                    />
                    {(
                      dockerfiles.find((d) => d.path === state.dockerfile)
                        ?.ambiguities ?? []
                    ).map((w, i) => (
                      <Alert key={i} tone="warning">
                        {w.message}
                      </Alert>
                    ))}
                  </>
                )}
                {(method === "recipe" || method === "dockerfile") && (
                  <BuildArgsAt path={["spec", "build", "args"]} />
                )}
                <EngineAt />

                <PlatformsAt
                  path={["spec", "target", "platforms"]}
                  single={singlePlatform}
                />
                <SecretsPanel
                  readiness={report?.secretReadiness}
                  mounted={mountedSecrets}
                  projectId={projectId}
                />
              </StepBody>
            )}
            {state.step === 4 && (
              <StepBody>
                <ReadinessList
                  heading="Before you build"
                  access={discoveryAccess}
                  items={
                    state.dockerfile
                      ? dockerfiles.find((d) => d.path === state.dockerfile)
                          ?.readiness
                      : finding?.readiness
                  }
                />
                <div className="samuori-actions samuori-actions--start">
                  <Button
                    variant="secondary"
                    onPress={() => setRecheck((n) => n + 1)}
                  >
                    Check again
                  </Button>
                </div>
                <SelectAt
                  path={["spec", "validation", "profile"]}
                  label="How should we check the image works?"
                  options={profiles}
                  description={`Recommended: ${profiles.find((p) => p.id === defaultProfile(kind))?.label}.`}
                />
                <p className="samuori-muted">
                  Checks an administrator requires are added automatically and
                  cannot be removed. They run when builds become available.
                </p>
                <NumberAt
                  path={["spec", "validation", "timeoutSeconds"]}
                  label="Check timeout in seconds (optional)"
                  description="Default 60. Checks stop after at most 600 seconds."
                />
                <CheckAt path={["spec", "outputs", "sbom"]}>
                  Produce a software bill of materials (SBOM)
                </CheckAt>
                <LinesAt
                  path={["spec", "validation", "extraChecks"]}
                  label="Extra checks (optional)"
                  item="extra check"
                  addLabel="Add extra check"
                  description="Check IDs in lowercase with dashes. Kept in the spec; builds do not run extra checks yet."
                />
                <CheckAt path={["spec", "outputs", "provenance"]}>
                  Record build provenance (kept in the spec; builds do not use
                  it yet)
                </CheckAt>
              </StepBody>
            )}
            {state.step === 5 && (
              <StepBody>
                <LinesAt
                  path={["spec", "outputs", "image", "tags"]}
                  label="Tags (optional)"
                  item="tag"
                  addLabel="Add tag"
                  placeholder="{version}"
                  description="Tags a publication applies when the person publishing gives none. {version} is the build's version, the UTC date it succeeded and a counter for that day, such as 2026-10-27.1. Leave empty for {version} alone. Add latest here if you want it. Every image also keeps its permanent samuori- tag."
                />
                <PairsAt
                  path={["spec", "outputs", "image", "labels"]}
                  label="Image labels (optional)"
                  item="label"
                  description="Written into the image's configuration, where docker inspect shows them. For example org.opencontainers.image.source. Samuori adds the build time, the image name and, for builds from a Git pipeline, the commit; a value you set wins. Not applied by Railpack or buildpacks builds."
                />
                <PairsAt
                  path={["spec", "outputs", "image", "annotations"]}
                  label="Image annotations (optional)"
                  item="annotation"
                  description="Written onto the image manifest, where registries show them, with the same automatic values. Not applied by buildpacks builds."
                />
                <p className="samuori-muted">
                  Labels and annotations describe the image; they are not tags,
                  which name it in a registry. Anyone who can pull the image can
                  read them, so never put secrets in them.
                </p>
              </StepBody>
            )}
            {state.step === 6 && (
              <Review
                projectId={projectId}
                doc={state.doc}
                report={report}
                access={reportAccess}
                onSave={() => void save()}
                onYaml={() => switchMode("yaml")}
                onCheck={() => void check()}
                onBuild={() => void build()}
                canCheck={!!state.snapshotId}
                scope={{ teamId: project?.teamId, projectId }}
              />
            )}
          </>
        )}
      </section>

      <div className="samuori-actions">
        <Button
          variant="secondary"
          isDisabled={state.step === 0}
          onPress={() => update((s) => ({ ...s, step: s.step - 1 }))}
        >
          Back
        </Button>
        {missing.length > 0 && (
          <span className="samuori-muted">
            Fill in {missing.join(", ")} first.
          </span>
        )}
        {state.step < steps.length - 1 && (
          <Button
            isDisabled={missing.length > 0}
            onPress={() => update((s) => ({ ...s, step: s.step + 1 }))}
          >
            Next
          </Button>
        )}
      </div>

      <Dialog
        title="Replace a change made in another view?"
        size="confirm"
        isOpen={!!pending}
        onOpenChange={(o) => !o && setPending(null)}
      >
        <p>
          This field was last changed in{" "}
          {pending?.last === "yaml" ? "the YAML view" : "the form"}. Replace
          that value?
        </p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => setPending(null)}>
            Keep it
          </Button>
          <Button
            onPress={() => {
              if (pending) apply(pending.path, pending.value, state.mode);
              setPending(null);
            }}
          >
            Replace
          </Button>
        </div>
      </Dialog>
      <Dialog
        title="Update the existing spec?"
        isOpen={!!existing}
        onOpenChange={(o) => !o && setExisting(null)}
      >
        <p>
          This project already has a spec named{" "}
          <strong>{existing?.spec.name}</strong> (latest revision{" "}
          {existing?.spec.latestRevision}). Save your choices as revision{" "}
          {(existing?.spec.latestRevision ?? 0) + 1} of it? Earlier revisions
          and their builds stay as they are. To keep both, cancel and change the
          image name.
        </p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => setExisting(null)}>
            Cancel
          </Button>
          <Button onPress={updateExisting}>Update existing spec</Button>
        </div>
      </Dialog>
      <Dialog
        title="Discard YAML changes?"
        size="confirm"
        isOpen={!!leaveYaml}
        onOpenChange={(o) => !o && setLeaveYaml(null)}
      >
        <p>You have YAML edits that have not been applied.</p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => setLeaveYaml(null)}>
            Stay in YAML
          </Button>
          <Button
            variant="danger"
            onPress={() => {
              setYamlDirty(false);
              update((s) => ({ ...s, mode: leaveYaml ?? s.mode }));
              setLeaveYaml(null);
            }}
          >
            Discard
          </Button>
        </div>
      </Dialog>
    </EditorContext.Provider>
  );
}

function StepBody({ children }: { children: ReactNode }) {
  return <div className="samuori-form">{children}</div>;
}

function EngineAt() {
  const { doc, edit } = useEditor();
  const path = ["spec", "build", "engine", "preference"];
  return (
    <Checkbox
      isSelected={get(doc, path) === "buildkit"}
      onChange={(on) => edit(path, on ? "buildkit" : undefined)}
    >
      Prefer the BuildKit engine (kept in the spec; the build pool decides the
      engine)
    </Checkbox>
  );
}

/** The recipe's runtimes that are not withdrawn, organization and deprecated ones labelled, plus a constraint the spec already holds. */
function runtimeOptions(recipe: Recipe, current: unknown) {
  const options = recipe.runtimes
    .filter((r) => r.status !== "withdrawn")
    .map((r) => ({
      id: r.version,
      label:
        r.source === "organization"
          ? `${r.version} (added by your organization)`
          : r.status === "deprecated"
            ? `${r.version} (deprecated${r.statusReason ? `: ${r.statusReason}` : ""})`
            : r.version,
    }));
  if (
    typeof current === "string" &&
    current &&
    !options.some((o) => o.id === current)
  )
    options.unshift({ id: current, label: `${current} (newest that matches)` });
  return options;
}

function Review({
  projectId,
  doc,
  report,
  access,
  onSave,
  onCheck,
  onBuild,
  onYaml,
  canCheck,
  scope,
}: {
  projectId: string;
  doc: Doc;
  report?: Schemas["AdmissionReport"];
  access?: AccessContext;
  onSave: () => void;
  onCheck: () => void;
  onBuild: () => void;
  onYaml: () => void;
  canCheck: boolean;
  scope: Scope;
}) {
  const can = useCan();
  const [yaml, setYaml] = useState<Schemas["SpecValidation"]>();
  useEffect(() => {
    void api
      .POST("/spec-documents/validate", {
        body: { document: doc as Schemas["BuildSpecDocument"] },
      })
      .then(({ data }) => setYaml(data));
  }, [doc]);
  const name = get(doc, ["metadata", "name"]) as string | undefined;
  const method = get(doc, ["spec", "build", "method"]);
  const issues = [
    ...(yaml?.schemaErrors ?? []),
    ...(yaml?.semanticErrors ?? []),
  ];
  return (
    <div className="samuori-form">
      <p>
        We will build <strong>{name ?? "your image"}</strong>{" "}
        {method === "dockerfile"
          ? "from your Dockerfile"
          : method === "railpack"
            ? "with Railpack"
            : method === "buildpacks"
              ? "with Paketo Buildpacks"
              : `with the ${get(doc, ["spec", "build", "recipe", "id"]) ?? "selected"} recipe`}
        {get(doc, ["spec", "application", "port"])
          ? `, listening on port ${get(doc, ["spec", "application", "port"])}`
          : ""}
        .
      </p>
      {issues.length > 0 && (
        <Alert>
          Fix these before saving:
          <ul className="samuori-issue-list">
            {issues.map((i, n) => (
              <li key={n}>
                <code>{i.field}</code> {i.reason}
              </li>
            ))}
          </ul>
        </Alert>
      )}
      {report && (
        <AdmissionSummary
          projectId={projectId}
          report={report}
          access={access}
        />
      )}
      <details className="samuori-details-toggle">
        <summary>Show the build specification (YAML)</summary>
        <pre className="samuori-code" data-testid="review-yaml">
          {yaml?.canonicalYaml}
        </pre>
      </details>
      <div className="samuori-actions samuori-actions--start">
        <Button variant="quiet" onPress={onYaml}>
          Advanced: edit as YAML
        </Button>
      </div>
      <div className="samuori-actions samuori-actions--start">
        <Button variant="secondary" onPress={onSave} isDisabled={!yaml?.valid}>
          Save draft
        </Button>
        {can("checkAdmission", scope) && (
          <Button
            variant="secondary"
            onPress={onCheck}
            isDisabled={!yaml?.valid || !canCheck}
          >
            Check if it can build
          </Button>
        )}
        {can("createBuild", scope) && (
          <Button onPress={onBuild} isDisabled={!yaml?.valid || !canCheck}>
            Build
          </Button>
        )}
      </div>
      <p className="samuori-muted">
        Building saves a new revision first. A build that cannot run yet is
        recorded as blocked, with the reason.
      </p>
    </div>
  );
}

function AdmissionSummary({
  projectId,
  report,
  access,
}: {
  projectId: string;
  report: Schemas["AdmissionReport"];
  access?: AccessContext;
}) {
  const blocking = [
    ...(report.semanticErrors ?? []).map((e) => e.reason ?? ""),
    ...(report.schemaErrors ?? []).map((e) => e.reason ?? ""),
    ...(report.policyDecisions ?? [])
      .filter((d) => d.outcome === "block")
      .map((d) => d.reason ?? "Blocked by policy"),
    ...(report.platformIssues ?? []).map((w) => w.message),
    ...report.secretReadiness
      .filter((s) => s.required && s.status !== "ready")
      .map((s) => `Secret ${s.id}: ${s.reason ?? s.status}`),
  ];
  const requestable = [
    ...new Set(
      (report.policyDecisions ?? []).flatMap(
        (d) => d.requestableRepositories ?? [],
      ),
    ),
  ];
  const predictedFailures = (report.readiness ?? []).filter(
    (r) => r.severity === "will-fail" && !r.blocking,
  ).length;
  return (
    <>
      {report.admissible ? (
        predictedFailures > 0 ? (
          <Alert tone="warning">
            Samuori would accept this revision, but predicts {predictedFailures}{" "}
            {predictedFailures === 1 ? "problem" : "problems"} that will make
            the build fail. See below.
          </Alert>
        ) : (
          <Alert tone="success">
            This revision can be built from the selected upload.
          </Alert>
        )
      ) : (
        <Alert tone="warning">
          This revision cannot be built yet:
          <ul className="samuori-issue-list">
            {blocking.map((b, i) => (
              <li key={i}>{b}</li>
            ))}
          </ul>
          {requestable.length > 0 && (
            <RequestableRepositories
              projectId={projectId}
              repositories={requestable}
            />
          )}
        </Alert>
      )}
      <ReadinessList items={report.readiness} access={access} />
    </>
  );
}

function RequestableRepositories({
  projectId,
  repositories,
}: {
  projectId: string;
  repositories: string[];
}) {
  const canRequest = useCan()("requestBaseImage");
  const [requesting, setRequesting] = useState<string>();
  const [sent, setSent] = useState<string[]>([]);
  return (
    <div className="samuori-actions samuori-actions--start">
      {repositories.map((repo) =>
        sent.includes(repo) ? (
          <span key={repo} className="samuori-muted">
            Requested <code>{repo}</code>; an organization administrator decides
            under Approvals.
          </span>
        ) : canRequest ? (
          <Button
            key={repo}
            variant="secondary"
            onPress={() => setRequesting(repo)}
            aria-label={`Request this repository: ${repo}`}
          >
            Request {repo}
          </Button>
        ) : (
          <span key={repo} className="samuori-muted">
            A project developer can request <code>{repo}</code>.
          </span>
        ),
      )}
      {requesting && (
        <RequestBaseImageDialog
          key={requesting}
          projectId={projectId}
          repository={requesting}
          isOpen
          onClose={() => setRequesting(undefined)}
          onDone={() => {
            setSent((s) => [...s, requesting]);
            setRequesting(undefined);
          }}
        />
      )}
    </div>
  );
}
