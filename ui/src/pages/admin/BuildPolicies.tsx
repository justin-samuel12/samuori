import { useCallback, useState, type FormEvent, type ReactNode } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Checkbox, Select, TextField } from "../../components/Field";
import { Alert, Loading, PageHeader, Section } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";
import { ConfirmPolicy } from "./SystemPages";
import {
  buildSecretClass,
  frontendProblem,
  outcomes,
  put,
  ruleIdProblem,
  sensitivePathProblem,
  without,
} from "./policyRules";

type Policy = Schemas["Policy"];
type Kind = "frontends" | "build-args" | "validation" | "sensitive-content";

type FrontendsDoc = { approved: string[] };
type ArgRule = {
  id: string;
  namePattern?: string;
  valuePattern?: string;
  reason: string;
};
type BuildArgsDoc = { rules: ArgRule[] };
type ValidationDoc = {
  requireSbom: boolean;
  requireRuntimeTest: boolean;
  requireVulnerabilityScan?: boolean;
  failOnSeverity?: string;
  ignoreVulnerabilities?: string[];
};
type SensitiveRule = {
  id: string;
  class: string;
  path: string;
  reason: string;
};
type SensitiveDoc = {
  outcomes: Record<string, string>;
  extraRules: SensitiveRule[];
};

/** Administrators' editors for the build policies that have no other page (recipes-and-policies.md#policies). */
export function BuildPoliciesPage() {
  const [items, setItems] = useState<Policy[] | null>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/policies");
    setItems(data?.items ?? null);
  }, []);
  useLoader(load);
  if (items === undefined) return <Loading />;
  if (items === null) return <Alert>The policies could not be loaded.</Alert>;
  const of = (kind: Kind) => items.find((p) => p.kind === kind);
  const keyOf = (p: Policy) => `${p.id}:${p.version}`;
  const frontends = of("frontends");
  const buildArgs = of("build-args");
  const validation = of("validation");
  const sensitive = of("sensitive-content");
  return (
    <>
      <PageHeader
        title="Build policies"
        description="Rules every project's builds follow. Each save is a new, audited version."
        help="build-policies"
      />
      <p className="samuori-muted">
        Base images are on Recipes &amp; base images, network egress on Network
        egress and image downloads on Registries.
      </p>
      {frontends && (
        <FrontendsEditor
          key={keyOf(frontends)}
          policy={frontends}
          onSaved={load}
        />
      )}
      {buildArgs && (
        <BuildArgsEditor
          key={keyOf(buildArgs)}
          policy={buildArgs}
          onSaved={load}
        />
      )}
      {validation && (
        <ValidationEditor
          key={keyOf(validation)}
          policy={validation}
          onSaved={load}
        />
      )}
      {sensitive && (
        <SensitiveEditor
          key={keyOf(sensitive)}
          policy={sensitive}
          onSaved={load}
        />
      )}
    </>
  );
}

