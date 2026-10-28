import { useState } from "react";
import { type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Checkbox, Select, TextField } from "../components/Field";
import { Section } from "../components/Feedback";

type Question = Schemas["KubeConversionQuestion"];

const other = "Other (specify custom option)";

function list(v: string | undefined): string[] {
  try {
    const parsed: unknown = JSON.parse(v ?? "[]");
    return Array.isArray(parsed) ? parsed.map(String) : [];
  } catch {
    return [];
  }
}

function initial(q: Question, answers?: Record<string, string>): string {
  return answers?.[q.id] ?? q.answer ?? q.default ?? "";
}

/** The provider's questions with the values the last run used; submitting re-runs with the answers (ADR 0048). */
export function KubeQuestions({
  questions,
  answers,
  open,
  canEdit,
  busy,
  onSubmit,
}: {
  questions: Question[];
  answers?: Record<string, string>;
  open: number;
  canEdit: boolean;
  busy: boolean;
  onSubmit: (answers: Record<string, string>) => void;
}) {
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(questions.map((q) => [q.id, initial(q, answers)])),
  );
  const set = (id: string, v: string) => setValues((s) => ({ ...s, [id]: v }));
  return (
    <Section
      title="Questions"
      description={
        open > 0
          ? `Move2Kube answered ${open} of these with its defaults. Samuori checks every answer before the next run; nothing runs interactively.`
          : "Every question has your answer. Change any and run again."
      }
    >
      <div className="samuori-form">
        {questions.map((q) => (
          <QuestionField
            key={q.id}
            q={q}
            value={values[q.id] ?? ""}
            answered={answers?.[q.id] !== undefined}
            isDisabled={!canEdit}
            onChange={(v) => set(q.id, v)}
          />
        ))}
        {canEdit && (
          <div className="samuori-actions">
            <Button onPress={() => onSubmit(values)} isDisabled={busy}>
              Run again with these answers
            </Button>
          </div>
        )}
      </div>
    </Section>
  );
}

function QuestionField({
  q,
  value,
  answered,
  isDisabled,
  onChange,
}: {
  q: Question;
  value: string;
  answered: boolean;
  isDisabled: boolean;
  onChange: (v: string) => void;
}) {
  const hints = [
    ...(q.hints ?? []),
    q.default !== undefined && q.default !== ""
      ? `Move2Kube's default: ${q.kind === "MultiSelect" ? list(q.default).join(", ") : q.default}.`
      : "",
    answered ? "" : "Not answered yet: the last run used the default.",
  ]
    .filter(Boolean)
    .join(" ");
  const options = q.options ?? [];
  switch (q.kind) {
    case "Select": {
      const custom = options.includes(other) && !options.includes(value);
      return (
        <>
          <Select
            label={q.text}
            options={options.map((o) => ({ id: o, label: o }))}
            value={custom ? other : value || null}
            onChange={(v) => onChange(v === other ? "" : v)}
            description={hints}
            isDisabled={isDisabled}
          />
          {custom && (
            <TextField
              label="Your value"
              value={value}
              onChange={onChange}
              isDisabled={isDisabled}
            />
          )}
        </>
      );
    }
    case "MultiSelect": {
      const picked = list(value);
      return (
        <fieldset className="samuori-field">
          <legend className="samuori-field-label">{q.text}</legend>
          {options.map((o) => (
            <Checkbox
              key={o}
              isSelected={picked.includes(o)}
              isDisabled={isDisabled}
              onChange={(on) =>
                onChange(
                  JSON.stringify(
                    on ? [...picked, o] : picked.filter((p) => p !== o),
                  ),
                )
              }
            >
              {o}
            </Checkbox>
          ))}
          {hints && <p className="samuori-muted">{hints}</p>}
        </fieldset>
      );
    }
    case "Confirm":
      return (
        <Checkbox
          isSelected={value === "true"}
          isDisabled={isDisabled}
          onChange={(on) => onChange(on ? "true" : "false")}
        >
          {q.text}
        </Checkbox>
      );
    default:
      return (
        <TextField
          label={q.text}
          value={value}
          onChange={onChange}
          description={hints}
          multiline={q.kind === "MultiLineInput"}
          isDisabled={isDisabled}
        />
      );
  }
}
