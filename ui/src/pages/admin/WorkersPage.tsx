import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Checkbox, Select, TextField } from "../../components/Field";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";
import {
  issueWorkerToken,
  WorkerJoinNotice,
  type WorkerToken,
} from "./WorkerJoin";

type Pool = Schemas["WorkerPool"];
type Worker = Schemas["Worker"];

const reasonText: Record<string, string> = {
  unassigned: "not assigned to a pool",
  "profile-not-approved": "pool disabled or profile not allowed",
  "worker-self-test-stale": "its self-test has not passed recently",
  "worker-self-test-failed": "its self-test failed",
  "worker-config-changed": "its configuration changed since the last self-test",
  "egress-not-strict": "egress is not strict",
  cordoned: "cordoned",
  quarantined: "quarantined",
  revoked: "revoked or expired",
  "version-incompatible": "incompatible protocol version",
  "platform-unsupported": "does not build for the requested platform",
  "no-free-daemon-slot": "no free build slot (no isolation provider)",
};

const profiles = [
  {
    id: "rootless-dedicated-vm",
    label: "rootless-dedicated-vm (a VM dedicated to one trust domain)",
  },
  {
    id: "vm-per-job",
    label: "vm-per-job (a fresh VM for every build; not available yet)",
  },
  { id: "rootless-host", label: "rootless-host (trusted workloads only)" },
];

