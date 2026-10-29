import { useCallback, useState, type FormEvent } from "react";
import { useLoader } from "../../hooks";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import { Dialog } from "../../components/Dialog";
import { Select, TextField } from "../../components/Field";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { orgId, useMe, type Role } from "../../session/session";
import { roleLabel, roleOptions, scopeLabel } from "../labels";
import { useTeams } from "../useTeams";

type User = Schemas["User"];
type Binding = Schemas["RoleBinding"];

export function UsersPage() {
  const me = useMe();
  const { teams } = useTeams();
  const [users, setUsers] = useState<User[]>();
  const [error, setError] = useState<string>();
  const [creating, setCreating] = useState(false);
  const [granting, setGranting] = useState<User>();
  const [renaming, setRenaming] = useState<User>();
  const [toggling, setToggling] = useState<User>();
  const [resetting, setResetting] = useState<User>();
  const [changing, setChanging] = useState<{ user: User; binding: Binding }>();
  const [message, setMessage] = useState<string>();
  const self = me.principal.id;

  const load = useCallback(async () => {
    const { data, error } = await api.GET("/users");
    if (data) setUsers(data.items);
    else setError(describeError(error, "Users could not be loaded."));
  }, []);
  useLoader(load);

  const [confirmDialog, ask] = useConfirm();
  async function revoke(bindingId: string) {
    const { error } = await api.POST("/role-bindings/{bindingId}/revoke", {
      params: { path: { bindingId } },
    });
    if (error) return describeError(error);
    await load();
    return undefined;
  }

  return (
    <>
      <PageHeader
        help="people-and-access"
        title="Users"
        description="Local and directory accounts and their roles. Keep at least one local administrator in case the identity provider fails."
        actions={<Button onPress={() => setCreating(true)}>New user</Button>}
      />
      {error && <Alert>{error}</Alert>}
      {message && <Alert tone="success">{message}</Alert>}
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
                        {!r.source && (
                          <Button
                            variant="quiet"
                            onPress={() => setChanging({ user: u, binding: r })}
                            aria-label={`Change ${roleLabel(r.role)} role of ${u.username}`}
                          >
                            Change role
                          </Button>
                        )}
                        <Button
                          variant="quiet-danger"
                          onPress={() =>
                            ask({
                              title: `Remove the ${roleLabel(r.role)} role from ${u.username}?`,
                              message: r.source
                                ? "It ends at once, and their directory group grants it again at their next sign-in."
                                : "They lose what the role allows at once.",
                              confirmLabel: "Remove role",
                              run: () => revoke(r.id),
                            })
                          }
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
                <div className="samuori-actions">
                  <Button
                    variant="secondary"
                    onPress={() => setGranting(u)}
                    aria-label={`Grant role to ${u.username}`}
                  >
                    Grant role
                  </Button>
                  {u.kind === "service" ? (
                    <span className="samuori-muted">
                      Service identity: manage it under Organization &gt;
                      Identity &amp; sign-in
                    </span>
                  ) : (
                    <Button
                      variant="quiet"
                      onPress={() => setRenaming(u)}
                      aria-label={`Edit name of ${u.username}`}
                    >
                      Edit name
                    </Button>
                  )}
                  {!u.source && u.kind !== "service" && u.id !== self && (
                    <Button
                      variant="quiet-danger"
                      onPress={() => setResetting(u)}
                      aria-label={`Reset password of ${u.username}`}
                    >
                      Reset password
                    </Button>
                  )}
                  {u.kind !== "service" && u.id !== self && (
                    <Button
                      variant={u.disabled ? "quiet" : "quiet-danger"}
                      onPress={() => setToggling(u)}
                      aria-label={`${u.disabled ? "Enable" : "Disable"} ${u.username}`}
                    >
                      {u.disabled ? "Enable" : "Disable"}
                    </Button>
                  )}
                </div>
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
      {renaming && (
        <RenameUserDialog
          user={renaming}
          onClose={() => setRenaming(undefined)}
          onSaved={() => {
            setMessage(`${renaming.username} renamed.`);
            setRenaming(undefined);
            void load();
          }}
        />
      )}
      {toggling && (
        <UserStateDialog
          user={toggling}
          onClose={() => setToggling(undefined)}
          onDone={() => {
            setMessage(
              `${toggling.username} ${toggling.disabled ? "enabled" : "disabled"}.`,
            );
            setToggling(undefined);
            void load();
          }}
        />
      )}
      {resetting && (
        <ResetPasswordDialog
          user={resetting}
          onClose={() => setResetting(undefined)}
        />
      )}
      {changing && (
        <ChangeRoleDialog
          user={changing.user}
          binding={changing.binding}
          scope={scopeLabel(
            changing.binding.scopeType,
            changing.binding.scopeId,
            teams,
          )}
          onClose={() => setChanging(undefined)}
          onChanged={() => {
            setMessage(`Role of ${changing.user.username} changed.`);
            setChanging(undefined);
            void load();
          }}
        />
      )}
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
      {confirmDialog}
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
          placeholder="alex"
          label="Username"
          value={form.username}
          onChange={set("username")}
          isRequired
          pattern="[a-z0-9][a-z0-9._\-]{1,63}"
          autoComplete="off"
        />
        <TextField
          placeholder="Alex Example"
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

function RenameUserDialog({
  user,
  onClose,
  onSaved,
}: {
  user: User;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState(user.displayName);
  const [error, setError] = useState<string>();

  async function submit(e: FormEvent) {
    e.preventDefault();
    const { error } = await api.PATCH("/users/{userId}", {
      params: { path: { userId: user.id } },
      body: { displayName: name.trim() },
    });
    if (error) setError(describeError(error));
    else onSaved();
  }

  return (
    <Dialog
      title={`Edit name of ${user.username}`}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          placeholder="Alex Example"
          label="Display name"
          value={name}
          onChange={setName}
          isRequired
          maxLength={128}
          description="The username stays the same."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Save</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function UserStateDialog({
  user,
  onClose,
  onDone,
}: {
  user: User;
  onClose: () => void;
  onDone: () => void;
}) {
  const [error, setError] = useState<string>();
  const enable = user.disabled;

  async function apply() {
    const { error } = await api.POST("/users/{userId}/state", {
      params: { path: { userId: user.id } },
      body: { enabled: enable },
    });
    if (error) setError(describeError(error));
    else onDone();
  }

  return (
    <Dialog
      title={`${enable ? "Enable" : "Disable"} ${user.displayName}?`}
      size="confirm"
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      {error && <Alert>{error}</Alert>}
      {enable ? (
        <p>
          {user.username} can sign in again. API tokens revoked when the account
          was disabled stay revoked.
        </p>
      ) : (
        <p>
          {user.username} is signed out everywhere, their API tokens are
          revoked, and they cannot sign in through any provider. Users are never
          deleted, so the audit log keeps naming who did what; you can enable
          the account again later.
        </p>
      )}
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          Cancel
        </Button>
        <Button
          variant={enable ? "primary" : "danger"}
          onPress={() => void apply()}
        >
          {enable ? "Enable user" : "Disable user"}
        </Button>
      </div>
    </Dialog>
  );
}

function ResetPasswordDialog({
  user,
  onClose,
}: {
  user: User;
  onClose: () => void;
}) {
  const [error, setError] = useState<string>();
  const [issued, setIssued] = useState<Schemas["TemporaryPassword"]>();

  async function reset() {
    const { data, error } = await api.POST("/users/{userId}/password-reset", {
      params: { path: { userId: user.id } },
    });
    if (!data) setError(describeError(error));
    else setIssued(data);
  }

  return (
    <Dialog
      title={`Reset password of ${user.displayName}?`}
      size="confirm"
      isOpen
      isDismissable={!issued}
      onOpenChange={(o) => !o && onClose()}
    >
      {error && <Alert>{error}</Alert>}
      {issued ? (
        <Alert tone="success">
          <p>
            <strong>
              Copy the temporary password now. It will not be shown again.
            </strong>
          </p>
          <code className="samuori-secret" data-testid="temporary-password">
            {issued.temporaryPassword}
          </code>
          <p>
            Give it to {user.username} over a secure channel. They must choose a
            new password when they next sign in, before{" "}
            {new Date(issued.expiresAt).toLocaleString()}.
          </p>
          <div className="samuori-actions">
            <Button
              variant="secondary"
              onPress={() =>
                void navigator.clipboard?.writeText(issued.temporaryPassword)
              }
            >
              Copy
            </Button>
            <Button variant="quiet" onPress={onClose}>
              Done
            </Button>
          </div>
        </Alert>
      ) : (
        <>
          <p>
            {user.username} is signed out everywhere and gets a temporary
            password, valid for 24 hours, that must be replaced at the next
            sign-in. Their API tokens keep working; disable the account to stop
            them.
          </p>
          <div className="samuori-actions">
            <Button variant="secondary" onPress={onClose}>
              Cancel
            </Button>
            <Button variant="danger" onPress={() => void reset()}>
              Reset password
            </Button>
          </div>
        </>
      )}
    </Dialog>
  );
}

function ChangeRoleDialog({
  user,
  binding,
  scope,
  onClose,
  onChanged,
}: {
  user: User;
  binding: Binding;
  scope: string;
  onClose: () => void;
  onChanged: () => void;
}) {
  const [role, setRole] = useState<string>(binding.role);
  const [error, setError] = useState<string>();

  async function submit(e: FormEvent) {
    e.preventDefault();
    const { error } = await api.PATCH("/role-bindings/{bindingId}", {
      params: { path: { bindingId: binding.id } },
      body: { role: role as Role },
    });
    if (error) setError(describeError(error));
    else onChanged();
  }

  return (
    <Dialog
      title={`Change role of ${user.displayName}`}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          Replaces {roleLabel(binding.role)} on {scope} in one step. The scope
          stays the same.
        </p>
        <Select
          label="New role"
          options={roleOptions}
          value={role}
          onChange={setRole}
          isRequired
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit" isDisabled={role === binding.role}>
            Change role
          </Button>
        </div>
      </Form>
    </Dialog>
  );
}
