import { createContext, useContext, useState, type ReactNode } from "react";
import { ArrowDown, ArrowUp, Plus, X } from "lucide-react";
import { Button } from "../components/Button";
import { Checkbox, Select, TextField, type Option } from "../components/Field";
import { get, pathKey, type Doc, type Mode, type Path } from "../spec/model";
import {
  commandLines,
  commandValue,
  listLines,
  listValue,
  move,
} from "../spec/lists";

interface Editor {
  doc: Doc;
  mode: Mode;
  edit: (path: Path, value: unknown) => void;
  readOnly: boolean;
}

export const EditorContext = createContext<Editor | null>(null);

export function useEditor(): Editor {
  const e = useContext(EditorContext);
  if (!e) throw new Error("useEditor outside EditorContext");
  return e;
}

interface FieldProps {
  path: Path;
  label: string;
  description?: string;
  placeholder?: string;
  isRequired?: boolean;
  /** Corrects the text while it is typed (done false) and when the field is left (done true). */
  normalize?: (text: string, done: boolean) => string;
}

function Locked({ label, value }: { label: string; value: unknown }) {
  return (
    <div className="samuori-field">
      <span className="samuori-field-label">{label}</span>
      <code className="samuori-locked">{JSON.stringify(value)}</code>
      <span className="samuori-muted">
        The form cannot edit this value as written. Switch to YAML to edit it.
      </span>
    </div>
  );
}

/** A text field. Argument lists are shown locked so the form never rewrites them as shell strings. */
export function TextAt({
  path,
  label,
  description,
  placeholder,
  isRequired,
  normalize,
}: FieldProps) {
  const { doc, edit, readOnly } = useEditor();
  const v = get(doc, path);
  if (Array.isArray(v)) return <Locked label={label} value={v} />;
  return (
    <TextField
      label={label}
      description={description}
      placeholder={placeholder}
      isRequired={isRequired}
      value={(v as string | undefined) ?? ""}
      onChange={(x) => edit(path, normalize ? normalize(x, false) : x)}
      onBlur={() => {
        if (normalize && typeof v === "string") edit(path, normalize(v, true));
      }}
      isReadOnly={readOnly}
      name={pathKey(path)}
    />
  );
}

export function NumberAt({ path, label, description, isRequired }: FieldProps) {
  const { doc, edit, readOnly } = useEditor();
  const v = get(doc, path);
  return (
    <TextField
      label={label}
      description={description}
      isRequired={isRequired}
      inputMode="numeric"
      pattern="[0-9]*"
      value={v === undefined ? "" : String(v)}
      onChange={(x) =>
        edit(path, x === "" ? undefined : /^\d+$/.test(x) ? Number(x) : x)
      }
      isReadOnly={readOnly}
      name={pathKey(path)}
    />
  );
}

export function SelectAt({
  path,
  label,
  options,
  description,
}: FieldProps & { options: Option[] }) {
  const { doc, edit } = useEditor();
  return (
    <Select
      label={label}
      options={options}
      value={(get(doc, path) as string | undefined) ?? null}
      onChange={(v) => edit(path, v)}
      description={description}
    />
  );
}

export function CheckAt({
  path,
  children,
}: {
  path: Path;
  children: ReactNode;
}) {
  const { doc, edit } = useEditor();
  return (
    <Checkbox
      isSelected={get(doc, path) === true}
      onChange={(v) => edit(path, v ? true : undefined)}
    >
      {children}
    </Checkbox>
  );
}

/**
 * Rows edited in the form before they are valid document values, such as an added empty line. They are kept while
 * the document still holds what they produced, and replaced by the document's value once anything else changes it.
 */
function useRows<R>(
  path: Path,
  toRows: (v: unknown) => R[] | null,
  toValue: (rows: R[]) => unknown,
) {
  const { doc, edit } = useEditor();
  const value = get(doc, path);
  const key = JSON.stringify(value ?? null);
  const [local, setLocal] = useState<{ key: string; rows: R[] } | null>(null);
  const rows = local && local.key === key ? local.rows : toRows(value);
  const change = (next: R[]) => {
    const v = toValue(next);
    const nextKey = JSON.stringify(v ?? null);
    setLocal({ key: nextKey, rows: next });
    if (nextKey !== key) edit(path, v);
  };
  return { value, rows, change };
}

