import { useCallback, useEffect, useState } from "react";
import { Link } from "react-aria-components";
import { api, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";
import { ControllerList, JoinControllers } from "./Controllers";
import { Resilience } from "./Topology";
import {
  issueWorkerToken,
  WorkerJoinNotice,
  type WorkerToken,
} from "./WorkerJoin";
import { AssignDialog, removalText, WorkerActionDialog } from "./WorkersPage";
import { WorkerStatus } from "./WorkerStatus";

type Pool = Schemas["WorkerPool"];
type Worker = Schemas["Worker"];

// ServersPage is the one place to add, accept and remove controllers and workers (ADR 0045). Pools, self-tests and
// the other worker actions stay under Workers & pools.
export function ServersPage() {
  const [status, setStatus] = useState<Schemas["UpgradeStatus"]>();
  const [pools, setPools] = useState<Pool[]>([]);
  const [workers, setWorkers] = useState<Worker[]>();
  const [token, setToken] = useState<WorkerToken>();
  const [assigning, setAssigning] = useState<Worker>();
  const [removing, setRemoving] = useState<Worker>();
  const [error, setError] = useState<string>();
  const load = useCallback(async () => {
    const [s, p, w] = await Promise.all([
      api.GET("/system/upgrades"),
      api.GET("/worker-pools"),
      api.GET("/workers", { params: { query: { limit: 200 } } }),
    ]);
    setStatus(s.data);
    setPools(p.data?.items ?? []);
    setWorkers(w.data?.items ?? []);
  }, []);
  useLoader(load);
  const busy =
    (status?.removals?.some((r) => r.state !== "removed") ?? false) ||
    (workers?.some((w) => w.removal && w.removal.state !== "removed") ?? false);
  // Hosts join and leave from the command line, so the page keeps polling: faster while a removal runs.
  useEffect(() => {
    const t = window.setInterval(() => void load(), busy ? 5000 : 15000);
    return () => window.clearInterval(t);
  }, [busy, load]);

  async function addWorker() {
    setError(undefined);
    const r = await issueWorkerToken();
    if ("token" in r) setToken(r.token);
    else setError(r.error);
  }

  if (!status || !workers) return <Loading />;
  const active = workers.filter(
    (w) => w.enrollmentState !== "revoked" || w.removal,
  );
  return (
    <>
      <PageHeader
        help="servers"
        title="Servers"
        description="The controllers and workers of this installation. Join codes are single use, expire within the hour and grant nothing until you accept the controller or assign the worker to a pool."
      />
      <Resilience admin />
      <section className="samuori-section" aria-labelledby="controllers-h">
        <h2 id="controllers-h">Controllers</h2>
        <ControllerList status={status} admin onChanged={load} />
      </section>
      <JoinControllers />
      <section className="samuori-section" aria-labelledby="servers-workers-h">
        <h2 id="servers-workers-h">Workers</h2>
        <WorkerStatus workers={workers} link={false} />
        {error && <Alert>{error}</Alert>}
        {token ? (
          <WorkerJoinNotice
            token={token}
            onDone={() => {
              setToken(undefined);
              void load();
            }}
          />
        ) : (
          <div className="samuori-actions justify-start">
            <Button onPress={() => void addWorker()}>Add a worker</Button>
          </div>
        )}
        <Table
          label="Worker servers"
          rows={active}
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
            { id: "version", name: "Release", render: (w) => w.version },
            {
              id: "seen",
              name: "Last seen",
              render: (w) =>
                w.lastSeenAt
                  ? new Date(w.lastSeenAt).toLocaleString()
                  : "never",
            },
            {
              id: "actions",
              name: "Actions",
              render: (w) =>
                w.removal ? (
                  ""
                ) : (
                  <div className="samuori-actions samuori-actions--start">
                    {!w.poolId && (
                      <Button
                        onPress={() => setAssigning(w)}
                        isDisabled={pools.length === 0}
                        aria-label={`Assign ${w.name} to a pool`}
                      >
                        Assign to pool
                      </Button>
                    )}
                    <Button
                      variant="quiet-danger"
                      onPress={() => setRemoving(w)}
                      aria-label={`Remove ${w.name}`}
                    >
                      Remove
                    </Button>
                  </div>
                ),
            },
          ]}
        />
        <p className="samuori-muted">
          Pools, moving workers, self-tests, cordon, drain and revoke are under{" "}
          <Link href="/admin/workers" className="samuori-link">
            Workers &amp; pools
          </Link>
          .
        </p>
      </section>
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
      {removing && (
        <WorkerActionDialog
          worker={removing}
          initialAction="remove"
          onClose={() => setRemoving(undefined)}
          onDone={() => {
            setRemoving(undefined);
            void load();
          }}
        />
      )}
    </>
  );
}