export function WorkersPage() {
  const [pools, setPools] = useState<Pool[]>();
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [locationCount, setLocationCount] = useState(1);
  const [creating, setCreating] = useState(false);
  const [approving, setApproving] = useState<Pool>();
  const [editing, setEditing] = useState<Pool>();
  const [acting, setActing] = useState<Worker>();
  const [assigning, setAssigning] = useState<Worker>();
  const [token, setToken] = useState<WorkerToken>();
  const [error, setError] = useState<string>();
  const load = useCallback(async () => {
    const [p, w, l] = await Promise.all([
      api.GET("/worker-pools"),
      api.GET("/workers", { params: { query: { limit: 200 } } }),
      api.GET("/locations"),
    ]);
    setPools(p.data?.items ?? []);
    setWorkers(w.data?.items ?? []);
    setLocationCount(l.data?.items.length ?? 1);
  }, []);
  useLoader(load);
  useEffect(() => {
    if (!workers.some((w) => w.selfTest?.running)) return;
    const t = setTimeout(() => void load(), 3000);
    return () => clearTimeout(t);
  }, [workers, load]);

  async function selfTest(pool: Pool) {
    setError(undefined);
    const { error } = await api.POST("/worker-pools/{poolId}/self-tests", {
      params: { path: { poolId: pool.id } },
    });
    if (error) setError(describeError(error));
    void load();
  }

  async function workerSelfTest(worker: Worker) {
    setError(undefined);
    const { error } = await api.POST("/workers/{workerId}/self-tests", {
      params: { path: { workerId: worker.id } },
    });
    if (error) setError(describeError(error));
    void load();
  }

  async function issue() {
    const r = await issueWorkerToken();
    if ("token" in r) setToken(r.token);
    else setError(r.error);
  }

  if (!pools) return <Loading />;
  const multiLocation = locationCount > 1;
  const unassigned = workers.filter(
    (w) => !w.poolId && w.enrollmentState === "enrolled" && !w.removal,
  );
  return (
    <>
      <PageHeader
        help="workers"
        title="Nodes & workers"
        description="Worker pools, their isolation profiles and enrolled workers. Enrollment never makes a worker trusted."
        actions={
          <>
            <Button variant="secondary" onPress={() => void issue()}>
              Enroll a worker
            </Button>
            <Button onPress={() => setCreating(true)}>New pool</Button>
          </>
        }
      />
      {error && <Alert>{error}</Alert>}
      {token && (
        <WorkerJoinNotice token={token} onDone={() => setToken(undefined)} />
      )}
      <section className="samuori-section" aria-labelledby="pools-h">
        <h2 id="pools-h">Pools</h2>
        <Table
          label="Worker pools"
          rows={pools}
          empty="No pools yet. Create one, then assign workers to it."
          columns={[
            {
              id: "name",
              name: "Pool",
              isRowHeader: true,
              render: (p) => p.name,
            },
            ...(multiLocation
              ? [
                  {
                    id: "location",
                    name: "Location",
                    render: (p: Pool) => p.location,
                  },
                ]
              : []),
            {
              id: "profile",
              name: "Isolation profile",
              render: (p) => p.isolationProfile,
            },
            {
              id: "approved",
              name: "Enabled",
              render: (p) =>
                p.profileApproved
                  ? `Yes${p.approvedBy ? `, approved by ${p.approvedBy}` : ""}`
                  : "No",
            },
            {
              id: "verified",
              name: "Verified workers",
              render: (p) =>
                `${p.verification.verifiedWorkers} of ${p.verification.workers}`,
            },
            {
              id: "actions",
              name: "Actions",
              render: (p) => (
                <div className="samuori-actions samuori-actions--start">
                  <Button
                    variant="secondary"
                    onPress={() => setEditing(p)}
                    aria-label={`Edit ${p.name}`}
                  >
                    Edit
                  </Button>
                  <Button
                    variant="secondary"
                    onPress={() => setApproving(p)}
                    aria-label={`${p.profileApproved ? "Disable" : "Enable"} ${p.name}`}
                  >
                    {p.profileApproved ? "Disable pool" : "Enable pool"}
                  </Button>
                  <Button
                    variant="secondary"
                    onPress={() => void selfTest(p)}
                    aria-label={`Run the self-test on every worker in ${p.name}`}
                  >
                    Test all workers
                  </Button>
                </div>
              ),
            },
          ]}
        />
        <p className="samuori-muted">
          Creating a pool approves its isolation profile; changing the profile
          or trust domain keeps it approved by you. A worker builds uploaded
          source only when its pool is enabled and the worker itself passed a
          self-test for that pool in the last 24 hours with its current
          configuration. Self-tests run the isolation, network and
          resource-limit checks in the worker&apos;s own build environments;
          they start when a worker is assigned to an enabled pool, when it
          connects, after its configuration changes and every 20 hours. Only the
          rootless-dedicated-vm provider can run them in this release;
          vm-per-job needs a provider validated on a KVM host.
        </p>
      </section>
      <section className="samuori-section" aria-labelledby="workers-h">
        <h2 id="workers-h">Workers</h2>
        {unassigned.length > 0 && (
          <Alert tone="info">
            {unassigned.length === 1
              ? "1 worker is"
              : `${unassigned.length} workers are`}{" "}
            waiting for a pool: {unassigned.map((w) => w.name).join(", ")}. An
            unassigned worker gets no builds, secrets or self-test. Use{" "}
            <strong>Assign to pool</strong>.
          </Alert>
        )}
        <Table
          label="Workers"
          rows={workers}
          empty="No workers enrolled."
          columns={[
            {
              id: "name",
              name: "Worker",
              isRowHeader: true,
              render: (w) => w.name,
            },
            {
              id: "pool",
              name: "Pool",
              render: (w) =>
                w.poolId
                  ? (pools.find((p) => p.id === w.poolId)?.name ?? w.poolId)
                  : "Unassigned",
            },
            {
              id: "state",
              name: "State",
              render: (w) =>
                w.removal
                  ? removalText(w.removal)
                  : `${w.enrollmentState}, ${w.operationalState}`,
            },
            {
              id: "seen",
              name: "Last seen",
              render: (w) =>
                w.lastSeenAt
                  ? new Date(w.lastSeenAt).toLocaleString()
                  : "never",
            },
            {
              id: "eligible",
              name: "Can build uploaded source",
              render: (w) =>
                w.eligibility.untrustedBuilds
                  ? "Yes"
                  : `No: ${w.eligibility.reasons.map((r) => reasonText[r] ?? r).join("; ")}`,
            },
            {
              id: "selftest",
              name: "Self-test",
              render: (w) =>
                w.selfTest.running
                  ? "running"
                  : `${w.selfTest.status}${w.selfTest.status !== "never-run" && !w.selfTest.current ? " (outdated)" : ""}${w.selfTest.failedChecks?.length ? `: ${w.selfTest.failedChecks.join(", ")}` : ""}`,
            },
            {
              id: "actions",
              name: "Actions",
              render: (w) =>
                w.enrollmentState === "revoked" || w.removal ? (
                  w.removal ? (
                    ""
                  ) : (
                    `Revoked: ${w.revokeReason ?? ""}`
                  )
                ) : (
                  <div className="samuori-actions samuori-actions--start">
                    <Button
                      variant={w.poolId ? "secondary" : undefined}
                      onPress={() => setAssigning(w)}
                      isDisabled={pools.length === 0}
                      aria-label={`${w.poolId ? "Move" : "Assign"} ${w.name} to a pool`}
                    >
                      {w.poolId ? "Move to pool" : "Assign to pool"}
                    </Button>
                    {w.poolId && (
                      <Button
                        variant="secondary"
                        onPress={() => void workerSelfTest(w)}
                        isDisabled={w.selfTest.running}
                        aria-label={`Run the self-test on ${w.name}`}
                      >
                        Run self-test
                      </Button>
                    )}
                    <Button
                      variant="secondary"
                      onPress={() => setActing(w)}
                      aria-label={`Change the state of ${w.name}`}
                    >
                      Manage
                    </Button>
                  </div>
                ),
            },
          ]}
        />
      </section>
      {creating && (
        <PoolDialog
          multiLocation={multiLocation}
          isOpen
          onOpenChange={setCreating}
          onSaved={() => {
            setCreating(false);
            void load();
          }}
        />
      )}
      {editing && (
        <PoolDialog
          pool={editing}
          multiLocation={multiLocation}
          isOpen
          onOpenChange={(o) => !o && setEditing(undefined)}
          onSaved={() => {
            setEditing(undefined);
            void load();
          }}
        />
      )}
      {assigning && (
        <AssignDialog
          worker={assigning}
          pools={pools}
          onClose={() => setAssigning(undefined)}
          onDone={() => {
            setAssigning(undefined);
            void load();
          }}
        />
      )}
      <WorkerActionDialog
        worker={acting}
        onClose={() => setActing(undefined)}
        onDone={() => {
          setActing(undefined);
          void load();
        }}
      />
      <ApprovalDialog
        pool={approving}
        onClose={() => setApproving(undefined)}
        onDone={() => {
          setApproving(undefined);
          void load();
        }}
      />
    </>
  );
}

