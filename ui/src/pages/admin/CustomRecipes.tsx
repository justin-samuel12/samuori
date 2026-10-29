import { useCallback, useState } from "react";
import {
  Label,
  TextArea,
  TextField as AriaTextField,
} from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { TextField } from "../../components/Field";
import { Alert, Loading, StateBadge } from "../../components/Feedback";
import { recipeStatus } from "../display";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type Custom = Schemas["CustomRecipe"];
type Preview = Schemas["CustomRecipePreview"];

// A working starting point: the base image is the one the built-in Node.js recipe pins.
const template = `id: my-recipe
version: 1.0.0
displayName: My recipe
description: What this recipe builds, for the developers who choose it.
platforms: [linux/amd64, linux/arm64]
applicationTypes: [http-service]
egressPresets: [npm]
secrets: []
parameters:
  - name: port
    type: port
    default: "8080"
    description: The port the application listens on.
stages:
  - name: build
    image: docker.io/library/node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392
    steps:
      - workdir: /app
      - copySource: {from: ., to: /app}
      - run: {shell: "npm ci --omit=dev", network: egress}
  - name: final
    image: docker.io/library/node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392
    steps:
      - workdir: /app
      - copyFromStage: {stage: build, from: /app, to: /app, owner: "10001"}
      - env: {name: NODE_ENV, value: production}
      - env: {name: PORT, value: "\${port}"}
      - expose: "\${port}"
      - user: "10001"
      - cmd: [node, server.js]
`;

function nextVersion(v: string): string {
  const [a = "0", b = "0", c = "0"] = v.split(".");
  return `${a}.${b}.${Number(c) + 1}`;
}

