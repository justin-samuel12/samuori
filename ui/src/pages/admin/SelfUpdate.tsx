import { useCallback, useEffect, useState } from "react";
import { FileTrigger } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import { Dialog } from "../../components/Dialog";
import { Checkbox, Select } from "../../components/Field";
import { Alert, Badge, StateBadge } from "../../components/Feedback";
import { checkState, rolloutState, rolloutStep } from "../display";
import { Table } from "../../components/Table";
import { bytes } from "../../format";
import { useLoader } from "../../hooks";

type ReleaseList = Schemas["ReleaseList"];
type Release = Schemas["Release"];
type Change = NonNullable<Release["changes"]>[number];
type Rollout = Schemas["Rollout"];
type Step = Schemas["RolloutStep"];
type Preflight = Schemas["Preflight"];
type Check = Preflight["checks"][number];

const waveOptions = [
  ...["10", "25", "50"].map((id) => ({ id, label: `${id}% of each pool` })),
  {
    id: "100",
    label: "Everything at once, after one controller",
  },
];

/** A failed request while controllers restart for their own update is expected; polling continues. */
function unreachable(status: number | undefined) {
  return (
    status === undefined || status === 502 || status === 503 || status === 504
  );
}

/** Splits a release's history at the commit the running version was built from. */
function splitChanges(changes: Change[], current: string) {
  const commit = current.split("+")[1] ?? "";
  const at = commit ? changes.findIndex((c) => c.commit === commit) : -1;
  return at < 0
    ? { added: changes, earlier: [], known: false }
    : { added: changes.slice(0, at), earlier: changes.slice(at), known: true };
}

function ChangeItems({ changes }: { changes: Change[] }) {
  return (
    <ul className="samuori-changes">
      {changes.map((c) => (
        <li key={c.commit}>
          <code>{c.commit}</code>{" "}
          <span className="samuori-muted">{c.date}</span> {c.summary}
        </li>
      ))}
    </ul>
  );
}

function Changes({ release, current }: { release: Release; current: string }) {
  const changes = release.changes ?? [];
  if (changes.length === 0)
    return (
      <p className="samuori-muted">This bundle carries no change history.</p>
    );
  const { added, earlier, known } = splitChanges(changes, current);
  return (
    <>
      <h4>
        {known
          ? `New since ${current} (${added.length})`
          : `History (${added.length})`}
      </h4>
      {added.length > 0 ? (
        <ChangeItems changes={added} />
      ) : (
        <p className="samuori-muted">Nothing new since the running version.</p>
      )}
      {earlier.length > 0 && (
        <details className="samuori-details-toggle">
          <summary>Already running ({earlier.length})</summary>
          <ChangeItems changes={earlier} />
        </details>
      )}
    </>
  );
}

