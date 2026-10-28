import { useEffect, useRef, useState } from "react";
import { Label, TextArea, TextField } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Alert } from "../components/Feedback";
import type { Doc } from "../spec/model";

type Validation = Schemas["SpecValidation"];

// A draft that has not reached the spec yet (for example step 1) still renders, as the document's skeleton.
function withHeader(doc: Doc): Schemas["BuildSpecDocument"] {
  return {
    apiVersion: "samuori.invalid/v1alpha1",
    kind: "ImageBuild",
    metadata: {},
    spec: {},
    ...doc,
  } as Schemas["BuildSpecDocument"];
}

/** Edits the whole document as YAML; the server parses, validates and returns the canonical form. */
export function YamlEditor({
  doc,
  onApply,
  onDirty,
}: {
  doc: Doc;
  onApply: (d: Doc) => void;
  onDirty: (dirty: boolean) => void;
}) {
  const [text, setText] = useState<string>();
  const [original, setOriginal] = useState("");
  const [result, setResult] = useState<{ text: string; v: Validation }>();
  const [failed, setFailed] = useState<string>();
  const [attempt, setAttempt] = useState(0);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    let live = true;
    api
      .POST("/spec-documents/validate", {
        body: { document: withHeader(doc) },
      })
      .then(({ data, error }) => {
        if (!live) return;
        if (!data) {
          setFailed(describeError(error, "The YAML could not be prepared."));
          return;
        }
        const y = data.canonicalYaml ?? "";
        setFailed(undefined);
        setText(y);
        setOriginal(y);
        setResult({ text: y, v: data as Validation });
      })
      .catch(() => live && setFailed("The server could not be reached."));
    return () => {
      live = false;
    };
  }, [doc, attempt]);

  function change(v: string) {
    setText(v);
    onDirty(v !== original);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      void api
        .POST("/spec-documents/validate", { body: { yaml: v } })
        .then(
          ({ data }) => data && setResult({ text: v, v: data as Validation }),
        );
    }, 400);
  }

  if (text === undefined && failed)
    return (
      <Alert>
        <p>{failed}</p>
        <Button variant="secondary" onPress={() => setAttempt((n) => n + 1)}>
          Try again
        </Button>
      </Alert>
    );
  if (text === undefined)
    return <p className="samuori-muted">Preparing YAML…</p>;
  // A result is used only for the text it was computed from, so a fast Apply never applies stale content.
  const current = result && result.text === text ? result.v : undefined;
  const issues = [
    ...(current?.schemaErrors ?? []),
    ...(current?.semanticErrors ?? []),
  ];
  return (
    <div className="samuori-yaml">
      <TextField
        className="samuori-field"
        value={text}
        onChange={change}
        aria-describedby="yaml-status"
      >
        <Label>BuildSpec YAML</Label>
        <TextArea className="samuori-code" rows={24} spellCheck={false} />
      </TextField>
      <div id="yaml-status" role="status">
        {!current ? (
          <span className="samuori-muted">Checking…</span>
        ) : issues.length === 0 ? (
          <span className="samuori-status samuori-status--ok">Valid</span>
        ) : (
          <ul className="samuori-issue-list">
            {issues.map((i, n) => (
              <li key={n}>
                <code>{i.field || "/"}</code> {i.reason}
              </li>
            ))}
          </ul>
        )}
      </div>
      {current?.document && text !== original && (
        <div className="samuori-actions samuori-actions--start">
          <Button
            onPress={() => {
              onApply(current.document as Doc);
              onDirty(false);
            }}
          >
            Apply YAML changes
          </Button>
          <Button variant="secondary" onPress={() => change(original)}>
            Discard changes
          </Button>
        </div>
      )}
      {current && !current.document && (
        <Alert>
          The YAML cannot be read. Fix the syntax before applying it.
        </Alert>
      )}
    </div>
  );
}