// PoolDialog creates a pool, or edits one; saving approves its profile and trust domain (ADR 0045). The location
// field appears only once a second location exists, or when asked for.
function PoolDialog({
  pool,
  multiLocation,
  isOpen,
  onOpenChange,
  onSaved,
}: {
  pool?: Pool;
  multiLocation: boolean;
  isOpen: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved: () => void;
}) {
  const [form, setForm] = useState({
    name: pool?.name ?? "",
    trustDomain: pool?.trustDomain ?? "default",
  });
  const [location, setLocation] = useState(pool?.location ?? "");
  const [otherLocation, setOtherLocation] = useState(false);
  const [profile, setProfile] = useState<string>(
    pool?.isolationProfile ?? "rootless-dedicated-vm",
  );
  const [error, setError] = useState<string>();
  const set = (k: keyof typeof form) => (v: string) =>
    setForm((f) => ({ ...f, [k]: v }));
  const showLocation = multiLocation || otherLocation;
  async function submit(e: FormEvent) {
    e.preventDefault();
    const isolationProfile = profile as "vm-per-job";
    const where = showLocation && location ? { location } : {};
    const { error } = pool
      ? await api.PATCH("/worker-pools/{poolId}", {
          params: { path: { poolId: pool.id } },
          body: { ...form, ...where, isolationProfile },
        })
      : await api.POST("/worker-pools", {
          body: {
            ...form,
            ...where,
            isolationProfile,
            trustedWorkloadsOnly: false,
          },
        });
    if (error) setError(describeError(error));
    else onSaved();
  }
  const changesTrust =
    pool?.profileApproved &&
    (profile !== pool.isolationProfile ||
      form.trustDomain !== pool.trustDomain);
  return (
    <Dialog
      title={pool ? `Edit ${pool.name}` : "New pool"}
      isOpen={isOpen}
      onOpenChange={onOpenChange}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        {!pool && (
          <p>
            Creating the pool approves its isolation profile in your name. Its
            workers still build uploaded source only after each passes its own
            self-test.
          </p>
        )}
        {changesTrust && (
          <Alert tone="info">
            Saving approves the new isolation profile or trust domain in your
            name, and the change is recorded. Each worker runs its self-test
            again for a new profile.
          </Alert>
        )}
        <TextField
          label="Name"
          value={form.name}
          onChange={set("name")}
          isRequired
        />
        {showLocation ? (
          <TextField
            label="Location"
            value={location}
            onChange={setLocation}
            isRequired={otherLocation}
            description="Where the workers run, for example lab-1. A new name adds a location."
          />
        ) : (
          <Checkbox isSelected={otherLocation} onChange={setOtherLocation}>
            Put this pool in another location
          </Checkbox>
        )}
        <TextField
          label="Trust domain"
          value={form.trustDomain}
          onChange={set("trustDomain")}
          isRequired
        />
        <Select
          label="Isolation profile"
          options={profiles}
          value={profile}
          onChange={setProfile}
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit">{pool ? "Save" : "Create pool"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function ApprovalDialog({
  pool,
  onClose,
  onDone,
}: {
  pool?: Pool;
  onClose: () => void;
  onDone: () => void;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!pool) return;
    const { error } = await api.PUT("/worker-pools/{poolId}/approval", {
      params: { path: { poolId: pool.id } },
      body: { approved: !pool.profileApproved, reason },
    });
    if (error) setError(describeError(error));
    else {
      setReason("");
      onDone();
    }
  }
  return (
    <Dialog
      title={pool?.profileApproved ? "Disable pool" : "Enable pool"}
      isOpen={!!pool}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          {pool?.profileApproved
            ? "A disabled pool's workers build no uploaded source until it is enabled again."
            : "Enabling approves the pool's isolation profile in your name and starts each connected worker's self-test; a worker builds uploaded source only once its own self-test passes."}{" "}
          The decision is recorded permanently with your reason.
        </p>
        <TextField
          label="Reason"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={5}
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant={pool?.profileApproved ? "danger" : undefined}
          >
            {pool?.profileApproved ? "Disable pool" : "Enable pool"}
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

// AssignDialog puts a worker in a pool or moves it. Trust never moves with it: the worker's self-test runs again
// for the new pool.
export function AssignDialog({
  worker,
  pools,
  onClose,
  onDone,
}: {
  worker: Worker;
  pools: Pool[];
  onClose: () => void;
  onDone: () => void;
}) {
  const options = pools
    .filter((p) => p.id !== worker.poolId)
    .map((p) => ({
      id: p.id,
      label: `${p.name} (${p.isolationProfile}${p.profileApproved ? "" : ", disabled"})`,
    }));
  const [poolId, setPoolId] = useState<string | null>(
    options.length === 1 ? (options[0]?.id ?? null) : null,
  );
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!poolId) return;
    setError(undefined);
    const { error } = await api.PUT("/workers/{workerId}/pool", {
      params: { path: { workerId: worker.id } },
      body: { poolId, reason: reason || undefined },
    });
    if (error) setError(describeError(error));
    else onDone();
  }
  return (
    <Dialog
      title={`${worker.poolId ? "Move" : "Assign"} ${worker.name}`}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          {worker.poolId
            ? "Moving a worker ends its trust in the current pool. It builds uploaded source again only after its self-test passes for the new pool. A worker with builds in progress cannot move; drain it first."
            : "The worker takes the pool's isolation profile. Its self-test starts when the pool is enabled; it builds uploaded source only once that passes."}
        </p>
        <Select
          label="Pool"
          options={options}
          value={poolId}
          onChange={setPoolId}
          isRequired
        />
        <TextField
          label="Reason (optional)"
          value={reason}
          onChange={setReason}
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit" isDisabled={!poolId}>
            {worker.poolId ? "Move to pool" : "Assign to pool"}
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

const actions = [
  { id: "cordon", label: "Cordon: take no new builds" },
  { id: "uncordon", label: "Uncordon: take builds again" },
  { id: "drain", label: "Drain: finish running builds, then stop" },
  { id: "quarantine", label: "Quarantine: stop scheduling for investigation" },
  {
    id: "remove",
    label: "Remove: drain, wipe Samuori from the host, then revoke",
  },
  { id: "revoke", label: "Revoke: end this identity now" },
];

export function removalText(r: NonNullable<Worker["removal"]>): string {
  if (r.state === "draining") return "Removing: finishing running builds";
  if (r.state === "wiping") return "Removing: wiping the host";
  return r.outcome === "wiped"
    ? "Removed; the host wiped Samuori"
    : `Removed; not confirmed wiped (${r.detail ?? ""}). Run sudo samuori-uninstall on the host or terminate it`;
}

export function WorkerActionDialog({
  worker,
  initialAction,
  onClose,
  onDone,
}: {
  worker?: Worker;
  initialAction?: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const [action, setAction] = useState<string | null>(initialAction ?? null);
  const [reason, setReason] = useState("");
  const [compromised, setCompromised] = useState(false);
  const [days, setDays] = useState("7");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!worker || !action) return;
    setError(undefined);
    const since =
      action === "revoke" && compromised
        ? new Date(Date.now() - Number(days) * 86_400_000).toISOString()
        : undefined;
    const { error } = await api.POST("/workers/{workerId}/actions", {
      params: {
        path: { workerId: worker.id },
        header: { "Idempotency-Key": crypto.randomUUID() },
      },
      body: {
        action: action as Schemas["WorkerAction"]["action"],
        reason,
        compromisedSince: since,
      },
    });
    if (error) setError(describeError(error));
    else {
      setReason("");
      setAction(null);
      setCompromised(false);
      onDone();
    }
  }
  return (
    <Dialog
      title={`Manage ${worker?.name ?? "worker"}`}
      isOpen={!!worker}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <Select
          label="Action"
          options={actions}
          value={action}
          onChange={setAction}
          isRequired
        />
        {action === "revoke" && (
          <>
            <Alert tone="warning">
              Revocation is immediate and permanent. The worker is disconnected,
              its running builds are requeued on other workers, and it must be
              enrolled again with a new token.
            </Alert>
            <Checkbox isSelected={compromised} onChange={setCompromised}>
              The worker may be compromised: quarantine images it built
            </Checkbox>
            {compromised && (
              <TextField
                label="Quarantine images built in the last (days)"
                value={days}
                onChange={setDays}
                pattern="[0-9]{1,3}"
                isRequired
              />
            )}
          </>
        )}
        <TextField
          label="Reason"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={5}
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant={
              action === "revoke" || action === "remove" ? "danger" : undefined
            }
            isDisabled={!action}
          >
            Apply
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}
