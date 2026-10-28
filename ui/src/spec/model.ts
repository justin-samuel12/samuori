import type { Schemas } from "../api/client";

export type Doc = Record<string, unknown>;
export type Mode = "form" | "yaml";
export type Path = (string | number)[];

export function get(doc: unknown, path: Path): unknown {
  let cur: unknown = doc;
  for (const k of path) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string | number, unknown>)[k];
  }
  return cur;
}

function isEmpty(v: unknown): boolean {
  return (
    v === undefined ||
    v === "" ||
    (Array.isArray(v) && v.length === 0) ||
    (typeof v === "object" &&
      v !== null &&
      !Array.isArray(v) &&
      Object.keys(v).length === 0)
  );
}

/** Returns a copy of doc with path set to value; empty values remove the key. Unrelated fields are never touched. */
export function set(doc: Doc, path: Path, value: unknown): Doc {
  const [head, ...rest] = path;
  if (head === undefined) return value as Doc;
  const src = (doc ?? {}) as Record<string | number, unknown>;
  const next = rest.length
    ? set(
        (src[head] ?? (typeof rest[0] === "number" ? [] : {})) as Doc,
        rest,
        value,
      )
    : value;
  if (Array.isArray(src)) {
    const arr = [...src];
    if (isEmpty(next)) arr.splice(Number(head), 1);
    else arr[Number(head)] = next;
    return arr as unknown as Doc;
  }
  const entries = Object.entries(src).filter(([k]) => k !== String(head));
  if (!isEmpty(next)) {
    const idx = Object.keys(src).indexOf(String(head));
    entries.splice(idx < 0 ? entries.length : idx, 0, [String(head), next]);
  }
  return Object.fromEntries(entries) as Doc;
}

export const pathKey = (p: Path) => p.join(".");

/** Fields the form edits. Anything else present in the document is kept and listed as YAML-only. */
export const formPaths: Path[] = [
  ["metadata", "name"],
  ["spec", "application", "type"],
  ["spec", "application", "runtime", "language"],
  ["spec", "application", "runtime", "framework"],
  ["spec", "application", "runtime", "versionConstraint"],
  ["spec", "application", "port"],
  ["spec", "application", "healthPath"],
  ["spec", "build", "method"],
  ["spec", "build", "recipe", "id"],
  ["spec", "build", "recipe", "version"],
  ["spec", "build", "recipe", "install"],
  ["spec", "build", "recipe", "build"],
  ["spec", "build", "recipe", "start"],
  ["spec", "build", "recipe", "test"],
  ["spec", "build", "recipe", "parameters"],
  ["spec", "build", "recipe", "systemPackages", "build"],
  ["spec", "build", "recipe", "systemPackages", "runtime"],
  ["spec", "build", "recipe", "path"],
  ["spec", "build", "recipe", "frontend", "path"],
  ["spec", "build", "recipe", "frontend", "install"],
  ["spec", "build", "recipe", "frontend", "build"],
  ["spec", "build", "recipe", "frontend", "output"],
  ["spec", "build", "recipe", "frontend", "apiPrefixes"],
  ["spec", "build", "recipe", "frontend", "backendPort"],
  ["spec", "build", "dockerfile", "context"],
  ["spec", "build", "dockerfile", "path"],
  ["spec", "build", "dockerfile", "target"],
  ["spec", "build", "railpack", "context"],
  ["spec", "build", "railpack", "buildCommand"],
  ["spec", "build", "railpack", "startCommand"],
  ["spec", "build", "buildpacks", "context"],
  ["spec", "secrets"],
  ["spec", "validation", "profile"],
  ["metadata", "description"],
  ["spec", "build", "args"],
  ["spec", "build", "engine"],
  ["spec", "target", "platforms"],
  ["spec", "validation", "timeoutSeconds"],
  ["spec", "validation", "extraChecks"],
  ["spec", "outputs", "sbom"],
  ["spec", "outputs", "provenance"],
  ["spec", "outputs", "image"],
];

function leaves(v: unknown, path: Path, out: Path[]) {
  if (v !== null && typeof v === "object" && !Array.isArray(v)) {
    for (const [k, child] of Object.entries(v))
      leaves(child, [...path, k], out);
  } else out.push(path);
}

/** Lists document fields the form cannot edit, such as x-* extensions. */
export function unrepresented(doc: Doc, editable: Path[]): string[] {
  const keys = editable.map(pathKey);
  const all: Path[] = [];
  leaves(doc, [], all);
  return all
    .filter(
      (p) =>
        !(p.length === 2 && p[0] === "apiVersion") &&
        !["apiVersion", "kind"].includes(String(p[0])),
    )
    .map(pathKey)
    .filter((k) => !keys.some((e) => k === e || k.startsWith(e + ".")))
    .sort();
}

/** Remembers which view last changed each field, so overwriting the other view's edit can be confirmed. */
export type Provenance = Record<string, Mode>;

/** The view a stored draft or provenance entry used; drafts saved before the single form said guided or detailed. */
export function toMode(v: unknown): Mode {
  return v === "yaml" ? "yaml" : "form";
}

