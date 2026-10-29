import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Select, TextField } from "../../components/Field";
import {
  Alert,
  Explain,
  Loading,
  PageHeader,
  Section,
} from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type Identity = Schemas["FederationIdentity"];
type Peer = Schemas["FederationPeer"];
type Grant = Schemas["FederationGrant"];
type Route = Schemas["FederationRoute"];
type Delegation = Schemas["FederationDelegation"];
type Project = Schemas["Project"];

interface State {
  identity: Identity;
  peers: Peer[];
  grants: Grant[];
  routes: Route[];
  delegations: Delegation[];
  projects: Project[];
}

export function FederationPage() {
  const [state, setState] = useState<State>();
  const [dialog, setDialog] = useState<"peer" | "grant" | "route">();
  const [abandoning, setAbandoning] = useState<Delegation>();
  const [disabling, setDisabling] = useState<Peer>();
  const [revoking, setRevoking] = useState<Grant>();
  const [deletingRoute, setDeletingRoute] = useState<Route>();
  const load = useCallback(async () => {
    const [identity, peers, grants, routes, delegations, projects] =
      await Promise.all([
        api.GET("/federation/identity"),
        api.GET("/federation/peers"),
        api.GET("/federation/grants"),
        api.GET("/federation/routes"),
        api.GET("/federation/delegations"),
        api.GET("/projects"),
      ]);
    if (identity.data)
      setState({
        identity: identity.data,
        peers: peers.data?.items ?? [],
        grants: grants.data?.items ?? [],
        routes: routes.data?.items ?? [],
        delegations: delegations.data?.items ?? [],
        projects: projects.data?.items ?? [],
      });
  }, []);
  useLoader(load);
  if (!state) return <Loading />;
  const peerName = (id: string) =>
    state.peers.find((p) => p.id === id)?.name ?? id;
  const done = () => {
    setDialog(undefined);
    setAbandoning(undefined);
    setDisabling(undefined);
    setRevoking(undefined);
    setDeletingRoute(undefined);
    void load();
  };
  return (
    <>
      <PageHeader
        title="Federation"
        description="Run builds at another Samuori installation, or for one."
      />
      <Explain summary="Both administrators agree on the locations; the requester owns the build.">
        <p>
          A build runs at a peer only in locations both administrators agreed
          on. The installation that requests a build owns it and validates the
          image itself; specs and source travel, build plans never do.
        </p>
      </Explain>
      <Section
        title="This installation"
        description="Give these to the administrator of a peer installation."
      >
        <dl className="samuori-details">
          <dt>Organization ID</dt>
          <dd>
            <code>{state.identity.organizationId}</code>
          </dd>
          <dt>CA pin</dt>
          <dd>
            <code>{state.identity.caPin}</code>
          </dd>
          <dt>Listener</dt>
          <dd>
            {state.identity.listening
              ? `Accepting peers at ${(state.identity.hostnames ?? []).join(", ")}`
              : "Off: set federation.listen to run builds for peers"}
          </dd>
        </dl>
      </Section>

      <Section
        title="Peers"
        actions={
          <Button variant="secondary" onPress={() => setDialog("peer")}>
            Add peer
          </Button>
        }
      >
        <Table
          label="Peers"
          rows={state.peers}
          empty="No peer is registered. Choose Add peer to connect another installation."
          columns={[
            {
              id: "name",
              name: "Name",
              isRowHeader: true,
              render: (p) => p.name,
            },
            { id: "endpoint", name: "Endpoint", render: (p) => p.endpoint },
            {
              id: "pin",
              name: "CA pin",
              render: (p) => <code>{p.caPin.slice(0, 19)}…</code>,
            },
            {
              id: "state",
              name: "State",
              render: (p) => (p.disabled ? "Disabled" : "Active"),
            },
            {
              id: "actions",
              name: "Actions",
              render: (p) =>
                !p.disabled && (
                  <Button
                    variant="danger"
                    onPress={() => setDisabling(p)}
                    aria-label={`Disable ${p.name}`}
                  >
                    Disable
                  </Button>
                ),
            },
          ]}
        />
        <Explain summary="A peer's endpoint and CA pin cannot be edited.">
          <p>
            They are the trust anchors for every call to the peer. To change
            them, disable the peer and add it again under a new name with the
            new values, then grant and route to the new peer.
          </p>
        </Explain>
      </Section>

      <Section
        title="Builds peers may run here"
        actions={
          <Button
            variant="secondary"
            onPress={() => setDialog("grant")}
            isDisabled={state.peers.length === 0}
          >
            Grant a peer
          </Button>
        }
      >
        <Table
          label="Grants"
          rows={state.grants}
          empty="No peer may run builds here."
          columns={[
            {
              id: "peer",
              name: "Peer",
              isRowHeader: true,
              render: (g) => peerName(g.peerId),
            },
            {
              id: "project",
              name: "Hosting project",
              render: (g) =>
                state.projects.find((p) => p.id === g.projectId)?.name ??
                g.projectId,
            },
            {
              id: "locations",
              name: "Locations",
              render: (g) => g.locations.join(", "),
            },
            {
              id: "max",
              name: "Active builds",
              align: "end",
              render: (g) => g.maxActive,
            },
            {
              id: "state",
              name: "State",
              render: (g) => (g.revoked ? "Revoked" : "Active"),
            },
            {
              id: "actions",
              name: "Actions",
              render: (g) =>
                !g.revoked && (
                  <Button
                    variant="danger"
                    onPress={() => setRevoking(g)}
                    aria-label={`Revoke the grant for ${peerName(g.peerId)}`}
                  >
                    Revoke
                  </Button>
                ),
            },
          ]}
        />
      </Section>

      <Section
        title="Locations at peers"
        actions={
          <Button
            variant="secondary"
            onPress={() => setDialog("route")}
            isDisabled={state.peers.length === 0}
          >
            Add a peer location
          </Button>
        }
      >
        <Table
          label="Peer locations"
          rows={state.routes}
          empty="No location sends builds to a peer."
          columns={[
            {
              id: "location",
              name: "Location here",
              isRowHeader: true,
              render: (r) => r.location,
            },
            { id: "peer", name: "Peer", render: (r) => peerName(r.peerId) },
            {
              id: "remote",
              name: "Peer location",
              render: (r) => r.remoteLocation,
            },
            {
              id: "actions",
              name: "Actions",
              render: (r) => (
                <Button
                  variant="quiet-danger"
                  onPress={() => setDeletingRoute(r)}
                  aria-label={`Delete the peer location ${r.location}`}
                >
                  Delete
                </Button>
              ),
            },
          ]}
        />
      </Section>

      <Section title="Builds at peers">
        <Table
          label="Delegated builds"
          rows={state.delegations}
          empty="No build has been sent to a peer."
          columns={[
            {
              id: "run",
              name: "Build",
              isRowHeader: true,
              render: (d) => <a href={`/builds/${d.runId}`}>{d.runId}</a>,
            },
            { id: "peer", name: "Peer", render: (d) => peerName(d.peerId) },
            {
              id: "state",
              name: "State",
              render: (d) =>
                d.unreachableSince
                  ? `${d.state}; peer unreachable since ${new Date(d.unreachableSince).toLocaleString()}`
                  : `${d.state}${d.remoteState ? ` (peer: ${d.remoteState})` : ""}`,
            },
            { id: "detail", name: "Detail", render: (d) => d.detail ?? "" },
            {
              id: "actions",
              name: "Actions",
              render: (d) =>
                (d.state === "pending" || d.state === "accepted") && (
                  <Button
                    variant="danger"
                    onPress={() => setAbandoning(d)}
                    aria-label={`Abandon ${d.runId}`}
                  >
                    Abandon
                  </Button>
                ),
            },
          ]}
        />
      </Section>
      <PeerDialog
        isOpen={dialog === "peer"}
        onClose={() => setDialog(undefined)}
        onDone={done}
      />
      <GrantDialog
        isOpen={dialog === "grant"}
        peers={state.peers.filter((p) => !p.disabled)}
        projects={state.projects}
        onClose={() => setDialog(undefined)}
        onDone={done}
      />
      <RouteDialog
        isOpen={dialog === "route"}
        peers={state.peers.filter((p) => !p.disabled)}
        onClose={() => setDialog(undefined)}
        onDone={done}
      />
      <ReasonDialog
        title="Abandon a build at a peer"
        isOpen={!!abandoning}
        explanation="The build fails here at once. The peer is told to stop when it can be reached, and any result it sends later is refused."
        action="Abandon"
        onSubmit={(reason) =>
          api.POST("/federation/delegations/{delegationId}/abandon", {
            params: { path: { delegationId: abandoning?.id ?? "" } },
            body: { reason },
          })
        }
        onClose={() => setAbandoning(undefined)}
        onDone={done}
      />
      <ReasonDialog
        title={`Disable peer ${disabling?.name ?? ""}`}
        isOpen={!!disabling}
        explanation="Federation with this peer stops in both directions at once: no new builds go to it or come from it. This cannot be undone; to federate again, add the peer under a new name."
        action="Disable peer"
        onSubmit={(reason) =>
          api.POST("/federation/peers/{peerId}/disable", {
            params: { path: { peerId: disabling?.id ?? "" } },
            body: { reason },
          })
        }
        onClose={() => setDisabling(undefined)}
        onDone={done}
      />
      <ReasonDialog
        title="Revoke a grant"
        isOpen={!!revoking}
        explanation={`${revoking ? peerName(revoking.peerId) : "The peer"} can no longer start new builds in this project.`}
        action="Revoke grant"
        onSubmit={(reason) =>
          api.POST("/federation/grants/{grantId}/revoke", {
            params: { path: { grantId: revoking?.id ?? "" } },
            body: { reason },
          })
        }
        onClose={() => setRevoking(undefined)}
        onDone={done}
      />
      <DeleteRouteDialog
        route={deletingRoute}
        onClose={() => setDeletingRoute(undefined)}
        onDone={done}
      />
    </>
  );
}