/** Signed update bundles, the checks before updating, and rollouts in waves (ADRs 0044, 0045). */
export function SelfUpdate({ current }: { current: string }) {
  const [releases, setReleases] = useState<ReleaseList>();
  const [rollouts, setRollouts] = useState<Rollout[]>();
  const [selected, setSelected] = useState<string>();
  const [checking, setChecking] = useState<string>();
  const [viewing, setViewing] = useState<Release>();
  const [purging, setPurging] = useState(false);
  const [restarting, setRestarting] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [file, setFile] = useState<File>();
  const [busy, setBusy] = useState(false);
  const [confirmDialog, ask] = useConfirm();

  const load = useCallback(async () => {
    try {
      const [r, l] = await Promise.all([
        api.GET("/system/releases"),
        api.GET("/system/rollouts", { params: { query: { limit: 20 } } }),
      ]);
      if (unreachable(r.response.status) || unreachable(l.response.status)) {
        setRestarting(true);
        return;
      }
      setRestarting(false);
      if (r.data) setReleases(r.data);
      if (l.data) setRollouts(l.data.items);
    } catch {
      setRestarting(true);
    }
  }, []);
  useLoader(load);
  const latest = rollouts?.[0];
  const running = latest?.state === "running";
  useEffect(() => {
    if (!running && !restarting) return;
    const t = window.setInterval(() => void load(), 3000);
    return () => window.clearInterval(t);
  }, [running, restarting, load]);
  if (!releases || !rollouts) {
    return restarting ? (
      <Alert tone="info">The controller is restarting; reconnecting…</Alert>
    ) : null;
  }
  const shown = rollouts.find((r) => r.id === selected) ?? latest;

  async function upload() {
    if (!file) return;
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    const form = new FormData();
    form.append("bundle", file, file.name);
    const { data, error } = await api
      .POST("/system/releases", {
        body: form as never,
        bodySerializer: (b) => b as unknown as FormData,
      })
      .catch(() => ({
        data: undefined,
        error: { message: "the controller did not answer; try again" },
      }));
    setBusy(false);
    if (data) {
      setNotice(
        `Release ${data.version} (${data.architectures.join(", ")}) verified and kept.`,
      );
      setFile(undefined);
      setChecking(data.version);
      void load();
    } else setError(describeError(error));
  }

  async function remove(r: Release) {
    setError(undefined);
    const { error } = await api.DELETE("/system/releases/{version}", {
      params: { path: { version: r.version } },
    });
    void load();
    return error ? describeError(error) : undefined;
  }

  async function purge() {
    setError(undefined);
    const { data, error } = await api.DELETE("/system/rollouts");
    setPurging(false);
    setSelected(undefined);
    if (error || !data) setError(describeError(error));
    else
      setNotice(
        `Cleared ${data.removed} finished rollout${data.removed === 1 ? "" : "s"}.`,
      );
    void load();
  }

  async function control(action: "pause" | "resume" | "cancel") {
    if (!latest) return;
    setError(undefined);
    const path = `/system/rollouts/{rolloutId}/${action}` as const;
    const { error } = await api.POST(path, {
      params: { path: { rolloutId: latest.id } },
    });
    if (error) setError(describeError(error));
    void load();
  }

  return (
    <>
      <h2>Self-update</h2>
      <p>
        Upload the signed bundle from <code>make update</code>. The checks run
        first; then <strong>Update</strong> moves controllers one at a time, the
        leader last, and then each pool's workers in waves, every worker
        drained, updated and self-tested. Every host verifies the signature
        itself, and a host that fails goes back to its previous release and
        pauses the rollout.
      </p>
      {restarting && (
        <Alert tone="info">
          A controller is restarting, probably for its own update; this page
          keeps trying.
        </Alert>
      )}
      {!releases.signersConfigured && (
        <Alert tone="info">
          This controller has no release signers, so it accepts no release.
          Reinstall it with --release-signers (see the upgrade guide).
        </Alert>
      )}
      {error && <Alert>{error}</Alert>}
      {notice && <Alert tone="success">{notice}</Alert>}
      {releases.signersConfigured && (
        <div className="samuori-actions samuori-actions--start">
          <FileTrigger
            acceptedFileTypes={[".samuoriupdate"]}
            onSelect={(l) => l && setFile(l[0])}
          >
            <Button variant="secondary" isDisabled={busy}>
              Choose update bundle
            </Button>
          </FileTrigger>
          <span className="samuori-muted">
            {file
              ? `${file.name} (${bytes(file.size)})`
              : "Select samuori-<version>.samuoriupdate."}
          </span>
          <Button isDisabled={busy || !file} onPress={() => void upload()}>
            {busy ? "Verifying…" : "Upload"}
          </Button>
        </div>
      )}
      <Table<Release & { id: string }>
        label="Releases"
        rows={releases.items.map((r) => ({ ...r, id: r.version }))}
        empty="No release uploaded yet. Choose the update bundle above, then Upload."
        columns={[
          {
            id: "version",
            name: "Version",
            isRowHeader: true,
            render: (r) => r.version,
          },
          {
            id: "arch",
            name: "Architectures",
            render: (r) => r.architectures.join(", "),
          },
          {
            id: "size",
            name: "Size",
            align: "end",
            render: (r) => bytes(r.size),
          },
          {
            id: "uploaded",
            name: "Uploaded",
            render: (r) => new Date(r.uploadedAt).toLocaleString(),
          },
          {
            id: "actions",
            name: "Actions",
            render: (r) => (
              <div className="samuori-actions samuori-actions--start">
                <Button
                  variant="secondary"
                  onPress={() => setViewing(r)}
                  aria-label={`Changes in ${r.version}`}
                >
                  Changes
                </Button>
                <Button
                  isDisabled={running || !r.bundled}
                  onPress={() => setChecking(r.version)}
                  aria-label={`Check and update to ${r.version}`}
                >
                  {r.bundled ? "Check and update" : "Not a bundle"}
                </Button>
                <Button
                  variant="quiet-danger"
                  isDisabled={running}
                  onPress={() =>
                    ask({
                      title: `Delete release ${r.version}?`,
                      message:
                        "Every architecture of the release is removed from this installation; upload it again to update to it.",
                      confirmLabel: "Delete release",
                      run: () => remove(r),
                    })
                  }
                  aria-label={`Delete release ${r.version}`}
                >
                  Delete
                </Button>
              </div>
            ),
          },
        ]}
      />
      {viewing && (
        <Dialog
          title={`Changes in ${viewing.version}`}
          size="wide"
          isOpen
          onOpenChange={(o) => !o && setViewing(undefined)}
        >
          <Changes release={viewing} current={current} />
          <div className="samuori-actions">
            <Button onPress={() => setViewing(undefined)}>Close</Button>
          </div>
        </Dialog>
      )}
      {checking && !running && (
        <UpdatePanel
          version={checking}
          release={releases.items.find((r) => r.version === checking)}
          current={current}
          onClose={() => setChecking(undefined)}
          onStarted={(r) => {
            setChecking(undefined);
            setSelected(r.id);
            void load();
          }}
        />
      )}
      {shown && (
        <RolloutView
          rollout={shown}
          latest={shown.id === latest?.id}
          onControl={(a) => void control(a)}
        />
      )}
      <div className="samuori-section-head">
        <h3 className="m-0">History</h3>
        <Button
          variant="secondary"
          isDisabled={!rollouts.some((r) => r.state !== "running")}
          onPress={() => setPurging(true)}
        >
          Clear history
        </Button>
      </div>
      {purging && (
        <Dialog
          title="Clear the update history?"
          size="confirm"
          isOpen
          onOpenChange={(o) => !o && setPurging(false)}
        >
          <p>
            Every finished rollout and its steps are deleted. A rollout that is
            running stays. The audit log keeps who started each update.
          </p>
          <div className="samuori-actions">
            <Button variant="secondary" onPress={() => setPurging(false)}>
              Cancel
            </Button>
            <Button variant="danger" onPress={() => void purge()}>
              Clear history
            </Button>
          </div>
        </Dialog>
      )}
      <Table<Rollout>
        label="Rollout history"
        rows={rollouts}
        empty="No rollout yet."
        columns={[
          {
            id: "started",
            name: "Started",
            isRowHeader: true,
            render: (r) => new Date(r.createdAt).toLocaleString(),
          },
          { id: "version", name: "Version", render: (r) => r.version },
          {
            id: "state",
            name: "State",
            render: (r) => <StateBadge state={rolloutState(r.state)} />,
          },
          {
            id: "hosts",
            name: "Hosts updated",
            render: (r) => `${doneCount(r.steps)} of ${r.steps.length}`,
          },
          { id: "by", name: "By", render: (r) => r.createdBy },
          {
            id: "view",
            name: "Actions",
            render: (r) => (
              <Button
                variant="secondary"
                onPress={() => setSelected(r.id)}
                aria-label={`Show the rollout of ${r.version} started ${new Date(r.createdAt).toLocaleString()}`}
              >
                Show
              </Button>
            ),
          },
        ]}
      />
      {confirmDialog}
    </>
  );
}