/** Reads a stored wizard draft, mapping the former Guided and Detailed modes to the single form. */
export function migrateDraft<T extends object>(
  raw: T,
): T & { mode: Mode; provenance: Provenance } {
  const r = raw as { mode?: unknown; provenance?: unknown };
  const provenance: Provenance = {};
  if (r.provenance && typeof r.provenance === "object")
    for (const [k, v] of Object.entries(r.provenance))
      provenance[k] = toMode(v);
  return { ...raw, mode: toMode(r.mode), provenance };
}

export function needsConfirmation(
  prov: Provenance,
  path: Path,
  mode: Mode,
): Mode | null {
  const last = prov[pathKey(path)];
  return last && last !== mode ? last : null;
}

const languageOf: Record<string, string> = {
  javascript: "nodejs",
  typescript: "nodejs",
  csharp: "dotnet",
  fsharp: "dotnet",
  visualbasic: "dotnet",
};

/** Builds a starting document from a detector finding; the user confirms or edits every field. */
export function fromFinding(
  name: string,
  f: Schemas["DetectorFinding"] | undefined,
  recipe: Schemas["Recipe"] | undefined,
): Doc {
  let d: Doc = {
    apiVersion: "samuori.invalid/v1alpha1",
    kind: "ImageBuild",
    metadata: { name },
  };
  if (f) {
    const lang = f.language ?? "";
    d = set(d, ["spec", "application", "type"], f.applicationKind);
    d = set(
      d,
      ["spec", "application", "runtime", "language"],
      languageOf[lang] ?? lang,
    );
    d = set(d, ["spec", "application", "runtime", "framework"], f.framework);
    const rc = f.runtimeConstraints ?? {};
    d = set(
      d,
      ["spec", "application", "runtime", "versionConstraint"],
      rc.node ?? rc.python ?? rc.dotnet ?? rc.powershell,
    );
    const cmds = f.suggestedCommands ?? {};
    for (const k of ["install", "build", "start", "test"])
      d = set(d, ["spec", "build", "recipe", k], cmds[k]);
    if (f.sourceSubpath && f.sourceSubpath !== ".")
      d = set(d, ["spec", "build", "recipe", "path"], f.sourceSubpath);
  }
  d = set(d, ["spec", "build", "method"], "recipe");
  if (recipe) {
    d = set(d, ["spec", "build", "recipe", "id"], recipe.id);
    d = set(d, ["spec", "build", "recipe", "version"], recipe.version);
  }
  d = set(
    d,
    ["spec", "validation", "profile"],
    defaultProfile(get(d, ["spec", "application", "type"]) as string),
  );
  return d;
}

export function defaultProfile(kind: string | undefined): string {
  return (
    {
      "http-service": "http-startup",
      "static-site": "static-serve",
      worker: "process-start",
      "cli-task": "cli-smoke",
    }[kind ?? ""] ?? "none-requested"
  );
}

export function slug(s: string): string {
  return (
    s
      .toLowerCase()
      .replace(/[^a-z0-9-]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 63) || "app"
  );
}

// withFrontend serves a Node.js frontend from the same image as the application: Samuori's web launcher listens on the
// image's port and forwards /api to the application on the frontend's backend port (ADR 0055).
export function withFrontend(doc: Doc, f: Schemas["DetectorFinding"]): Doc {
  const cmds = f.suggestedCommands ?? {};
  let d = set(doc, ["spec", "build", "recipe", "frontend"], {
    path: f.sourceSubpath,
    ...(cmds.install ? { install: cmds.install } : {}),
    ...(cmds.build ? { build: cmds.build } : {}),
    apiPrefixes: ["/api"],
    backendPort: 8000,
  });
  d = set(d, ["spec", "application", "type"], "http-service");
  if (get(d, ["spec", "application", "port"]) === undefined)
    d = set(d, ["spec", "application", "port"], 8080);
  return set(d, ["spec", "validation", "profile"], "http-startup");
}

type Finding = Schemas["DetectorFinding"];

export const findingKey = (f: Finding) => `${f.detectorId}:${f.sourceSubpath}`;
export const comboPrefix = "frontend+backend:";
export const comboKey = (fe: Finding, be: Finding) =>
  `${comboPrefix}${findingKey(fe)}|${findingKey(be)}`;

// frontendBackendPairs lists each Node.js static site and HTTP service in separate folders, which can ship as one
// image behind Samuori's web launcher.
export function frontendBackendPairs(
  findings: Finding[],
): { fe: Finding; be: Finding }[] {
  const node = ["javascript", "typescript", "nodejs"];
  return findings.flatMap((fe) =>
    fe.applicationKind === "static-site" &&
    fe.sourceSubpath !== "." &&
    node.includes(fe.language ?? "")
      ? findings
          .filter(
            (be) =>
              be.applicationKind === "http-service" &&
              be.sourceSubpath !== "." &&
              be.sourceSubpath !== fe.sourceSubpath,
          )
          .map((be) => ({ fe, be }))
      : [],
  );
}