function PolicySection({
  title,
  description,
  policy,
  document,
  dirty,
  onSaved,
  children,
}: {
  title: string;
  description: ReactNode;
  policy: Policy;
  document: object;
  dirty: boolean;
  onSaved: () => void;
  children: ReactNode;
}) {
  const kind = policy.kind;
  const [confirming, setConfirming] = useState<{
    summary: string;
    token: string;
  }>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function save() {
    setError(undefined);
    setBusy(true);
    const { data, error } = await api.POST("/policies/{policyId}/dry-run", {
      params: { path: { policyId: policy.id } },
      body: { kind, document: { ...document } },
    });
    if (error) {
      setBusy(false);
      setError(describeError(error));
      return;
    }
    if (data.confirmation) {
      setBusy(false);
      setConfirming({ summary: data.summary ?? "", token: data.confirmation });
      return;
    }
    await apply();
  }

  async function apply(token?: string, reason?: string) {
    setBusy(true);
    const { error } = await api.PUT("/policies/{policyId}", {
      params: {
        path: { policyId: policy.id },
        header: {
          "If-Match": `"${policy.version}"`,
          ...(token ? { "X-Samuori-Confirm": token } : {}),
        },
      },
      body: { kind, document: { ...document }, reason },
    });
    setBusy(false);
    if (error) setError(describeError(error));
    else {
      setConfirming(undefined);
      onSaved();
    }
  }

  return (
    <Section title={title} description={description}>
      <p className="samuori-muted text-xs">
        Version {policy.version}
        {policy.updatedAt &&
          `, saved ${new Date(policy.updatedAt).toLocaleString()}`}
        {policy.updatedBy && ` by ${policy.updatedBy}`}.
        {dirty && " Unsaved changes."}
      </p>
      {children}
      {error && <Alert>{error}</Alert>}
      <div className="samuori-actions samuori-actions--start">
        <Button isDisabled={!dirty || busy} onPress={() => void save()}>
          Save {title.toLowerCase()}
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

const same = (a: unknown, b: unknown) =>
  JSON.stringify(a) === JSON.stringify(b);

function FrontendsEditor({
  policy,
  onSaved,
}: {
  policy: Policy;
  onSaved: () => void;
}) {
  const saved = policy.document as FrontendsDoc;
  const [approved, setApproved] = useState(saved.approved);
  const [editing, setEditing] = useState<number>();
  return (
    <PolicySection
      title="Dockerfile frontends"
      description={
        <>
          Frontends a Dockerfile may name in its <code># syntax=</code> line.
          Each is pinned by digest; a Dockerfile naming any other frontend is
          refused before it is scheduled. Dockerfiles without the line are not
          affected.
        </>
      }
      policy={policy}
      document={{ approved }}
      dirty={!same(approved, saved.approved)}
      onSaved={onSaved}
    >
      <Table
        label="Approved frontends"
        rows={approved.map((ref, i) => ({ id: String(i), ref, i }))}
        empty="No frontends approved. Choose Add frontend to allow one."
        columns={[
          {
            id: "ref",
            name: "Frontend",
            isRowHeader: true,
            render: (r) => <code className="break-all">{r.ref}</code>,
          },
          {
            id: "actions",
            name: "Actions",
            render: (r) => (
              <span className="inline-flex gap-1">
                <Button
                  variant="quiet"
                  aria-label={`Edit ${r.ref}`}
                  onPress={() => setEditing(r.i)}
                >
                  Edit
                </Button>
                <Button
                  variant="quiet-danger"
                  aria-label={`Remove ${r.ref}`}
                  onPress={() => setApproved(without(approved, r.i))}
                >
                  Remove
                </Button>
              </span>
            ),
          },
        ]}
        dependencies={[approved]}
      />
      <div className="samuori-actions">
        <Button variant="secondary" onPress={() => setEditing(approved.length)}>
          Add frontend
        </Button>
      </div>
      {editing !== undefined && (
        <EntryDialog
          title={editing < approved.length ? "Edit frontend" : "Add frontend"}
          onClose={() => setEditing(undefined)}
          onDone={(values) => {
            setApproved(put(approved, editing, values.ref?.trim() ?? ""));
            setEditing(undefined);
          }}
          fields={[
            {
              name: "ref",
              label: "Frontend image",
              initial: approved[editing] ?? "",
              placeholder: "docker.io/docker/dockerfile@sha256:…",
              description:
                "Find the digest with docker buildx imagetools inspect docker/dockerfile:1.",
              check: (v) =>
                frontendProblem(v) ??
                (approved.some((a, i) => i !== editing && a === v.trim())
                  ? "This frontend is already approved."
                  : undefined),
            },
          ]}
        />
      )}
    </PolicySection>
  );
}

function BuildArgsEditor({
  policy,
  onSaved,
}: {
  policy: Policy;
  onSaved: () => void;
}) {
  const saved = policy.document as BuildArgsDoc;
  const [rules, setRules] = useState(saved.rules);
  const [editing, setEditing] = useState<number>();
  const current = editing !== undefined ? rules[editing] : undefined;
  return (
    <PolicySection
      title="Build arguments"
      description="Refuse build arguments whose name or value matches a pattern, such as names that look like credentials. Patterns are Go regular expressions; a rule needs at least one."
      policy={policy}
      document={{ rules }}
      dirty={!same(rules, saved.rules)}
      onSaved={onSaved}
    >
      <Table
        label="Build argument rules"
        rows={rules.map((r, i) => ({ ...r, id: String(i), i }))}
        empty="No rules: every build argument is accepted."
        columns={[
          {
            id: "rule",
            name: "Rule",
            isRowHeader: true,
            render: (r) => <code>{r.id}</code>,
          },
          {
            id: "name",
            name: "Name pattern",
            render: (r) => r.namePattern && <code>{r.namePattern}</code>,
          },
          {
            id: "value",
            name: "Value pattern",
            render: (r) => r.valuePattern && <code>{r.valuePattern}</code>,
          },
          { id: "reason", name: "Reason", render: (r) => r.reason },
          {
            id: "actions",
            name: "Actions",
            render: (r) => (
              <span className="inline-flex gap-1">
                <Button
                  variant="quiet"
                  aria-label={`Edit rule ${r.id}`}
                  onPress={() => setEditing(r.i)}
                >
                  Edit
                </Button>
                <Button
                  variant="quiet-danger"
                  aria-label={`Remove rule ${r.id}`}
                  onPress={() => setRules(without(rules, r.i))}
                >
                  Remove
                </Button>
              </span>
            ),
          },
        ]}
        dependencies={[rules]}
      />
      <div className="samuori-actions">
        <Button variant="secondary" onPress={() => setEditing(rules.length)}>
          Add rule
        </Button>
      </div>
      {editing !== undefined && (
        <EntryDialog
          title={current ? `Edit rule ${current.id}` : "Add rule"}
          onClose={() => setEditing(undefined)}
          check={(v) =>
            v.namePattern?.trim() || v.valuePattern?.trim()
              ? undefined
              : "Enter a name pattern, a value pattern or both."
          }
          onDone={(v) => {
            const rule: ArgRule = {
              id: v.id?.trim() ?? "",
              reason: v.reason?.trim() ?? "",
            };
            if (v.namePattern?.trim()) rule.namePattern = v.namePattern.trim();
            if (v.valuePattern?.trim())
              rule.valuePattern = v.valuePattern.trim();
            setRules(put(rules, editing, rule));
            setEditing(undefined);
          }}
          fields={[
            {
              name: "id",
              label: "Rule ID",
              initial: current?.id ?? "",
              check: (v) =>
                ruleIdProblem(
                  v.trim(),
                  rules.filter((_, i) => i !== editing).map((r) => r.id),
                ),
            },
            {
              name: "namePattern",
              label: "Name pattern (optional)",
              initial: current?.namePattern ?? "",
              placeholder: "(?i)(token|password|secret)",
            },
            {
              name: "valuePattern",
              label: "Value pattern (optional)",
              initial: current?.valuePattern ?? "",
            },
            {
              name: "reason",
              label: "Reason",
              initial: current?.reason ?? "",
              description: "Shown to developers whose build is refused.",
              check: (v) => (v.trim() ? undefined : "Enter a reason."),
            },
          ]}
        />
      )}
    </PolicySection>
  );
}

const severityOptions = [
  { id: "none", label: "Do not fail on findings" },
  { id: "critical", label: "Critical" },
  { id: "high", label: "High or above" },
  { id: "medium", label: "Medium or above" },
  { id: "low", label: "Any severity" },
];

function ValidationEditor({
  policy,
  onSaved,
}: {
  policy: Policy;
  onSaved: () => void;
}) {
  const saved = policy.document as ValidationDoc;
  const [doc, setDoc] = useState<ValidationDoc>(saved);
  const [ignoreText, setIgnoreText] = useState(
    (saved.ignoreVulnerabilities ?? []).join("\n"),
  );
  const ignore = ignoreText
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
  const next: ValidationDoc = {
    requireSbom: doc.requireSbom,
    requireRuntimeTest: doc.requireRuntimeTest,
    ...(doc.requireVulnerabilityScan ? { requireVulnerabilityScan: true } : {}),
    ...(doc.failOnSeverity ? { failOnSeverity: doc.failOnSeverity } : {}),
    ...(ignore.length ? { ignoreVulnerabilities: ignore } : {}),
  };
  const normalized: ValidationDoc = {
    requireSbom: saved.requireSbom,
    requireRuntimeTest: saved.requireRuntimeTest,
    ...(saved.requireVulnerabilityScan
      ? { requireVulnerabilityScan: true }
      : {}),
    ...(saved.failOnSeverity ? { failOnSeverity: saved.failOnSeverity } : {}),
    ...(saved.ignoreVulnerabilities?.length
      ? { ignoreVulnerabilities: saved.ignoreVulnerabilities }
      : {}),
  };
  return (
    <PolicySection
      title="Validation"
      description="Which checks a validation run needs before an image is ready. A required check that is skipped fails the run."
      policy={policy}
      document={next}
      dirty={!same(next, normalized)}
      onSaved={onSaved}
    >
      <Checkbox
        isSelected={doc.requireRuntimeTest}
        onChange={(v) => setDoc({ ...doc, requireRuntimeTest: v })}
      >
        Require a passing runtime test
      </Checkbox>
      <Checkbox
        isSelected={doc.requireSbom}
        onChange={(v) => setDoc({ ...doc, requireSbom: v })}
      >
        Require a software bill of materials (SBOM)
      </Checkbox>
      <Checkbox
        isSelected={!!doc.requireVulnerabilityScan}
        onChange={(v) => setDoc({ ...doc, requireVulnerabilityScan: v })}
      >
        Require a vulnerability scan
      </Checkbox>
      <p className="samuori-muted text-xs">
        The scan needs a scanner installed on the controller; without one a
        required scan fails closed.
      </p>
      <Select
        label="Fail the scan on findings at"
        options={severityOptions}
        value={doc.failOnSeverity || "none"}
        onChange={(v) =>
          setDoc({ ...doc, failOnSeverity: v === "none" ? undefined : v })
        }
      />
      <TextField
        label="Ignored vulnerabilities"
        multiline
        value={ignoreText}
        onChange={setIgnoreText}
        description="One ID per line, such as CVE-2024-12345. Reported as ignored, never hidden."
      />
    </PolicySection>
  );
}

const outcomeLabels: Record<string, string> = {
  warn: "Warn",
  "require-exception": "Require an exception",
  block: "Block",
};

function SensitiveEditor({
  policy,
  onSaved,
}: {
  policy: Policy;
  onSaved: () => void;
}) {
  const saved = policy.document as SensitiveDoc;
  const [byClass, setByClass] = useState(saved.outcomes);
  const [rules, setRules] = useState(saved.extraRules);
  const [editing, setEditing] = useState<number>();
  const classes = Object.keys(saved.outcomes).sort();
  const ruleClasses = classes.filter((c) => c !== buildSecretClass);
  const current = editing !== undefined ? rules[editing] : undefined;
  return (
    <PolicySection
      title="Sensitive content"
      description="What validation does when the image contains files that look like keys, credentials or database data (ADR 0031). Secret bytes delivered to the build always block."
      policy={policy}
      document={{ outcomes: byClass, extraRules: rules }}
      dirty={!same(byClass, saved.outcomes) || !same(rules, saved.extraRules)}
      onSaved={onSaved}
    >
      <h3 className="mt-0">Outcome per rule class</h3>
      <Table
        label="Outcome per rule class"
        rows={classes.map((c) => ({ id: c }))}
        columns={[
          {
            id: "class",
            name: "Class",
            isRowHeader: true,
            render: (r) => <code>{r.id}</code>,
          },
          {
            id: "outcome",
            name: "Outcome",
            render: (r) =>
              r.id === buildSecretClass ? (
                "Block (can't change)"
              ) : (
                <Select
                  label={`Outcome for ${r.id}`}
                  hideLabel
                  options={outcomes.map((o) => ({
                    id: o,
                    label: outcomeLabels[o] ?? o,
                  }))}
                  value={byClass[r.id] ?? null}
                  onChange={(v) => setByClass({ ...byClass, [r.id]: v })}
                />
              ),
          },
        ]}
        dependencies={[byClass]}
      />
      <h3>Extra path rules</h3>
      <Table
        label="Extra path rules"
        rows={rules.map((r, i) => ({ ...r, id: String(i), ruleId: r.id, i }))}
        empty="No extra rules."
        columns={[
          {
            id: "rule",
            name: "Rule",
            isRowHeader: true,
            render: (r) => <code>{r.ruleId}</code>,
          },
          { id: "class", name: "Class", render: (r) => r.class },
          { id: "path", name: "Path", render: (r) => <code>{r.path}</code> },
          { id: "reason", name: "Reason", render: (r) => r.reason },
          {
            id: "actions",
            name: "Actions",
            render: (r) => (
              <span className="inline-flex gap-1">
                <Button
                  variant="quiet"
                  aria-label={`Edit rule ${r.ruleId}`}
                  onPress={() => setEditing(r.i)}
                >
                  Edit
                </Button>
                <Button
                  variant="quiet-danger"
                  aria-label={`Remove rule ${r.ruleId}`}
                  onPress={() => setRules(without(rules, r.i))}
                >
                  Remove
                </Button>
              </span>
            ),
          },
        ]}
        dependencies={[rules]}
      />
      <div className="samuori-actions">
        <Button variant="secondary" onPress={() => setEditing(rules.length)}>
          Add path rule
        </Button>
      </div>
      {editing !== undefined && (
        <EntryDialog
          title={current ? `Edit rule ${current.id}` : "Add path rule"}
          onClose={() => setEditing(undefined)}
          onDone={(v) => {
            setRules(
              put(rules, editing, {
                id: v.id?.trim() ?? "",
                class: v.class ?? "",
                path: v.path?.trim() ?? "",
                reason: v.reason?.trim() ?? "",
              }),
            );
            setEditing(undefined);
          }}
          fields={[
            {
              name: "id",
              label: "Rule ID",
              initial: current?.id ?? "",
              check: (v) =>
                ruleIdProblem(
                  v.trim(),
                  rules.filter((_, i) => i !== editing).map((r) => r.id),
                ),
            },
            {
              name: "class",
              label: "Class",
              initial: current?.class ?? ruleClasses[0] ?? "",
              options: ruleClasses,
            },
            {
              name: "path",
              label: "Path pattern",
              initial: current?.path ?? "",
              placeholder: "srv/config/*.env",
              description: "A glob relative to the image root.",
              check: sensitivePathProblem,
            },
            {
              name: "reason",
              label: "Reason",
              initial: current?.reason ?? "",
              check: (v) => (v.trim() ? undefined : "Enter a reason."),
            },
          ]}
        />
      )}
    </PolicySection>
  );
}

interface EntryField {
  name: string;
  label: string;
  initial: string;
  placeholder?: string;
  description?: string;
  options?: string[];
  check?: (value: string) => string | undefined;
}

/** Edits one list entry locally; nothing is saved until the policy itself is saved. */
function EntryDialog({
  title,
  fields,
  check,
  onClose,
  onDone,
}: {
  title: string;
  fields: EntryField[];
  check?: (values: Record<string, string>) => string | undefined;
  onClose: () => void;
  onDone: (values: Record<string, string>) => void;
}) {
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(fields.map((f) => [f.name, f.initial])),
  );
  const [shown, setShown] = useState(false);
  const problems = Object.fromEntries(
    fields.map((f) => [f.name, f.check?.(values[f.name] ?? "")]),
  );
  const overall = check?.(values);
  const ok = !overall && Object.values(problems).every((p) => !p);
  function submit(e: FormEvent) {
    e.preventDefault();
    setShown(true);
    if (ok) onDone(values);
  }
  return (
    <Dialog title={title} isOpen onOpenChange={(o) => !o && onClose()}>
      <Form className="samuori-form" onSubmit={submit}>
        {fields.map((f) =>
          f.options ? (
            <Select
              key={f.name}
              label={f.label}
              options={f.options.map((o) => ({ id: o, label: o }))}
              value={values[f.name] ?? null}
              onChange={(v) => setValues({ ...values, [f.name]: v })}
            />
          ) : (
            <TextField
              key={f.name}
              label={f.label}
              placeholder={f.placeholder}
              description={f.description}
              value={values[f.name] ?? ""}
              onChange={(v) => setValues({ ...values, [f.name]: v })}
              isInvalid={shown && !!problems[f.name]}
              errorMessage={problems[f.name]}
            />
          ),
        )}
        {shown && overall && <Alert>{overall}</Alert>}
        <p className="samuori-muted text-xs">
          Changes apply when you save the policy.
        </p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Done</Button>
        </div>
      </Form>
    </Dialog>
  );
}
