import { useCallback, useState, type FormEvent } from "react";
import { useLoader } from "../../hooks";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Select, TextField } from "../../components/Field";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { orgId, useMe, type Role } from "../../session/session";
import { roleLabel, roleOptions, scopeLabel } from "../labels";
import { useTeams } from "../useTeams";

type User = Schemas["User"];

export function UsersPage() {
  const me = useMe();
  const { teams } = useTeams();
  const [users, setUsers] = useState<User[]>();
  const [error, setError] = useState<string>();
  const [creating, setCreating] = useState(false);
  const [granting, setGranting] = useState<User>();

  const load = useCallback(async () => {
    const { data, error } = await api.GET("/users");
    if (data) setUsers(data.items);
    else setError(describeError(error, "Users could not be loaded."));
  }, []);
  useLoader(load);

  async function revoke(bindingId: string) {
    const { error } = await api.POST("/role-bindings/{bindingId}/revoke", {
      params: { path: { bindingId } },
    });
    setError(error ? describeError(error) : undefined);
    await load();
  }

  return (
    <>
      <PageHeader
        help="people-and-access"
        title="Users"
        description="Local and directory accounts and their roles. Keep at least one local administrator: it is the way in if the identity provider fails."
        actions={<Button onPress={() => setCreating(true)}>New user</Button>}
      />
      {error && <Alert>{error}</Alert>}
      {!users ? (
        <Loading />
      ) : (
        <Table
          label="Users"
          rows={users}
          columns={[
            {
              id: "name",
              name: "User",
              isRowHeader: true,
              render: (u) => (
                <>
                  <strong>{u.displayName}</strong>
                  <div className="samuori-muted">
                    {u.username} · {u.source ?? "local account"}
                  </div>
                </>
              ),
            },
            {
              id: "roles",
              name: "Roles",
              render: (u) =>
                u.roles.length === 0 ? (
                  <span className="samuori-muted">No roles</span>
                ) : (
                  <ul className="samuori-plain-list">
                    {u.roles.map((r) => (
                      <li key={r.id}>
                        {roleLabel(r.role)} —{" "}
                        {scopeLabel(r.scopeType, r.scopeId, teams)}{" "}
                        {r.source && (
                          <span className="samuori-muted">
                            (from a {r.source} group; granted again at next
                            sign-in){" "}
                          </span>
                        )}
                        <Button
                          variant="quiet"
                          onPress={() => void revoke(r.id)}
                          aria-label={`Remove ${roleLabel(r.role)} role from ${u.username}`}
                        >
                          Remove
                        </Button>
                      </li>
                    ))}
                  </ul>
                ),
            },
            {
              id: "status",
              name: "Status",
              render: (u) => (u.disabled ? "Disabled" : "Active"),
            },
            {
              id: "actions",
              name: "Actions",
              render: (u) => (
                <Button
                  variant="secondary"
                  onPress={() => setGranting(u)}
                  aria-label={`Grant role to ${u.username}`}
                >
                  Grant role
                </Button>
              ),
            },
          ]}
        />
      )}
      <CreateUserDialog
        isOpen={creating}
        onOpenChange={setCreating}
        onCreated={() => {
          setCreating(false);
          void load();
        }}
      />
      <GrantDialog
        user={granting}
        orgId={orgId(me) ?? ""}
        teams={teams ?? []}
        onClose={() => setGranting(undefined)}
        onGranted={() => {
          setGranting(undefined);
          void load();
        }}
      />
    </>
  );
}

function CreateUserDialog({
  isOpen,
  onOpenChange,
  onCreated,
}: {
  isOpen: boolean;
  onOpenChange: (o: boolean) => void;
  onCreated: () => void;
}) {
  const [form, setForm] = useState({
    username: "",
    displayName: "",
    password: "",
  });
  const [error, setError] = useState<string>();
  const set = (k: keyof typeof form) => (v: string) =>
    setForm((f) => ({ ...f, [k]: v }));

  async function submit(e: FormEvent) {
    e.preventDefault();
    const { data, error } = await api.POST("/users", {
      body: {
        username: form.username,
        displayName: form.displayName || undefined,
        password: form.password,
      },
    });
    if (!data) {
      setError(describeError(error));
      return;
    }
    setForm({ username: "", displayName: "", password: "" });
    setError(undefined);
    onCreated();
  }

  return (
    <Dialog title="New user" isOpen={isOpen} onOpenChange={onOpenChange}>
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          label="Username"
          value={form.username}
          onChange={set("username")}
          isRequired
          pattern="[a-z0-9][a-z0-9._\-]{1,63}"
          autoComplete="off"
        />
        <TextField
          label="Display name"
          value={form.displayName}
          onChange={set("displayName")}
          maxLength={128}
        />
        <TextField
          label="Initial password"
          type="password"
          value={form.password}
          onChange={set("password")}
          isRequired
          autoComplete="new-password"
          description="At least 12 characters. Share it with the user over a secure channel."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit">Create user</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function GrantDialog({
  user,
  orgId,
  teams,
  onClose,
  onGranted,
}: {
  user?: User;
  orgId: string;
  teams: { id: string; name: string }[];
  onClose: () => void;
  onGranted: () => void;
}) {
  const [role, setRole] = useState<string>("developer");
  const [scope, setScope] = useState<string>("");
  const [error, setError] = useState<string>();
  const scopes = [
    ...teams.map((t) => ({ id: `team:${t.id}`, label: `Team ${t.name}` })),
    { id: `organization:${orgId}`, label: "Whole organization" },
  ];

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!user || !scope) {
      setError("Choose where the role applies.");
      return;
    }
    const [scopeType, scopeId] = scope.split(":") as [
      "team" | "organization",
      string,
    ];
    const { error } = await api.POST("/role-bindings", {
      body: { principalId: user.id, role: role as Role, scopeType, scopeId },
    });
    if (error) {
      setError(describeError(error));
      return;
    }
    setError(undefined);
    onGranted();
  }

  return (
    <Dialog
      title={`Grant role to ${user?.displayName ?? ""}`}
      isOpen={!!user}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <Select
          label="Role"
          options={roleOptions}
          value={role}
          onChange={setRole}
          isRequired
        />
        <Select
          label="Applies to"
          options={scopes}
          value={scope || null}
          onChange={setScope}
          isRequired
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Grant role</Button>
        </div>
      </Form>
    </Dialog>
  );
}