interface DialogProps {
  isOpen: boolean;
  onClose: () => void;
  onDone: () => void;
}

function PeerDialog({ isOpen, onClose, onDone }: DialogProps) {
  const [name, setName] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [pin, setPin] = useState("");
  const [org, setOrg] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    const { error } = await api.POST("/federation/peers", {
      body: { name, endpoint, caPin: pin, organizationId: org },
    });
    if (error) setError(describeError(error));
    else onDone();
  }
  return (
    <Dialog
      title="Add a peer installation"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          placeholder="second-site"
          label="Name"
          value={name}
          onChange={setName}
          isRequired
        />
        <TextField
          placeholder="https://samuori-2.example.com:8443"
          label="Endpoint"
          description="The peer's federation listener, such as https://samuori-b.example:8445"
          value={endpoint}
          onChange={setEndpoint}
          isRequired
        />
        <TextField label="CA pin" value={pin} onChange={setPin} isRequired />
        <TextField
          label="Organization ID"
          value={org}
          onChange={setOrg}
          isRequired
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Add peer</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function GrantDialog({
  isOpen,
  peers,
  projects,
  onClose,
  onDone,
}: DialogProps & { peers: Peer[]; projects: Project[] }) {
  const [peer, setPeer] = useState<string | null>(null);
  const [project, setProject] = useState<string | null>(null);
  const [locations, setLocations] = useState("");
  const [max, setMax] = useState("2");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!peer || !project) return;
    const { error } = await api.POST("/federation/grants", {
      body: {
        peerId: peer,
        projectId: project,
        locations: locations
          .split(",")
          .map((l) => l.trim())
          .filter(Boolean),
        maxActive: Number(max),
      },
    });
    if (error) setError(describeError(error));
    else onDone();
  }
  return (
    <Dialog
      title="Let a peer run builds here"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <Select
          label="Peer"
          options={peers.map((p) => ({ id: p.id, label: p.name }))}
          value={peer}
          onChange={setPeer}
          isRequired
        />
        <Select
          label="Hosting project"
          description="The peer's builds run in this project, under this installation's policies."
          options={projects.map((p) => ({ id: p.id, label: p.name }))}
          value={project}
          onChange={setProject}
          isRequired
        />
        <TextField
          placeholder="eu-west"
          label="Locations"
          description="Comma-separated locations of this installation's own workers"
          value={locations}
          onChange={setLocations}
          isRequired
        />
        <TextField
          label="Active builds"
          value={max}
          onChange={setMax}
          isRequired
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Grant</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function RouteDialog({
  isOpen,
  peers,
  onClose,
  onDone,
}: DialogProps & { peers: Peer[] }) {
  const [peer, setPeer] = useState<string | null>(null);
  const [location, setLocation] = useState("");
  const [remote, setRemote] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!peer) return;
    const { error } = await api.POST("/federation/routes", {
      body: { peerId: peer, location, remoteLocation: remote },
    });
    if (error) setError(describeError(error));
    else onDone();
  }
  return (
    <Dialog
      title="Add a location at a peer"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <Select
          label="Peer"
          options={peers.map((p) => ({ id: p.id, label: p.name }))}
          value={peer}
          onChange={setPeer}
          isRequired
        />
        <TextField
          placeholder="eu-west"
          label="Location here"
          description="Projects choose this name like any location"
          value={location}
          onChange={setLocation}
          isRequired
        />
        <TextField
          placeholder="us-east"
          label="Peer location"
          value={remote}
          onChange={setRemote}
          isRequired
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Add location</Button>
        </div>
      </Form>
    </Dialog>
  );
}

