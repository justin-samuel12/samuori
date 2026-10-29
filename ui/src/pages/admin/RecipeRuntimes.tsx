import { useCallback, useState } from "react";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { TextField } from "../../components/Field";
import { Alert, Badge, Loading } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";
import { useCan } from "../../session/session";

type OrgRuntime = Schemas["OrganizationRuntime"];
type Runtimes = Schemas["RecipeRuntimes"];
type ShippedRuntime = Schemas["RecipeRuntime"];
type ShippedStatus = ShippedRuntime["status"];

const tone = {
  draft: "neutral",
  approved: "ok",
  deprecated: "warn",
  withdrawn: "bad",
} as const;

const shippedActions: {
  status: ShippedStatus;
  label: string;
  shownWhen: (s: ShippedStatus) => boolean;
}[] = [
  {
    status: "deprecated",
    label: "Deprecate",
    shownWhen: (s) => s === "approved",
  },
  {
    status: "withdrawn",
    label: "Withdraw",
    shownWhen: (s) => s !== "withdrawn",
  },
  {
    status: "approved",
    label: "Approve again",
    shownWhen: (s) => s !== "approved",
  },
];

/** Runtimes of one built-in recipe version: shipped ones are read-only; administrators add their own (ADR 0051). */
export function RecipeRuntimes({
  recipe,
  onClose,
  onChange,
}: {
  recipe: { id: string; version: string; displayName: string };
  onClose: () => void;
  onChange: () => void;
}) {
  const [data, setData] = useState<Runtimes | null>();
  const [adding, setAdding] = useState<{ from?: OrgRuntime }>();
  const [deciding, setDeciding] = useState<{
    runtime: OrgRuntime;
    action: "approve" | "withdraw";
  }>();
  const [settingShipped, setSettingShipped] = useState<{
    runtime: ShippedRuntime;
    status: ShippedStatus;
  }>();
  const canSetShipped = useCan()("setShippedRuntimeStatus");
  const path = { recipeId: recipe.id, version: recipe.version };
  const load = useCallback(async () => {
    const { data } = await api.GET(
      "/recipes/{recipeId}/versions/{version}/runtimes",
      { params: { path: { recipeId: recipe.id, version: recipe.version } } },
    );
    setData(data ?? null);
  }, [recipe.id, recipe.version]);
  useLoader(load);
  const done = () => {
    setAdding(undefined);
    setDeciding(undefined);
    setSettingShipped(undefined);
    void load();
    onChange();
  };
  return (
    <>
      <Dialog
        title={`Runtimes of ${recipe.displayName} ${recipe.version}`}
        size="wide"
        isOpen
        onOpenChange={(o) => !o && onClose()}
      >
        {data === undefined ? (
          <Loading />
        ) : data === null ? (
          <Alert>The runtimes could not be loaded.</Alert>
        ) : (
          <>
            <h3 className="mt-0">Shipped with Samuori</h3>
            <p className="samuori-muted">
              A Samuori release changes these. You can deprecate one (builds
              warn) or withdraw it (new builds are refused and developers no
              longer see it); your decisions and your own runtimes below are
              kept across updates.
            </p>
            <Table
              label="Shipped runtimes"
              rows={data.shipped.map((r) => ({ ...r, id: r.version }))}
              columns={[
                {
                  id: "version",
                  name: "Runtime",
                  isRowHeader: true,
                  render: (r) => r.version,
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
                {
                  id: "status",
                  name: "Status",
                  render: (r) => (
                    <>
                      <Badge tone={tone[r.status]}>{r.status}</Badge>
                      {r.statusReason && (
                        <div className="samuori-muted text-xs">
                          {r.statusReason}
                        </div>
                      )}
                    </>
                  ),
                },
                ...(canSetShipped
                  ? [
                      {
                        id: "actions",
                        name: "Actions",
                        render: (r: ShippedRuntime) => (
                          <span className="inline-flex flex-wrap gap-1">
                            {shippedActions
                              .filter((a) => a.shownWhen(r.status))
                              .map((a) => (
                                <Button
                                  key={a.status}
                                  variant="quiet"
                                  aria-label={`${a.label} runtime ${r.version}`}
                                  onPress={() =>
                                    setSettingShipped({
                                      runtime: r,
                                      status: a.status,
                                    })
                                  }
                                >
                                  {a.label}
                                </Button>
                              ))}
                          </span>
                        ),
                      },
                    ]
                  : []),
              ]}
              dependencies={[data, canSetShipped]}
            />
            <div className="samuori-section-head">
              <div>
                <h3>Added by your organization</h3>
                <p className="samuori-muted m-0">
                  Offered in New image once approved. Withdrawn runtimes stay
                  here as history and are refused for new builds. Approved
                  runtimes can&apos;t change; duplicate one to create a new
                  draft.
                </p>
              </div>
              <Button variant="secondary" onPress={() => setAdding({})}>
                Add runtime
              </Button>
            </div>
            <Table
              label="Organization runtimes"
              rows={data.organization}
              empty="No runtimes added."
              columns={[
                {
                  id: "label",
                  name: "Runtime",
                  isRowHeader: true,
                  render: (r) => (
                    <>
                      {r.label}
                      {r.note && (
                        <div className="samuori-muted text-xs">{r.note}</div>
                      )}
                    </>
                  ),
                },
                {
                  id: "images",
                  name: "Images",
                  render: (r) => (
                    <ul className="samuori-plain-list">
                      <li>
                        build <code>{r.buildImage}</code>
                      </li>
                      <li>
                        run <code>{r.runImage}</code>
                      </li>
                    </ul>
                  ),
                },
                {
                  id: "status",
                  name: "Status",
                  render: (r) => (
                    <>
                      <Badge tone={tone[r.status]}>{r.status}</Badge>
                      {r.superseded && (
                        <div className="samuori-muted text-xs">
                          A Samuori release now ships {r.label}; builds use that
                          one.
                        </div>
                      )}
                      {r.reason && (
                        <div className="samuori-muted text-xs">{r.reason}</div>
                      )}
                    </>
                  ),
                },
                {
                  id: "actions",
                  name: "Actions",
                  render: (r) => (
                    <span className="inline-flex flex-wrap gap-1">
                      {r.status === "draft" && (
                        <Button
                          variant="quiet"
                          aria-label={`Approve runtime ${r.label}`}
                          onPress={() =>
                            setDeciding({ runtime: r, action: "approve" })
                          }
                        >
                          Approve
                        </Button>
                      )}
                      {r.status !== "withdrawn" && (
                        <Button
                          variant="quiet-danger"
                          aria-label={`Withdraw runtime ${r.label}`}
                          onPress={() =>
                            setDeciding({ runtime: r, action: "withdraw" })
                          }
                        >
                          Withdraw
                        </Button>
                      )}
                      {r.status !== "draft" && (
                        <Button
                          variant="quiet"
                          aria-label={`Duplicate runtime ${r.label} as a new draft`}
                          onPress={() => setAdding({ from: r })}
                        >
                          Duplicate
                        </Button>
                      )}
                    </span>
                  ),
                },
              ]}
              dependencies={[data]}
            />
          </>
        )}
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Close
          </Button>
        </div>
      </Dialog>
      {adding && (
        <AddRuntime
          path={path}
          from={adding.from}
          onClose={() => setAdding(undefined)}
          onDone={done}
        />
      )}
      {deciding && (
        <DecideRuntime
          path={path}
          runtime={deciding.runtime}
          action={deciding.action}
          onClose={() => setDeciding(undefined)}
          onDone={done}
        />
      )}
      {settingShipped && (
        <SetShippedStatus
          path={path}
          runtime={settingShipped.runtime}
          status={settingShipped.status}
          onClose={() => setSettingShipped(undefined)}
          onDone={done}
        />
      )}
    </>
  );
}

const shippedCopy: Record<ShippedStatus, { verb: string; effect: string }> = {
  deprecated: {
    verb: "Deprecate",
    effect:
      "Builds that use it still run, with a warning that shows your reason. Developers see it marked as deprecated.",
  },
  withdrawn: {
    verb: "Withdraw",
    effect:
      "Developers no longer see it. New builds that ask for exactly this version are refused with your reason; a range such as >=22 picks the newest runtime still available. Builds already queued are not affected.",
  },
  approved: {
    verb: "Approve again",
    effect:
      "Developers can choose it again and builds use it without a warning.",
  },
};

function SetShippedStatus({
  path,
  runtime,
  status,
  onClose,
  onDone,
}: {
  path: { recipeId: string; version: string };
  runtime: ShippedRuntime;
  status: ShippedStatus;
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const { verb, effect } = shippedCopy[status];
  async function save() {
    setBusy(true);
    setError(undefined);
    const { response, error } = await api.PUT(
      "/recipes/{recipeId}/versions/{version}/shipped-runtimes/{label}/status",
      {
        params: { path: { ...path, label: runtime.version } },
        body: { status, reason },
      },
    );
    setBusy(false);
    if (response.ok) onDone();
    else setError(describeError(error));
  }
  return (
    <Dialog
      title={`${verb} runtime ${runtime.version}`}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <p className="mt-0">{effect}</p>
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
          variant={status === "withdrawn" ? "danger" : "primary"}
          isDisabled={busy || reason.trim().length < 3}
          onPress={() => void save()}
        >
          {verb}
        </Button>
      </div>
    </Dialog>
  );
}

function AddRuntime({
  path,
  from,
  onClose,
  onDone,
}: {
  path: { recipeId: string; version: string };
  from?: OrgRuntime;
  onClose: () => void;
  onDone: () => void;
}) {
  const [label, setLabel] = useState(from?.label ?? "");
  const [buildImage, setBuildImage] = useState(from?.buildImage ?? "");
  const [runImage, setRunImage] = useState(
    from && from.runImage !== from.buildImage ? from.runImage : "",
  );
  const [note, setNote] = useState(from?.note ?? "");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  async function save() {
    setBusy(true);
    setError(undefined);
    const { data, error } = await api.POST(
      "/recipes/{recipeId}/versions/{version}/runtimes",
      {
        params: { path },
        body: {
          label,
          buildImage,
          runImage: runImage || buildImage,
          ...(note ? { note } : {}),
        },
      },
    );
    setBusy(false);
    if (data) onDone();
    else setError(describeError(error, "The runtime could not be added."));
  }
  return (
    <Dialog
      title={from ? `Duplicate runtime ${from.label}` : "Add runtime"}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <p className="mt-0">
        {from && (
          <>
            This creates a new draft; {from.label} is unchanged.
            {from.status === "approved" &&
              " Give it another runtime version, or withdraw the original first: only one runtime per version can be active."}{" "}
          </>
        )}
        Saved as a draft. Both images must be pinned by digest and allowed by
        the base-image policy. Find a digest with{" "}
        <code>docker buildx imagetools inspect node:26-bookworm-slim</code>.
      </p>
      <TextField
        label="Runtime version"
        description="As developers ask for it, for example 26 or 3.14"
        value={label}
        onChange={setLabel}
        isRequired
      />
      <TextField
        label="Build image"
        placeholder="docker.io/library/node:26-bookworm-slim@sha256:…"
        value={buildImage}
        onChange={setBuildImage}
        isRequired
      />
      <TextField
        label="Run image"
        description="Leave empty to use the build image"
        value={runImage}
        onChange={setRunImage}
      />
      <TextField label="Note" value={note} onChange={setNote} />
      {error && <Alert>{error}</Alert>}
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          Cancel
        </Button>
        <Button
          isDisabled={busy || !label.trim() || !buildImage.trim()}
          onPress={() => void save()}
        >
          Save draft
        </Button>
      </div>
    </Dialog>
  );
}

function DecideRuntime({
  path,
  runtime,
  action,
  onClose,
  onDone,
}: {
  path: { recipeId: string; version: string };
  runtime: OrgRuntime;
  action: "approve" | "withdraw";
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  const verb = action === "approve" ? "Approve" : "Withdraw";
  async function decide() {
    const { data, error } = await api.POST(
      "/recipes/{recipeId}/versions/{version}/runtimes/{runtimeId}/actions",
      {
        params: { path: { ...path, runtimeId: runtime.id } },
        body: { action, reason },
      },
    );
    if (data) onDone();
    else setError(describeError(error));
  }
  return (
    <Dialog
      title={`${verb} runtime ${runtime.label}`}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <p className="mt-0">
        {action === "approve"
          ? "Developers can then choose this runtime in New image. The images are checked against the base-image policy again."
          : "New builds that ask for this runtime are refused. It stays listed as history."}
      </p>
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
          isDisabled={reason.trim().length < 3}
          onPress={() => void decide()}
        >
          {verb}
        </Button>
      </div>
    </Dialog>
  );
}