function doneCount(steps: Step[]) {
  return steps.filter((s) => s.state === "succeeded" || s.state === "skipped")
    .length;
}

function StepsTable({ steps, label }: { steps: Step[]; label: string }) {
  return (
    <Table<Step & { id: string }>
      label={label}
      rows={steps.map((s) => ({ ...s, id: String(s.seq) }))}
      empty="Nothing to update."
      columns={[
        { id: "wave", name: "Wave", render: (s) => s.wave },
        { id: "kind", name: "Host", render: (s) => s.kind },
        {
          id: "name",
          name: "Name",
          isRowHeader: true,
          render: (s) => s.name,
        },
        {
          id: "pool",
          name: "Pool",
          render: (s) =>
            s.pool ? `${s.pool}${s.location ? ` (${s.location})` : ""}` : "",
        },
        {
          id: "state",
          name: "State",
          render: (s) => <StateBadge state={rolloutStep(s.state)} />,
        },
        { id: "detail", name: "Detail", render: (s) => s.detail ?? "" },
      ]}
    />
  );
}

function RolloutView({
  rollout,
  latest,
  onControl,
}: {
  rollout: Rollout;
  latest: boolean;
  onControl: (a: "pause" | "resume" | "cancel") => void;
}) {
  const running = rollout.state === "running";
  const failed = rollout.steps.some((s) => s.state === "failed");
  return (
    <>
      <h3>
        Rollout of {rollout.version}{" "}
        <Badge
          tone={rollout.paused ? "warn" : rolloutState(rollout.state).tone}
          live={running && !rollout.paused}
        >
          {rollout.paused ? "Paused" : rolloutState(rollout.state).text}
        </Badge>
      </h3>
      <p className="samuori-muted">
        Started {new Date(rollout.createdAt).toLocaleString()} by{" "}
        {rollout.createdBy}
        {rollout.reason ? `: ${rollout.reason}` : ""}.{" "}
        {doneCount(rollout.steps)} of {rollout.steps.length} hosts updated
        {rollout.wavePercent === 100
          ? "; one controller first, which then leads, then everything else at once"
          : rollout.wavePercent
            ? `; workers in waves of ${rollout.wavePercent}% of each pool (at least one), controllers one at a time`
            : ""}
        .{rollout.detail ? ` ${rollout.detail}` : ""}
      </p>
      {rollout.paused && rollout.pauseReason && (
        <Alert tone={failed ? "warning" : "info"}>
          {rollout.pauseReason}
          {failed
            ? " Resume retries the failed hosts; Stop ends the rollout."
            : ""}
        </Alert>
      )}
      {running && latest && (
        <div className="samuori-actions samuori-actions--start">
          {rollout.paused ? (
            <Button onPress={() => onControl("resume")}>Resume</Button>
          ) : (
            <Button variant="secondary" onPress={() => onControl("pause")}>
              Pause
            </Button>
          )}
          <Button variant="danger" onPress={() => onControl("cancel")}>
            Stop
          </Button>
        </div>
      )}
      <StepsTable steps={rollout.steps} label="Rollout steps" />
    </>
  );
}

