import { useState } from "react";
import { FileTrigger, Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button, buttonClass } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import {
  Alert,
  Badge,
  PageHeader,
  Section,
  StateBadge,
} from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useCan } from "../../session/session";
import { importAction } from "../display";

type Plan = Schemas["ConfigurationImport"];
type Item = Schemas["ConfigurationImportItem"];

const attentionLabel = {
  credential: "Credentials to add",
  secret: "Secret to enter",
  reference: "Missing reference",
  review: "Change by hand",
} as const;

async function sendImport(text: string, apply: boolean) {
  return api
    .POST("/configuration/import", {
      params: {
        query: { dryRun: !apply },
        header: apply ? { "Idempotency-Key": crypto.randomUUID() } : {},
      },
      body: text as never,
      bodySerializer: (b) => b as unknown as string,
      headers: {
        "Content-Type": text.trimStart().startsWith("{")
          ? "application/json"
          : "application/yaml",
      },
    })
    .catch(() => ({
      data: undefined,
      error: { message: "the controller did not answer; try again" },
    }));
}

function problemText(error: unknown): string {
  const details = (error as Schemas["Problem"] | undefined)?.details ?? [];
  const lines = details.map((d) => `${d.field ?? ""}: ${d.reason ?? ""}`);
  return [describeError(error), ...lines].join("\n");
}

export function ConfigurationPage() {
  const can = useCan();
  const [file, setFile] = useState<{ name: string; text: string }>();
  const [plan, setPlan] = useState<Plan>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);

  async function preview(name: string, text: string) {
    setFile({ name, text });
    setPlan(undefined);
    setError(undefined);
    setBusy(true);
    const { data, error } = await sendImport(text, false);
    setBusy(false);
    if (data) setPlan(data);
    else setError(problemText(error));
  }

  async function apply() {
    if (!file) return;
    setConfirming(false);
    setBusy(true);
    setError(undefined);
    const { data, error } = await sendImport(file.text, true);
    setBusy(false);
    if (data) setPlan(data);
    else setError(problemText(error));
  }

  const changes = plan ? plan.summary.create + plan.summary.update : 0;
  const rows = (plan?.items ?? []).map((it, i) => ({ ...it, id: String(i) }));

  return (
    <>
      <PageHeader
        help="configuration"
        title="Export & import"
        description="Copy this organization's configuration to another installation. Secrets, credentials, users, role bindings and projects are never in the file."
      />
      {can("exportConfiguration") && (
        <Section
          title="Export"
          description="Policies, build methods and providers, recipe decisions, custom recipes, worker pools, registries, destinations, notification channels and rules, sign-in and audit settings, as one YAML file."
        >
          <a
            className={buttonClass("secondary")}
            href="/api/v1/configuration/export"
            download
          >
            Download configuration
          </a>
        </Section>
      )}
      {can("importConfiguration") && (
        <Section
          title="Import"
          description="Choose an exported file to see what would change. Nothing changes until you apply it, and nothing that is missing from the file is deleted."
        >
          <FileTrigger
            acceptedFileTypes={[".yml", ".yaml", ".json"]}
            onSelect={(list) => {
              const f = list?.[0];
              if (f) void f.text().then((text) => preview(f.name, text));
            }}
          >
            <Button variant="secondary" isDisabled={busy}>
              Choose file
            </Button>
          </FileTrigger>
          <p className="samuori-muted mt-2 mb-0 text-sm">
            {file
              ? file.name
              : "A .yml or .json file downloaded from Export, named samuori-configuration-….yml."}
          </p>
          {busy && <p className="samuori-muted">Comparing…</p>}
          {error && (
            <Alert>
              <span className="whitespace-pre-line">{error}</span>
            </Alert>
          )}
          {plan && (
            <>
              {plan.dryRun ? (
                <Alert tone="info">
                  Preview{" "}
                  {plan.sourceOrganization
                    ? `of the configuration from ${plan.sourceOrganization}`
                    : ""}
                  : {plan.summary.create} to create, {plan.summary.update} to
                  update, {plan.summary.unchanged} unchanged,{" "}
                  {plan.summary.skip} skipped. Nothing has changed yet.
                </Alert>
              ) : plan.complete ? (
                <Alert tone="success">
                  Imported: {plan.summary.create} created, {plan.summary.update}{" "}
                  updated, {plan.summary.unchanged} unchanged,{" "}
                  {plan.summary.skip} skipped.
                </Alert>
              ) : (
                <Alert>
                  {plan.summary.failed} change(s) failed; the others were
                  applied. The table shows why.
                </Alert>
              )}
              {plan.needsAttention.length > 0 && (
                <Section
                  title="Needs attention"
                  description="Secrets are never exported, and some changes are left for you to make where they are checked."
                >
                  <ul className="samuori-plain-list grid gap-2">
                    {plan.needsAttention.map((x, i) => (
                      <li key={i}>
                        <Badge tone="warn">{attentionLabel[x.kind]}</Badge>{" "}
                        {x.message}{" "}
                        {x.link && (
                          <Link className="samuori-link" href={x.link}>
                            Open
                          </Link>
                        )}
                      </li>
                    ))}
                  </ul>
                </Section>
              )}
              <Table<Item & { id: string }>
                label="Import plan"
                rows={rows}
                empty="The file holds no configuration."
                columns={[
                  {
                    id: "section",
                    name: "Section",
                    render: (it) => it.section,
                    sortValue: (it) => it.section,
                  },
                  {
                    id: "name",
                    name: "Item",
                    isRowHeader: true,
                    render: (it) => it.name,
                  },
                  {
                    id: "action",
                    name: "Change",
                    render: (it) => (
                      <StateBadge state={importAction(it.action)} />
                    ),
                    sortValue: (it) => it.action,
                  },
                  {
                    id: "detail",
                    name: "Details",
                    render: (it) =>
                      it.outcome === "failed" ? (
                        <span className="text-bad">Failed: {it.error}</span>
                      ) : (
                        it.reason
                      ),
                  },
                ]}
              />
              {plan.dryRun && (
                <div className="samuori-actions samuori-actions--start">
                  <Button
                    isDisabled={busy || changes === 0}
                    onPress={() => setConfirming(true)}
                  >
                    Apply import
                  </Button>
                  {changes === 0 && (
                    <span className="samuori-muted">Nothing to change.</span>
                  )}
                </div>
              )}
            </>
          )}
        </Section>
      )}
      <Dialog
        title="Apply this import?"
        size="confirm"
        isOpen={confirming}
        onOpenChange={setConfirming}
      >
        <p>
          {plan?.summary.create ?? 0} item(s) will be created and{" "}
          {plan?.summary.update ?? 0} updated, each checked and audited as if
          you made it by hand. Nothing is deleted. Secrets and credentials still
          have to be entered afterwards.
        </p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => setConfirming(false)}>
            Cancel
          </Button>
          <Button onPress={() => void apply()}>Apply import</Button>
        </div>
      </Dialog>
    </>
  );
}
