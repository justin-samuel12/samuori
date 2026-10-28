/** Reads the OpenAPI contract served at /api/v1/openapi.json into what the API reference page shows. */

export type Json = Record<string, unknown>;

export interface Parameter {
  name: string;
  in: string;
  required: boolean;
  description?: string;
  schema?: Json;
}

export interface Response {
  status: string;
  description: string;
  schema?: Json;
}

export interface Operation {
  id: string;
  method: string;
  path: string;
  tag: string;
  description?: string;
  roles: string[];
  isPublic: boolean;
  deprecated: boolean;
  parameters: Parameter[];
  body?: Json;
  bodyType?: string;
  responses: Response[];
}

const methods = ["get", "post", "put", "patch", "delete"];

const obj = (v: unknown): Json =>
  v && typeof v === "object" && !Array.isArray(v) ? (v as Json) : {};
const list = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const str = (v: unknown): string | undefined =>
  typeof v === "string" ? v : undefined;

/** Follows a local "#/components/…" reference; anything else is returned unchanged. */
export function deref(spec: Json, v: unknown): Json {
  const ref = str(obj(v).$ref);
  if (!ref?.startsWith("#/")) return obj(v);
  let at: unknown = spec;
  for (const part of ref.slice(2).split("/")) at = obj(at)[part];
  return obj(at);
}

const refName = (v: unknown) => str(obj(v).$ref)?.split("/").pop();

function contentSchema(spec: Json, holder: unknown): Json | undefined {
  const content = obj(deref(spec, holder).content);
  const first = Object.values(content)[0];
  return first === undefined ? undefined : obj(obj(first).schema);
}

export function operations(spec: Json): Operation[] {
  const out: Operation[] = [];
  for (const [path, rawItem] of Object.entries(obj(spec.paths))) {
    const item = obj(rawItem);
    for (const method of methods) {
      if (!(method in item)) continue;
      const op = obj(item[method]);
      const parameters = [...list(item.parameters), ...list(op.parameters)]
        .map((p) => deref(spec, p))
        .map((p) => ({
          name: str(p.name) ?? "",
          in: str(p.in) ?? "",
          required: p.required === true,
          description: str(p.description),
          schema: obj(p.schema),
        }));
      out.push({
        id: str(op.operationId) ?? `${method} ${path}`,
        method: method.toUpperCase(),
        path,
        tag: str(list(op.tags)[0]) ?? "other",
        description: str(op.description) ?? str(op.summary),
        roles: list(op["x-samuori-roles"]).filter(
          (r): r is string => typeof r === "string",
        ),
        isPublic: op["x-samuori-public"] === true,
        deprecated: op.deprecated === true,
        parameters,
        body: op.requestBody ? contentSchema(spec, op.requestBody) : undefined,
        bodyType: Object.keys(obj(deref(spec, op.requestBody).content))[0],
        responses: Object.entries(obj(op.responses)).map(([status, r]) => ({
          status,
          description: str(deref(spec, r).description) ?? "",
          schema: contentSchema(spec, r),
        })),
      });
    }
  }
  return out;
}

/** Groups by first tag, in the order the contract lists its tags. */
export function groupByTag(
  spec: Json,
  ops: Operation[],
): [string, Operation[]][] {
  const order = list(spec.tags).map((t) => str(obj(t).name) ?? "");
  const groups = new Map<string, Operation[]>();
  for (const t of order) groups.set(t, []);
  for (const op of ops) groups.set(op.tag, [...(groups.get(op.tag) ?? []), op]);
  return [...groups].filter(([, items]) => items.length > 0);
}

/** Keeps operations that match every word of the query in their path, ID, method, tag, roles or description. */
export function search(ops: Operation[], query: string): Operation[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return ops;
  return ops.filter((op) => {
    const text = [
      op.method,
      op.path,
      op.id,
      op.tag,
      op.description,
      ...op.roles,
    ]
      .join(" ")
      .toLowerCase();
    return words.every((w) => text.includes(w));
  });
}

