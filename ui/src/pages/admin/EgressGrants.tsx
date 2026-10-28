import { useCallback, useState, type FormEvent } from "react";
import { Form, Link } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Checkbox, TextField } from "../../components/Field";
import { Alert, Badge, Section } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";

type Granted = Schemas["GrantedDestinations"];
type Group = Granted["destinations"][number];

/** Destinations projects were given one request at a time, which an administrator can move into the policy
 * (docs/architecture/egress.md#promoting-project-allowances). */
export function EgressGrants({ onPromoted }: { onPromoted: () => void }) {
  const [data, setData] = useState<Granted>();
  const [promoting, setPromoting] = useState<Group>();
  const [done, setDone] = useState<string>();
  const [names, setNames] = useState<Record<string, string>>({});
  const load = useCallback(async () => {
    const [granted, projects] = await Promise.all([
      api.GET("/egress-allowances/by-destination"),
      api.GET("/projects", { params: { query: { limit: 100 } } }),
    ]);
    setNames(
      Object.fromEntries(
        (projects.data?.items ?? []).map((p) => [p.id, p.name]),
      ),
    );
    setData(granted.data);
  }, []);
  useLoader(load);
  if (!data) return null;
  const projects = (g: Group) =>
    [...new Set(g.allowances.map((a) => a.projectId))].length;
  return (
    <Section
      title="Granted to projects"
      description="Destinations opened for single projects through approved access requests. When several projects need the same one, add it to the organization policy instead; the project allowances are then revoked."
    >
      {done && <Alert tone="success">{done}</Alert>}
      <Table
        label="Destinations granted to projects"
        dependencies={[names]}
        rows={data.destinations.map((g) => ({
          ...g,
          id: `${g.destination}|${g.network}`,
        }))}
        empty="No project has its own network access."
        columns={[
          {
            id: "dest",
            name: "Destination",
            isRowHeader: true,
            render: (g) => (
              <>
                <code>{g.destination}</code>
                {g.network === "internal" && (
                  <>
                    {" "}
                    <Badge tone="warn">Internal network</Badge>
                  </>
                )}
              </>
            ),
            sortValue: (g) => g.destination,
          },
          {
            id: "projects",
            name: "Projects",
            render: (g) => (
              <span className="inline-flex flex-wrap gap-x-2">
                {[...new Set(g.allowances.map((a) => a.projectId))].map(
                  (id) => (
                    <Link
                      key={id}
                      href={`/projects/${id}`}
                      className="samuori-link"
                    >
                      {names[id] ?? id}
                    </Link>
                  ),
                )}
              </span>
            ),
            sortValue: (g) => projects(g),
          },
          {
            id: "methods",
            name: "For builds",
            render: (g) =>
              [...new Set(g.allowances.flatMap((a) => a.methods))].join(", "),
          },
          {
            id: "actions",
            name: "Actions",
            render: (g) => (
              <Button
                variant="secondary"
                onPress={() => {
                  setDone(undefined);
                  setPromoting(g);
                }}
                aria-label={`Add ${g.destination} to the organization policy`}
              >
                Add to organization policy
              </Button>
            ),
          },
        ]}
      />
      {data.presets.length > 0 && (
        <p className="samuori-muted mt-3 text-sm">
          {data.presets.length} preset allowance(s), such as{" "}
          <code>{data.presets[0]?.preset}</code>, are listed on their project
          pages. A preset allowed in the organization policy applies only to
          builds that ask for it, so preset allowances cannot be moved here.
        </p>
      )}
      <PromoteDialog
        group={promoting}
        onClose={() => setPromoting(undefined)}
        onDone={(text) => {
          setPromoting(undefined);
          setDone(text);
          void load();
          onPromoted();
        }}
      />
    </Section>
  );
}

function PromoteDialog({
  group,
  onClose,
  onDone,
}: {
  group?: Group;
  onClose: () => void;
  onDone: (text: string) => void;
}) {
  const [reason, setReason] = useState("");
  const [dockerfile, setDockerfile] = useState(true);
  const [error, setError] = useState<string>();
  const methods = [...new Set(group?.allowances.flatMap((a) => a.methods))];
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!group) return;
    setError(undefined);
    const { data, error } = await api.POST("/egress-allowances/promotion", {
      body: {
        host: group.host,
        port: group.port,
        network: group.network,
        noDockerfile: !dockerfile,
        reason,
      },
    });
    if (!data) {
      setError(describeError(error));
      return;
    }
    setReason("");
    setDockerfile(true);
    onDone(
      `${group.destination} is in egress policy version ${data.policyVersion}; ${data.revoked.length} project allowance(s) were revoked.`,
    );
  }
  return (
    <Dialog
      title="Add to organization policy"
      isOpen={group !== undefined}
      onOpenChange={(o) => !o && onClose()}
    >
      {group && (
        <Form onSubmit={submit} className="samuori-form">
          {error && <Alert>{error}</Alert>}
          <p>
            Opens <code>{group.destination}</code> as{" "}
            {group.network === "internal" ? "an internal" : "a public"}{" "}
            destination for <strong>every project</strong>. It replaces{" "}
            {group.allowances.length} project allowance(s), which are revoked
            with this reason.
          </p>
          <Alert tone="warning">
            The projects asked for {methods.join(", ")} builds only. The policy
            entry opens it for every build method
            {dockerfile
              ? ", Dockerfile builds included"
              : " except Dockerfile builds"}
            .
          </Alert>
          <Checkbox isSelected={dockerfile} onChange={setDockerfile}>
            Dockerfile builds may reach it too
          </Checkbox>
          <TextField
            label="Reason"
            value={reason}
            onChange={setReason}
            isRequired
            minLength={10}
            maxLength={1024}
            multiline
            description="Recorded with the new policy version and on each revoked allowance."
          />
          <p className="samuori-muted">
            Unsaved changes elsewhere on this page are discarded when the policy
            reloads.
          </p>
          <div className="samuori-actions">
            <Button variant="secondary" onPress={onClose}>
              Cancel
            </Button>
            <Button type="submit">Add and revoke</Button>
          </div>
        </Form>
      )}
    </Dialog>
  );
}