function RowButtons({
  label,
  index,
  count,
  onMove,
  onRemove,
}: {
  label: string;
  index: number;
  count: number;
  onMove?: (by: number) => void;
  onRemove: () => void;
}) {
  return (
    <div className="samuori-line-buttons">
      {onMove && (
        <>
          <Button
            variant="quiet"
            aria-label={`Move ${label} up`}
            isDisabled={index === 0}
            onPress={() => onMove(-1)}
          >
            <ArrowUp size={16} aria-hidden="true" />
          </Button>
          <Button
            variant="quiet"
            aria-label={`Move ${label} down`}
            isDisabled={index === count - 1}
            onPress={() => onMove(1)}
          >
            <ArrowDown size={16} aria-hidden="true" />
          </Button>
        </>
      )}
      <Button variant="quiet" aria-label={`Remove ${label}`} onPress={onRemove}>
        <X size={16} aria-hidden="true" />
      </Button>
    </div>
  );
}

interface LinesProps {
  path: Path;
  label: string;
  /** Names one entry for screen readers, for example "install command". */
  item: string;
  description?: string;
  placeholder?: string;
  addLabel: string;
  /** Commands keep their order and are saved as one shell string; other lists are saved as arrays. */
  commands?: boolean;
  isRequired?: boolean;
}

/** An ordered list of single-line entries with Add, Remove and Move up/down. */
export function LinesAt({
  path,
  label,
  item,
  description,
  placeholder,
  addLabel,
  commands,
  isRequired,
}: LinesProps) {
  const { value, rows, change } = useRows<string>(
    path,
    commands ? commandLines : listLines,
    commands ? commandValue : listValue,
  );
  if (rows === null) return <Locked label={label} value={value} />;
  const shown = rows.length ? rows : [""];
  return (
    <fieldset
      className="samuori-fieldset"
      data-testid={`lines-${pathKey(path)}`}
    >
      <legend>
        {label}
        {isRequired && <span aria-hidden="true"> *</span>}
      </legend>
      {description && (
        <p className="samuori-muted samuori-line-help">{description}</p>
      )}
      <ol className="samuori-line-list">
        {shown.map((line, i) => (
          <li key={i} className="samuori-line-row">
            <TextField
              label={`${item} ${i + 1}`}
              hideLabel
              placeholder={i === 0 ? placeholder : undefined}
              value={line}
              onChange={(x) => change(shown.map((l, j) => (j === i ? x : l)))}
            />
            <RowButtons
              label={`${item} ${i + 1}`}
              index={i}
              count={shown.length}
              onMove={commands ? (by) => change(move(shown, i, by)) : undefined}
              onRemove={() => change(shown.filter((_, j) => j !== i))}
            />
          </li>
        ))}
      </ol>
      <div>
        <Button variant="quiet" onPress={() => change([...shown, ""])}>
          <Plus size={16} aria-hidden="true" /> {addLabel}
        </Button>
      </div>
    </fieldset>
  );
}

type Pair = [string, string];

const pairsOf = (v: unknown): Pair[] | null =>
  v === undefined
    ? []
    : v !== null && typeof v === "object" && !Array.isArray(v)
      ? Object.entries(v).map(([k, x]) => [k, String(x)])
      : null;

const mapOf = (rows: Pair[]) => {
  const kept = rows.filter(([k]) => k.trim());
  return kept.length
    ? Object.fromEntries(kept.map(([k, x]) => [k.trim(), x]))
    : undefined;
};

/** A name → value map, such as image labels. */
export function PairsAt({
  path,
  label,
  item,
  description,
}: {
  path: Path;
  label: string;
  item: string;
  description?: string;
}) {
  const { value, rows, change } = useRows<Pair>(path, pairsOf, mapOf);
  if (rows === null) return <Locked label={label} value={value} />;
  return (
    <fieldset
      className="samuori-fieldset"
      data-testid={`pairs-${pathKey(path)}`}
    >
      <legend>{label}</legend>
      {description && (
        <p className="samuori-muted samuori-line-help">{description}</p>
      )}
      {rows.length > 0 && (
        <ul className="samuori-line-list">
          {rows.map(([k, x], i) => (
            <li key={i} className="samuori-line-row">
              <TextField
                label={`${item} ${i + 1} name`}
                hideLabel
                placeholder="name"
                value={k}
                onChange={(n) =>
                  change(rows.map((r, j) => (j === i ? [n, r[1]] : r)))
                }
              />
              <TextField
                label={`${item} ${i + 1} value`}
                hideLabel
                placeholder="value"
                value={x}
                onChange={(n) =>
                  change(rows.map((r, j) => (j === i ? [r[0], n] : r)))
                }
              />
              <RowButtons
                label={`${item} ${i + 1}`}
                index={i}
                count={rows.length}
                onRemove={() => change(rows.filter((_, j) => j !== i))}
              />
            </li>
          ))}
        </ul>
      )}
      <div>
        <Button variant="quiet" onPress={() => change([...rows, ["", ""]])}>
          <Plus size={16} aria-hidden="true" /> Add {item}
        </Button>
      </div>
    </fieldset>
  );
}