export function rolesLabel(op: Operation): string {
  if (op.isPublic) return "Public: no sign-in needed";
  if (op.roles.includes("any")) return "Any signed-in user or token";
  return op.roles.join(", ");
}

const maxDepth = 4;

/** A readable, TypeScript-like outline of a schema, expanding references a few levels deep. */
export function schemaText(
  spec: Json,
  schema: unknown,
  depth = 0,
  seen: string[] = [],
): string {
  const name = refName(schema);
  if (name) {
    if (seen.includes(name) || depth >= maxDepth) return name;
    return schemaText(spec, deref(spec, schema), depth, [...seen, name]);
  }
  const s = obj(schema);
  const nullable = s.nullable === true ? " | null" : "";
  const join = (key: string, sep: string) =>
    list(s[key])
      .map((x) => schemaText(spec, x, depth, seen))
      .join(sep);
  if (s.allOf) return join("allOf", " & ") + nullable;
  if (s.oneOf) return join("oneOf", " | ") + nullable;
  if (s.anyOf) return join("anyOf", " | ") + nullable;
  if (s.enum)
    return (
      list(s.enum)
        .map((v) => JSON.stringify(v))
        .join(" | ") + nullable
    );
  if (s.type === "array")
    return `${schemaText(spec, s.items, depth, seen)}[]${nullable}`;
  const props = Object.entries(obj(s.properties));
  if (s.type === "object" || props.length > 0) {
    const pad = "  ".repeat(depth + 1);
    const required = new Set(list(s.required));
    const lines = props.map(([key, value]) => {
      const desc = str(deref(spec, value).description);
      const comment = desc ? `  // ${desc.split("\n")[0]}` : "";
      const opt = required.has(key) ? "" : "?";
      return `${pad}${key}${opt}: ${schemaText(spec, value, depth + 1, seen)};${comment}`;
    });
    const extra = s.additionalProperties;
    if (extra && props.length === 0)
      return `{ [key: string]: ${extra === true ? "unknown" : schemaText(spec, extra, depth + 1, seen)} }${nullable}`;
    if (lines.length === 0) return `object${nullable}`;
    return `{\n${lines.join("\n")}\n${"  ".repeat(depth)}}${nullable}`;
  }
  const type = str(s.type) ?? "unknown";
  const format = str(s.format);
  return `${type}${format ? ` (${format})` : ""}${nullable}`;
}

const placeholder = (name: string) =>
  "$" +
  name
    .replace(/([a-z0-9])([A-Z])/g, "$1_$2")
    .replace(/[^A-Za-z0-9]+/g, "_")
    .toUpperCase();

/** A curl command for an API token; path parameters become shell variables such as $PROJECT_ID. */
export function curlExample(op: Operation, origin: string): string {
  const path = op.path.replace(/\{([^}]+)\}/g, (_, n: string) =>
    placeholder(n),
  );
  const query = op.parameters
    .filter((p) => p.in === "query" && p.required)
    .map((p) => `${p.name}=${placeholder(p.name)}`)
    .join("&");
  const lines = [`curl -X ${op.method}`];
  if (!op.isPublic) lines.push(`-H "Authorization: Bearer $SAMUORI_TOKEN"`);
  for (const p of op.parameters) {
    if (p.in === "header" && p.required && p.name !== "X-CSRF-Token")
      lines.push(`-H "${p.name}: ${placeholder(p.name)}"`);
  }
  if (op.bodyType === "application/json")
    lines.push(`-H "Content-Type: application/json"`, `-d @request.json`);
  else if (op.bodyType)
    lines.push(
      `-H "Content-Type: ${op.bodyType}"`,
      `--data-binary @request.body`,
    );
  lines.push(`"${origin}/api/v1${path}${query ? `?${query}` : ""}"`);
  return lines.join(" \\\n  ");
}