function UpdatePanel({
  version,
  release,
  current,
  onClose,
  onStarted,
}: {
  version: string;
  release?: Release;
  current: string;
  onClose: () => void;
  onStarted: (r: Rollout) => void;
}) {
  const [preflight, setPreflight] = useState<Preflight>();
  const [pause, setPause] = useState(true);
  const [downgrade, setDowngrade] = useState(false);
  const [wave, setWave] = useState("25");
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState<string>();
  const check = useCallback(async () => {
    setPreflight(undefined);
    setError(undefined);
    const { data, error } = await api.GET(
      "/system/releases/{version}/preflight",
      {
        params: {
          path: { version },
          query: { allowDowngrade: downgrade, wavePercent: Number(wave) },
        },
      },
    );
    if (data) setPreflight(data);
    else setError(describeError(error));
  }, [version, downgrade, wave]);
  useLoader(check);

  async function start() {
    setStarting(true);
    setError(undefined);
    const { data, error } = await api
      .POST("/system/rollouts", {
        body: {
          version,
          pauseAfterFirstWave: pause,
          wavePercent: Number(wave),
          allowDowngrade: downgrade,
        },
      })
      .catch(() => ({
        data: undefined,
        error: {
          message:
            "the controller did not answer; check the history before trying again",
        },
      }));
    if (data) onStarted(data);
    else {
      setStarting(false);
      setError(describeError(error));
    }
  }

  return (
    <section aria-label={`Update to ${version}`}>
      <h3>Update to {version}</h3>
      {error && <Alert>{error}</Alert>}
      {release && <Changes release={release} current={current} />}
      {!preflight ? (
        !error && <p className="samuori-muted">Running the checks…</p>
      ) : (
        <>
          <Table<Check>
            label="Checks before updating"
            rows={preflight.checks}
            empty="No checks."
            columns={[
              {
                id: "name",
                name: "Check",
                isRowHeader: true,
                render: (c) => c.name,
              },
              {
                id: "state",
                name: "Result",
                render: (c) => <StateBadge state={checkState(c.state)} />,
              },
              { id: "detail", name: "Detail", render: (c) => c.detail },
            ]}
          />
          <h4>Plan</h4>
          <StepsTable steps={preflight.steps} label="Planned steps" />
        </>
      )}
      <Select
        label="Wave size"
        options={waveOptions}
        value={wave}
        onChange={setWave}
        description={
          wave === "100"
            ? "One secondary controller, picked at random, updates first and takes over as leader; then every other controller and every worker update together. Builds stop until the workers are back."
            : "The share of each pool's workers updated at once, rounded up to at least one: 25% of 2 workers is one at a time. Pools update in parallel; controllers go one at a time, the leader last."
        }
      />
      <Checkbox isSelected={pause} onChange={setPause}>
        {wave === "100"
          ? "Pause after the first controller so I can check it"
          : "Pause after the first wave of workers so I can check it"}
      </Checkbox>
      <Checkbox isSelected={downgrade} onChange={setDowngrade}>
        Allow a downgrade
      </Checkbox>
      <div className="samuori-actions samuori-actions--start">
        <Button variant="secondary" onPress={onClose}>
          Close
        </Button>
        <Button variant="secondary" onPress={() => void check()}>
          Run the checks again
        </Button>
        <Button
          isDisabled={starting || !preflight?.ready}
          onPress={() => void start()}
        >
          {starting ? "Starting…" : "Update"}
        </Button>
      </div>
    </section>
  );
}