/** Administrators' recipes (ADR 0041): edited as YAML, previewed as the Dockerfile Samuori renders, then approved. */
export function CustomRecipes({ onChange }: { onChange: () => void }) {
  const [items, setItems] = useState<Custom[]>();
  const [editing, setEditing] = useState<{ yaml: string; draft?: Custom }>();
  const [discarding, setDiscarding] = useState<Custom>();
  const [deciding, setDeciding] = useState<{
    recipe: Custom;
    action: "approve" | "deprecate" | "withdraw";
  }>();
  const load = useCallback(async () => {
    setItems((await api.GET("/custom-recipes")).data?.items ?? []);
  }, []);
  useLoader(load);
  const done = () => {
    setEditing(undefined);
    setDeciding(undefined);
    setDiscarding(undefined);
    void load();
    onChange();
  };
  return (
    <section className="samuori-section" aria-labelledby="custom-recipes-h">
      <div className="samuori-section-head">
        <div>
          <h2 id="custom-recipes-h" className="m-0">
            Custom recipes
          </h2>
          <p className="samuori-muted m-0 mt-1 max-w-3xl">
            Your organization's own builds: stages from images pinned by digest
            and a fixed set of steps that Samuori turns into a Dockerfile
            itself. Developers see approved versions in New image and fill in
            their parameters. Approved versions can&apos;t change; create a new
            version.
          </p>
        </div>
        <Button
          variant="secondary"
          onPress={() => setEditing({ yaml: template })}
        >
          New recipe
        </Button>
      </div>
      {!items ? (
        <Loading />
      ) : (
        <Table
          label="Custom recipes"
          rows={items.map((r) => ({
            ...r,
            key: r.id,
            id: `${r.id}@${r.version}`,
          }))}
          empty="No custom recipes yet. Choose New recipe to define one."
          columns={[
            {
              id: "name",
              name: "Recipe",
              isRowHeader: true,
              sortValue: (r) => r.id,
              render: (r) => (
                <>
                  <span className="font-medium">{r.displayName}</span>
                  <div className="samuori-muted text-xs">
                    <code>{r.id}</code>
                  </div>
                </>
              ),
            },
            {
              id: "version",
              name: "Version",
              render: (r) => r.version,
            },
            {
              id: "status",
              name: "Status",
              render: (r) => (
                <>
                  <StateBadge state={recipeStatus(r.status)} />
                  {r.reason && (
                    <div className="samuori-muted text-xs">{r.reason}</div>
                  )}
                </>
              ),
            },
            {
              id: "updated",
              name: "Updated",
              sortValue: (r) => r.updatedAt,
              render: (r) => new Date(r.updatedAt).toLocaleString(),
            },
            {
              id: "actions",
              name: "Actions",
              render: (r) => {
                const original = items.find(
                  (x) => `${x.id}@${x.version}` === r.id,
                ) as Custom;
                return (
                  <span className="inline-flex flex-wrap gap-1">
                    {r.status === "draft" ? (
                      <>
                        <Button
                          variant="quiet"
                          aria-label={`Edit ${r.displayName} ${r.version}`}
                          onPress={() =>
                            setEditing({ yaml: r.yaml, draft: original })
                          }
                        >
                          Edit
                        </Button>
                        <Button
                          variant="quiet"
                          aria-label={`Approve ${r.displayName} ${r.version}`}
                          onPress={() =>
                            setDeciding({ recipe: original, action: "approve" })
                          }
                        >
                          Approve
                        </Button>
                        <Button
                          variant="quiet-danger"
                          aria-label={`Discard draft ${r.displayName} ${r.version}`}
                          onPress={() => setDiscarding(original)}
                        >
                          Discard draft
                        </Button>
                      </>
                    ) : (
                      <Button
                        variant="quiet"
                        aria-label={`New version of ${r.displayName}`}
                        onPress={() =>
                          setEditing({
                            yaml: r.yaml.replace(
                              /^version: .*$/m,
                              `version: ${nextVersion(r.version)}`,
                            ),
                          })
                        }
                      >
                        New version
                      </Button>
                    )}
                    {r.status === "approved" && (
                      <Button
                        variant="quiet-danger"
                        onPress={() =>
                          setDeciding({ recipe: original, action: "deprecate" })
                        }
                      >
                        Deprecate
                      </Button>
                    )}
                    {(r.status === "approved" || r.status === "deprecated") && (
                      <Button
                        variant="quiet-danger"
                        onPress={() =>
                          setDeciding({ recipe: original, action: "withdraw" })
                        }
                      >
                        Withdraw
                      </Button>
                    )}
                  </span>
                );
              },
            },
          ]}
          dependencies={[items]}
        />
      )}
      {editing && (
        <RecipeEditor
          initial={editing.yaml}
          draft={editing.draft}
          onClose={() => setEditing(undefined)}
          onSaved={done}
        />
      )}
      {discarding && (
        <DiscardDialog
          recipe={discarding}
          onClose={() => setDiscarding(undefined)}
          onDone={done}
        />
      )}
      {deciding && (
        <DecideDialog
          recipe={deciding.recipe}
          action={deciding.action}
          onClose={() => setDeciding(undefined)}
          onDone={done}
        />
      )}
    </section>
  );
}

async function preview(yaml: string): Promise<Preview> {
  const { data, error } = await api.POST("/custom-recipes/preview", {
    body: { yaml },
  });
  return (
    data ?? {
      valid: false,
      problems: [
        { path: "/", message: describeError(error, "The preview failed.") },
      ],
    }
  );
}

function PreviewPanel({ result }: { result?: Preview }) {
  if (!result) return null;
  if (!result.valid)
    return (
      <Alert tone="warning">
        <p>The recipe has problems:</p>
        <ul className="m-0 pl-5">
          {result.problems.map((p, i) => (
            <li key={i}>
              <code>{p.path}</code> {p.message}
            </li>
          ))}
        </ul>
      </Alert>
    );
  return (
    <div>
      <p className="samuori-field-label mb-1">
        Dockerfile Samuori will build (with default values)
      </p>
      <pre className="samuori-log max-h-80">{result.dockerfile}</pre>
    </div>
  );
}

