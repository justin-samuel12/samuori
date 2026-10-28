import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Checkbox, Select, TextField } from "../../components/Field";
import { Alert, Badge, Loading } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type Status = Schemas["TopologyStatus"];
type Plan = Schemas["TopologyPlan"];
type Assessment = Schemas["TopologyAssessment"];

const tierText: Record<Assessment["tier"], string> = {
  standalone: "Standalone",
  "partial-ha": "Partial HA",
  ha: "Highly available",
};
const tierTone = { standalone: "bad", "partial-ha": "warn", ha: "ok" } as const;

// Resilience shows the tier measured from live servers next to the administrator's plan (ADR 0046).
export function Resilience({ admin }: { admin: boolean }) {
  const [status, setStatus] = useState<Status>();
  const [editing, setEditing] = useState(false);
  const load = useCallback(async () => {
    setStatus((await api.GET("/system/topology")).data);
  }, []);
  useLoader(load);
  if (!status) return <Loading />;
  const { live, plan, planned } = status;
  return (
    <section className="samuori-section" aria-labelledby="resilience-h">
      <div className="samuori-section-head">
        <h2 id="resilience-h">Resilience</h2>
        {admin && (
          <Button variant="secondary" onPress={() => setEditing(true)}>
            {plan ? "Change plan" : "Set a plan"}
          </Button>
        )}
      </div>
      <p>
        Now: <Badge tone={tierTone[live.tier]}>{tierText[live.tier]}</Badge>{" "}
        {plan && planned && (
          <>
            · Plan: {plan.controllers} controller
            {plan.controllers === 1 ? "" : "s"} ({status.liveControllers}{" "}
            running), {plan.workers} worker{plan.workers === 1 ? "" : "s"} (
            {status.liveWorkers} ready) ·{" "}
            <Badge tone={tierTone[planned.tier]}>
              {tierText[planned.tier]} when complete
            </Badge>
          </>
        )}
      </p>
      <p className="samuori-muted">
        Measured from running controllers, online self-tested workers, the
        database, storage and the portal URL. Highly available means every part
        below survives at least one failure.
      </p>
      {(live.warnings ?? []).map((w) => (
        <Alert key={w} tone="error">
          {w}
        </Alert>
      ))}
      <Components assessment={live} />
      {planned && (planned.warnings ?? []).length > 0 && (
        <Alert tone="error">{planned.warnings?.join(" ")}</Alert>
      )}
      {editing && (
        <PlanDialog
          plan={plan}
          onClose={() => setEditing(false)}
          onSaved={(s) => {
            setStatus(s);
            setEditing(false);
          }}
        />
      )}
    </section>
  );
}

function Components({ assessment }: { assessment: Assessment }) {
  return (
    <Table
      label="Resilience by component"
      rows={assessment.components.map((c) => ({ ...c, id: c.name }))}
      columns={[
        {
          id: "name",
          name: "Component",
          isRowHeader: true,
          render: (c) => c.name[0]?.toUpperCase() + c.name.slice(1),
        },
        { id: "have", name: "Now", render: (c) => c.have },
        {
          id: "tolerates",
          name: "Survives",
          render: (c) => (
            <Badge tone={c.tolerates > 0 ? "ok" : "bad"}>
              {c.tolerates} failure{c.tolerates === 1 ? "" : "s"}
              {c.declared ? " (declared)" : ""}
            </Badge>
          ),
        },
        {
          id: "gap",
          name: "For high availability",
          render: (c) => c.gap ?? "Nothing to add",
        },
      ]}
    />
  );
}

function PlanDialog({
  plan,
  onClose,
  onSaved,
}: {
  plan?: Plan;
  onClose: () => void;
  onSaved: (s: Status) => void;
}) {
  const [deployment, setDeployment] = useState<string>(
    plan?.deployment ?? "cluster",
  );
  const [controllers, setControllers] = useState(
    String(plan?.controllers ?? 3),
  );
  const [workers, setWorkers] = useState(String(plan?.workers ?? 2));
  const [dbReplicated, setDbReplicated] = useState(
    plan?.databaseReplicated ?? false,
  );
  const [storageReplicated, setStorageReplicated] = useState(
    plan?.storageReplicated ?? false,
  );
  const [error, setError] = useState<string>();
  const n = Number(controllers);
  const even = deployment === "cluster" && n > 1 && n % 2 === 0;
  async function submit(e: FormEvent) {
    e.preventDefault();
    const single = deployment === "single-server";
    const { data, error } = await api.PUT("/system/topology", {
      body: {
        deployment: deployment as Plan["deployment"],
        controllers: single ? 1 : n,
        workers: Number(workers),
        databaseReplicated: !single && dbReplicated,
        storageReplicated,
      },
    });
    if (error || !data) setError(describeError(error));
    else onSaved(data);
  }
  return (
    <Dialog
      title={plan ? "Change the plan" : "Set a plan"}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p className="samuori-muted">
          The plan is the target. It changes no server: add or remove servers
          below. Converting between a single server and a cluster is a
          maintenance-window step on the host.
        </p>
        <Select
          label="Deployment"
          options={[
            { id: "single-server", label: "Single server" },
            { id: "cluster", label: "Cluster" },
          ]}
          value={deployment}
          onChange={setDeployment}
        />
        {deployment === "cluster" && (
          <TextField
            label="Controllers"
            type="number"
            value={controllers}
            onChange={setControllers}
            isRequired
            description={
              even
                ? `${n} controllers survive no more failures than ${n - 1}: a majority must stay up. Use an odd number.`
                : "3 or 5 survive failures; 1 survives none."
            }
          />
        )}
        <TextField
          label={
            deployment === "cluster"
              ? "Build workers"
              : "Workers, this server's own included"
          }
          type="number"
          value={workers}
          onChange={setWorkers}
          isRequired
          description="2 or more in each pool survive a worker failure."
        />
        {deployment === "cluster" && (
          <Checkbox isSelected={dbReplicated} onChange={setDbReplicated}>
            The database is replicated across zones, such as RDS Multi-AZ
            (Samuori cannot check this)
          </Checkbox>
        )}
        <Checkbox
          isSelected={storageReplicated}
          onChange={setStorageReplicated}
        >
          The OCI repository used for storage is replicated (S3 always counts as
          replicated)
        </Checkbox>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">{even ? "Save anyway" : "Save"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}