// ReasonDialog confirms a federation change that is recorded with the administrator's reason.
function ReasonDialog({
  title,
  isOpen,
  explanation,
  action,
  onSubmit,
  onClose,
  onDone,
}: DialogProps & {
  title: string;
  explanation: string;
  action: string;
  onSubmit: (reason: string) => Promise<{ error?: unknown }>;
}) {
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    const { error } = await onSubmit(reason);
    if (error) setError(describeError(error));
    else {
      setReason("");
      setError(undefined);
      onDone();
    }
  }
  return (
    <Dialog
      title={title}
      isOpen={isOpen}
      onOpenChange={(o) => {
        if (!o) {
          setError(undefined);
          onClose();
        }
      }}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>{explanation}</p>
        <TextField
          label="Reason"
          value={reason}
          onChange={setReason}
          isRequired
          minLength={10}
          description="At least 10 characters; recorded in the audit log."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="danger">
            {action}
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}

function DeleteRouteDialog({
  route,
  onClose,
  onDone,
}: {
  route?: Route;
  onClose: () => void;
  onDone: () => void;
}) {
  const [error, setError] = useState<string>();
  async function remove() {
    if (!route) return;
    const { error } = await api.DELETE("/federation/routes/{routeId}", {
      params: { path: { routeId: route.id } },
    });
    if (error) setError(describeError(error));
    else onDone();
  }
  const close = () => {
    setError(undefined);
    onClose();
  };
  return (
    <Dialog
      title={`Delete peer location ${route?.location ?? ""}`}
      size="confirm"
      isOpen={!!route}
      onOpenChange={(o) => !o && close()}
    >
      <div className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          The location and its route to the peer are removed; new builds can no
          longer choose it. Builds already sent to the peer keep running. It is
          refused while a pool or a project&apos;s location restriction names
          it.
        </p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={close}>
            Cancel
          </Button>
          <Button
            variant="danger"
            onPress={() => void remove()}
            isDisabled={!!error}
          >
            Delete
          </Button>
        </div>
      </div>
    </Dialog>
  );
}