interface BuildArg {
  name: string;
  value: string;
  confirmedNotSecret?: boolean;
}

// Mirrors the BuildSpec schema's rule for argument names that need confirmedNotSecret.
const secretLike = /(pass(word|wd)|secret|token|credential|(^|_)key$)/i;

const argsOf = (v: unknown): BuildArg[] | null =>
  v === undefined
    ? []
    : Array.isArray(v) &&
        v.every((a) => a && typeof a === "object" && !Array.isArray(a))
      ? (v as BuildArg[])
      : null;

const argsValue = (rows: BuildArg[]) => {
  const kept = rows
    .filter((a) => a.name.trim())
    .map((a) => ({
      name: a.name.trim(),
      value: a.value ?? "",
      ...(a.confirmedNotSecret ? { confirmedNotSecret: true } : {}),
    }));
  return kept.length ? kept : undefined;
};

/** Non-secret build arguments; names that look like secrets need an explicit confirmation. */
export function BuildArgsAt({ path }: { path: Path }) {
  const { value, rows, change } = useRows<BuildArg>(path, argsOf, argsValue);
  const label = "Build arguments (optional)";
  if (rows === null) return <Locked label={label} value={value} />;
  const set = (i: number, patch: Partial<BuildArg>) =>
    change(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  return (
    <fieldset className="samuori-fieldset" data-testid="build-args">
      <legend>{label}</legend>
      <p className="samuori-muted samuori-line-help">
        Values stay visible in the image history; never put a password or token
        here. Use a secret instead.
      </p>
      {rows.length > 0 && (
        <ul className="samuori-line-list">
          {rows.map((a, i) => (
            <li key={i} className="samuori-line-row samuori-line-row--wrap">
              <TextField
                label={`Build argument ${i + 1} name`}
                hideLabel
                placeholder="NAME"
                value={a.name}
                onChange={(n) => set(i, { name: n })}
              />
              <TextField
                label={`Build argument ${i + 1} value`}
                hideLabel
                placeholder="value"
                value={a.value}
                onChange={(n) => set(i, { value: n })}
              />
              <RowButtons
                label={`build argument ${i + 1}`}
                index={i}
                count={rows.length}
                onRemove={() => change(rows.filter((_, j) => j !== i))}
              />
              {secretLike.test(a.name) && (
                <Checkbox
                  isSelected={a.confirmedNotSecret === true}
                  onChange={(v) => set(i, { confirmedNotSecret: v })}
                >
                  {a.name} is not a secret
                </Checkbox>
              )}
            </li>
          ))}
        </ul>
      )}
      <div>
        <Button
          variant="quiet"
          onPress={() => change([...rows, { name: "", value: "" }])}
        >
          <Plus size={16} aria-hidden="true" /> Add build argument
        </Button>
      </div>
    </fieldset>
  );
}

const platformChoices = [
  { id: "linux/amd64", label: "linux/amd64 (Intel/AMD)" },
  { id: "linux/arm64", label: "linux/arm64 (ARM)" },
];

/** Target platforms; platforms the form does not offer are kept. */
export function PlatformsAt({
  path,
  single,
}: {
  path: Path;
  /** Railpack and buildpacks build one platform. */
  single?: boolean;
}) {
  const { doc, edit } = useEditor();
  const v = get(doc, path);
  const list = Array.isArray(v) ? (v as string[]) : [];
  return (
    <fieldset className="samuori-fieldset">
      <legend>Processor architectures</legend>
      <p className="samuori-muted samuori-line-help">
        {single
          ? "This build method builds one architecture. "
          : "Choose one or more. "}
        None chosen builds linux/amd64.
      </p>
      {platformChoices.map((p) => (
        <Checkbox
          key={p.id}
          isSelected={list.includes(p.id)}
          onChange={(on) =>
            edit(
              path,
              on
                ? single
                  ? [
                      ...list.filter(
                        (x) => !platformChoices.some((c) => c.id === x),
                      ),
                      p.id,
                    ]
                  : [...list, p.id]
                : list.filter((x) => x !== p.id),
            )
          }
        >
          {p.label}
        </Checkbox>
      ))}
    </fieldset>
  );
}