function RecipeEditor({
  initial,
  draft,
  onClose,
  onSaved,
}: {
  initial: string;
  draft?: Custom;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [yaml, setYaml] = useState(initial);
  const [result, setResult] = useState<Preview>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  async function save() {
    setBusy(true);
    setError(undefined);
    const { data, error } = draft
      ? await api.PUT("/custom-recipes/{recipeId}/versions/{version}", {
          params: { path: { recipeId: draft.id, version: draft.version } },
          body: { yaml },
        })
      : await api.POST("/custom-recipes", { body: { yaml } });
    setBusy(false);
    if (data) onSaved();
    else setError(describeError(error, "The recipe could not be saved."));
  }
  return (
    <Dialog
      wide
      title={
        draft ? `Edit ${draft.displayName} ${draft.version}` : "New recipe"
      }
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <div className="grid gap-4 lg:grid-cols-2">
        <AriaTextField
          value={yaml}
          onChange={setYaml}
          className="samuori-field"
        >
          <Label>Recipe (YAML)</Label>
          <TextArea className="samuori-code min-h-[28rem]" spellCheck={false} />
        </AriaTextField>
        <div className="min-w-0">
          {result ? (
            <PreviewPanel result={result} />
          ) : (
            <p className="samuori-muted mt-7">
              Press Preview to check the recipe and see the Dockerfile it
              produces. Steps: workdir, copySource, copyFromStage, run (argv or
              shell; network none or egress; secrets), env, user, expose, label,
              cmd. Use <code>{"${name}"}</code> for parameters in values and{" "}
              <code>$name</code> inside shell scripts.
            </p>
          )}
        </div>
      </div>
      {error && <Alert>{error}</Alert>}
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          Cancel
        </Button>
        <Button
          variant="secondary"
          onPress={() => void preview(yaml).then(setResult)}
        >
          Preview
        </Button>
        <Button isDisabled={busy} onPress={() => void save()}>
          Save draft
        </Button>
      </div>
    </Dialog>
  );
}

function DecideDialog({
  recipe,
  action,
  onClose,
  onDone,
}: {
  recipe: Custom;
  action: "approve" | "deprecate" | "withdraw";
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  const [result, setResult] = useState<Preview>();
  const load = useCallback(async () => {
    if (action === "approve") setResult(await preview(recipe.yaml));
  }, [action, recipe.yaml]);
  useLoader(load);
  const verb = {
    approve: "Approve",
    deprecate: "Deprecate",
    withdraw: "Withdraw",
  }[action];
  async function decide() {
    const { data, error } = await api.POST(
      "/custom-recipes/{recipeId}/versions/{version}/actions",
      {
        params: { path: { recipeId: recipe.id, version: recipe.version } },
        body: { action, reason },
      },
    );
    if (data) onDone();
    else setError(describeError(error));
  }
  return (
    <Dialog
      wide={action === "approve"}
      title={`${verb} ${recipe.displayName} ${recipe.version}`}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      {action === "approve" && (
        <>
          <p className="mt-0">
            Once approved, developers can build with this version and it can no
            longer change. Check the Dockerfile Samuori will build:
          </p>
          <PreviewPanel result={result} />
        </>
      )}
      {action === "deprecate" && (
        <p className="mt-0">
          Builds still work but warn that the version is deprecated.
        </p>
      )}
      {action === "withdraw" && (
        <p className="mt-0">New builds with this version are refused.</p>
      )}
      {error && <Alert>{error}</Alert>}
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
        <Button
          variant={action === "withdraw" ? "danger" : "primary"}
          isDisabled={
            reason.trim().length < 3 || (action === "approve" && !result?.valid)
          }
          onPress={() => void decide()}
        >
          {verb}
        </Button>
      </div>
    </Dialog>
  );
}

function DiscardDialog({
  recipe,
  onClose,
  onDone,
}: {
  recipe: Custom;
  onClose: () => void;
  onDone: () => void;
}) {
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  async function discard() {
    setBusy(true);
    const { error } = await api.DELETE(
      "/custom-recipes/{recipeId}/versions/{version}",
      { params: { path: { recipeId: recipe.id, version: recipe.version } } },
    );
    setBusy(false);
    if (error) setError(describeError(error));
    else onDone();
  }
  return (
    <Dialog
      title={`Discard draft ${recipe.displayName} ${recipe.version}?`}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <p className="mt-0">
        The draft is deleted and cannot be recovered. Approved, deprecated and
        withdrawn versions are not affected.
      </p>
      {error && <Alert>{error}</Alert>}
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          Cancel
        </Button>
        <Button
          variant="danger"
          isDisabled={busy}
          onPress={() => void discard()}
        >
          Discard draft
        </Button>
      </div>
    </Dialog>
  );
}
