import { useCallback, useState, type FormEvent } from "react";
import { useLoader } from "../hooks";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { Checkbox, Select, TextField } from "../components/Field";
import { Alert, Loading, PageHeader } from "../components/Feedback";
import { Table } from "../components/Table";
import { useMe } from "../session/session";
import { roleLabel, scopeLabel } from "./labels";
import { useTeams } from "./useTeams";

type Token = Schemas["ApiToken"];

const lifetimes = [
  { id: "7", label: "7 days" },
  { id: "30", label: "30 days" },
  { id: "90", label: "90 days" },
  { id: "365", label: "1 year" },
];

export function AccountPage() {
  const me = useMe();
  const { teams } = useTeams();
  const [tokens, setTokens] = useState<Token[]>();
  const [creating, setCreating] = useState(false);
  const [issued, setIssued] = useState<string>();
  const [revoking, setRevoking] = useState<Token>();
  const [error, setError] = useState<string>();

  const load = useCallback(async () => {
    const { data, error } = await api.GET("/me/api-tokens");
    if (data) setTokens(data.items);
    else setError(describeError(error, "API tokens could not be loaded."));
  }, []);
  useLoader(load);

  async function revoke(t: Token) {
    const { error } = await api.POST("/me/api-tokens/{tokenId}/revoke", {
      params: { path: { tokenId: t.id } },
    });
    setRevoking(undefined);
    if (error) setError(describeError(error));
    await load();
  }

  return (
    <>
      <PageHeader title="Account" help="roles" />
      <section aria-labelledby="profile-h" className="samuori-section">
        <h2 id="profile-h">Profile</h2>
        <dl className="samuori-details">
          <dt>Name</dt>
          <dd>{me.displayName}</dd>
          <dt>Roles</dt>
          <dd>
            {me.roles.length === 0 ? (
              "None"
            ) : (
              <ul className="samuori-plain-list">
                {me.roles.map((r) => (
                  <li key={`${r.role}-${r.scopeId}`}>
                    {roleLabel(r.role)} —{" "}
                    {scopeLabel(r.scopeType, r.scopeId, teams)}
                  </li>
                ))}
              </ul>
            )}
          </dd>
        </dl>
      </section>

      <section aria-labelledby="tokens-h" className="samuori-section">
        <div className="samuori-section-head">
          <div>
            <h2 id="tokens-h">API tokens</h2>
            <p className="samuori-muted">
              Tokens let scripts and pipelines call the API as you. They expire
              and can be revoked at any time.
            </p>
          </div>
          <Button onPress={() => setCreating(true)}>Create token</Button>
        </div>
        {error && <Alert>{error}</Alert>}
        {issued && (
          <Alert tone="success">
            <p>
              <strong>
                Copy your new token now. It will not be shown again.
              </strong>
            </p>
            <code className="samuori-secret" data-testid="issued-token">
              {issued}
            </code>
            <div className="samuori-actions">
              <Button
                variant="secondary"
                onPress={() => void navigator.clipboard?.writeText(issued)}
              >
                Copy
              </Button>
              <Button variant="quiet" onPress={() => setIssued(undefined)}>
                Done
              </Button>
            </div>
          </Alert>
        )}
        {!tokens ? (
          <Loading />
        ) : (
          <Table
            label="API tokens"
            rows={tokens}
            empty="You have no API tokens."
            columns={[
              {
                id: "name",
                name: "Name",
                isRowHeader: true,
                render: (t) => t.name,
              },
              {
                id: "access",
                name: "Access",
                render: (t) =>
                  t.scopes?.includes("write") ? "Read and write" : "Read only",
              },
              {
                id: "expires",
                name: "Expires",
                render: (t) => new Date(t.expiresAt).toLocaleDateString(),
              },
              {
                id: "used",
                name: "Last used",
                render: (t) =>
                  t.lastUsedAt
                    ? new Date(t.lastUsedAt).toLocaleString()
                    : "Never",
              },
              {
                id: "actions",
                name: "Actions",
                render: (t) => (
                  <Button
                    variant="danger"
                    onPress={() => setRevoking(t)}
                    aria-label={`Revoke ${t.name}`}
                  >
                    Revoke
                  </Button>
                ),
              },
            ]}
          />
        )}
      </section>

      <CreateTokenDialog
        isOpen={creating}
        onOpenChange={setCreating}
        onCreated={(value) => {
          setCreating(false);
          setIssued(value);
          void load();
        }}
      />
      <Dialog
        title="Revoke token?"
        size="confirm"
        isOpen={!!revoking}
        onOpenChange={(o) => !o && setRevoking(undefined)}
      >
        <p>Anything using “{revoking?.name}” will stop working immediately.</p>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => setRevoking(undefined)}>
            Cancel
          </Button>
          <Button
            variant="danger"
            onPress={() => revoking && void revoke(revoking)}
          >
            Revoke token
          </Button>
        </div>
      </Dialog>
    </>
  );
}

function CreateTokenDialog({
  isOpen,
  onOpenChange,
  onCreated,
}: {
  isOpen: boolean;
  onOpenChange: (o: boolean) => void;
  onCreated: (token: string) => void;
}) {
  const [name, setName] = useState("");
  const [days, setDays] = useState("30");
  const [write, setWrite] = useState(false);
  const [error, setError] = useState<string>();

  async function submit(e: FormEvent) {
    e.preventDefault();
    const expiresAt = new Date(
      Date.now() + Number(days) * 86_400_000,
    ).toISOString();
    const { data, error } = await api.POST("/me/api-tokens", {
      body: { name, expiresAt, scopes: write ? ["read", "write"] : ["read"] },
    });
    if (!data) {
      setError(describeError(error));
      return;
    }
    setName("");
    setWrite(false);
    onCreated(data.token);
  }

  return (
    <Dialog
      title="Create API token"
      isOpen={isOpen}
      onOpenChange={onOpenChange}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          label="Name"
          value={name}
          onChange={setName}
          isRequired
          maxLength={128}
          description="What will use this token, for example “release pipeline”."
        />
        <Select
          label="Expires after"
          options={lifetimes}
          value={days}
          onChange={setDays}
        />
        <Checkbox isSelected={write} onChange={setWrite}>
          Allow changes (without this, the token can only read)
        </Checkbox>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit">Create token</Button>
        </div>
      </Form>
    </Dialog>
  );
}
