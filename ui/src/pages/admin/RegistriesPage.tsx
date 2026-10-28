import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Checkbox, Select, TextField } from "../../components/Field";
import { Alert, Loading, PageHeader, Section } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";
import { useTeams } from "../useTeams";
import { DownloadPolicy } from "./DownloadPolicy";

type Registry = Schemas["Registry"];
type Profile = Schemas["RegistryCredentialProfile"];
type Destination = Schemas["Destination"];

export function RegistriesPage() {
  const [registries, setRegistries] = useState<Registry[]>();
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [destinations, setDestinations] = useState<Destination[]>([]);
  const [secrets, setSecrets] = useState<Schemas["LocalSecret"][]>([]);
  const [keys, setKeys] = useState<Schemas["SigningKey"][]>([]);
  const [keyName, setKeyName] = useState("");
  const [keyHome, setKeyHome] = useState<string | null>("controller");
  const [keyRef, setKeyRef] = useState("");
  const [keyServices, setKeyServices] = useState<Schemas["SecretProvider"][]>(
    [],
  );
  const [projects, setProjects] = useState<{ id: string; label: string }[]>([]);
  const [dialog, setDialog] = useState<
    "registry" | "profile" | "destination"
  >();
  const [editingRegistry, setEditingRegistry] = useState<Registry>();
  const [removingRegistry, setRemovingRegistry] = useState<Registry>();
  const [editingProfile, setEditingProfile] = useState<Profile>();
  const [removingProfile, setRemovingProfile] = useState<Profile>();
  const [editingDestination, setEditingDestination] = useState<Destination>();
  const [removingDestination, setRemovingDestination] =
    useState<Schemas["Destination"]>();
  const [message, setMessage] = useState<{
    tone: "success" | "error";
    text: string;
  }>();
  const load = useCallback(async () => {
    const [r, p, d, prov, proj] = await Promise.all([
      api.GET("/registries"),
      api.GET("/registry-credential-profiles"),
      api.GET("/destinations"),
      api.GET("/secret-providers"),
      api.GET("/projects", { params: { query: { limit: 100 } } }),
    ]);
    setRegistries(r.data?.items ?? []);
    setProfiles(p.data?.items ?? []);
    setDestinations(d.data?.items ?? []);
    const k = await api.GET("/signing-keys");
    setKeys(k.data?.items ?? []);
    setProjects(
      (proj.data?.items ?? []).map((x) => ({ id: x.id, label: x.name })),
    );
    setKeyServices((prov.data?.items ?? []).filter((x) => x.type !== "local"));
    const local = prov.data?.items.find((x) => x.type === "local");
    if (local) {
      const s = await api.GET("/secret-providers/{providerId}/local-secrets", {
        params: { path: { providerId: local.id } },
      });
      setSecrets(s.data?.items ?? []);
    }
  }, []);
  useLoader(load);
  if (!registries) return <Loading />;
  const regName = (id: string) =>
    registries.find((r) => r.id === id)?.name ?? id;
  const done = (text: string) => {
    setDialog(undefined);
    setEditingRegistry(undefined);
    setRemovingRegistry(undefined);
    setEditingProfile(undefined);
    setRemovingProfile(undefined);
    setMessage({ tone: "success", text });
    void load();
  };

  async function createKey(e: FormEvent) {
    e.preventDefault();
    setMessage(undefined);
    const remote = keyHome !== "controller" && keyHome !== null;
    const { error } = await api.POST("/signing-keys", {
      body: remote
        ? { name: keyName, providerId: keyHome, keyRef }
        : { name: keyName },
    });
    if (error) setMessage({ tone: "error", text: describeError(error) });
    else {
      setKeyName("");
      done(
        `Created signing key ${keyName}; it signs from now on and the previous key is retired.`,
      );
    }
  }

  async function test(p: Profile) {
    setMessage(undefined);
    const { data, error } = await api.POST(
      "/registry-credential-profiles/{profileId}/test",
      { params: { path: { profileId: p.id } } },
    );
    if (error || !data)
      setMessage({ tone: "error", text: describeError(error) });
    else
      setMessage(
        data.status === "ok"
          ? {
              tone: "success",
              text: `${p.name}: the registry accepted the credentials.`,
            }
          : {
              tone: "error",
              text: `${p.name}: ${data.detail ?? "the test failed"}`,
            },
      );
    void load();
  }

  return (
    <>
      <PageHeader
        help="registries"
        title="Registries"
        description="Where validated images are published. Credentials stay on the controller; builds never see them."
      />
      {message && <Alert tone={message.tone}>{message.text}</Alert>}
      <Section
        title="Registries"
        actions={
          <Button variant="secondary" onPress={() => setDialog("registry")}>
            Add registry
          </Button>
        }
      >
        <Table
          label="Registries"
          rows={registries}
          empty="No registry yet."
          columns={[
            {
              id: "name",
              name: "Name",
              isRowHeader: true,
              render: (r) => r.name,
            },
            {
              id: "endpoint",
              name: "Endpoint",
              render: (r) => <code>{r.endpoint}</code>,
            },
            {
              id: "kind",
              name: "Kind",
              render: (r) => (r.kind === "ecr" ? "Amazon ECR" : "OCI"),
            },
            {
              id: "flags",
              name: "Connection",
              render: (r) =>
                [
                  r.plainHttp ? "plain HTTP" : "HTTPS",
                  r.allowPrivateNetwork ? "private network" : "public network",
                ].join(", "),
            },
            {
              id: "actions",
              name: "Actions",
              render: (r) => (
                <div className="samuori-actions justify-start">
                  <Button
                    variant="quiet"
                    aria-label={`View or edit ${r.name}`}
                    onPress={() => setEditingRegistry(r)}
                  >
                    View or edit
                  </Button>
                  <Button
                    variant="quiet"
                    aria-label={`Delete ${r.name}`}
                    onPress={() => setRemovingRegistry(r)}
                  >
                    Delete
                  </Button>
                </div>
              ),
            },
          ]}
        />
      </Section>
      <Section
        title="Push credentials"
        actions={
          <Button
            variant="secondary"
            onPress={() => setDialog("profile")}
            isDisabled={registries.length === 0}
          >
            Add credentials
          </Button>
        }
      >
        <Table
          label="Push credentials"
          rows={profiles}
          empty="No credentials yet."
          columns={[
            {
              id: "name",
              name: "Name",
              isRowHeader: true,
              render: (p) => p.name,
            },
            {
              id: "registry",
              name: "Registry",
              render: (p) => regName(p.registryId),
            },
            { id: "purpose", name: "Purpose", render: (p) => p.purpose },
            { id: "type", name: "Type", render: (p) => profileType(p) },
            {
              id: "test",
              name: "Last test",
              render: (p) =>
                p.lastTest ? (
                  <>
                    {p.lastTest.status}
                    {p.lastTest.detail ? ` — ${p.lastTest.detail}` : ""}
                    {p.lastTest.steps && (
                      <ul className="samuori-plain-list">
                        {p.lastTest.steps.map((s) => (
                          <li key={s.step}>
                            {s.step}: {s.status}
                            {s.detail ? ` (${s.detail})` : ""}
                          </li>
                        ))}
                      </ul>
                    )}
                  </>
                ) : (
                  "not tested"
                ),
            },
            {
              id: "actions",
              name: "Actions",
              render: (p) => (
                <div className="samuori-actions justify-start">
                  <Button
                    variant="quiet"
                    onPress={() => void test(p)}
                    aria-label={`Test ${p.name}`}
                  >
                    Test
                  </Button>
                  <Button
                    variant="quiet"
                    aria-label={`View or edit ${p.name}`}
                    onPress={() => setEditingProfile(p)}
                  >
                    View or edit
                  </Button>
                  <Button
                    variant="quiet"
                    aria-label={`Delete ${p.name}`}
                    onPress={() => setRemovingProfile(p)}
                  >
                    Delete
                  </Button>
                </div>
              ),
            },
          ]}
        />
      </Section>
      {registries.some((r) => r.kind === "ecr") && (
        <EcrRepositories registries={registries} profiles={profiles} />
      )}
      <ImageSources profiles={profiles} />
      <Section
        title="Destinations"
        actions={
          <Button
            variant="secondary"
            onPress={() => setDialog("destination")}
            isDisabled={profiles.length === 0}
          >
            Add destination
          </Button>
        }
      >
        <Table
          label="Destinations"
          rows={destinations}
          empty="No destination yet."
          columns={[
            {
              id: "name",
              name: "Name",
              isRowHeader: true,
              render: (d) => d.name,
            },
            {
              id: "repo",
              name: "Repository",
              render: (d) => (
                <code>{`${regName(d.registryId)}/${d.repository}`}</code>
              ),
            },
            {
              id: "projects",
              name: "Projects",
              render: (d) =>
                (d.allowedProjectIds ?? [])
                  .map((id) => projects.find((p) => p.id === id)?.label ?? id)
                  .join(", ") || "none",
            },
            {
              id: "rules",
              name: "Rules",
              render: (d) =>
                [
                  d.requiresApproval ? "approval required" : "",
                  d.immutableTags ? "immutable tags" : "",
                  (d.protectedTags ?? []).length
                    ? `protected: ${(d.protectedTags ?? []).join(", ")}`
                    : "",
                ]
                  .filter(Boolean)
                  .join("; ") || "—",
            },
            {
              id: "actions",
              name: "Actions",
              render: (d) => (
                <div className="samuori-actions justify-start">
                  <Button
                    variant="quiet"
                    aria-label={`View or edit destination ${d.name}`}
                    onPress={() => setEditingDestination(d)}
                  >
                    View or edit
                  </Button>
                  <Button
                    variant="quiet"
                    aria-label={`Delete destination ${d.name}`}
                    onPress={() => setRemovingDestination(d)}
                  >
                    Delete
                  </Button>
                </div>
              ),
            },
          ]}
        />
      </Section>
      <Section
        title="Signing keys"
        description={
          <>
            Destinations that sign images use the active key. A key is generated
            on the controller, or stays in OpenBao or Vault Transit or AWS KMS;
            its private part never leaves where it is held. Verify published
            images with <code>cosign verify --key cosign.pub IMAGE</code>.
          </>
        }
      >
        <Table
          label="Signing keys"
          rows={keys}
          empty="No signing key yet."
          columns={[
            {
              id: "name",
              name: "Name",
              isRowHeader: true,
              render: (k) => k.name,
            },
            { id: "state", name: "State", render: (k) => k.state },
            {
              id: "held",
              name: "Held by",
              render: (k) =>
                !k.backend || k.backend === "local"
                  ? "Controller"
                  : `${k.backend}: ${k.keyRef ?? ""}`,
            },
            {
              id: "created",
              name: "Created",
              render: (k) => new Date(k.createdAt).toLocaleDateString(),
            },
            {
              id: "public",
              name: "Public key",
              render: (k) => (
                <details>
                  <summary>Show</summary>
                  <code className="samuori-secret">{k.publicKey}</code>
                </details>
              ),
            },
          ]}
        />
        <Form
          onSubmit={createKey}
          className="samuori-form samuori-form--inline"
        >
          <TextField
            label="New key name"
            value={keyName}
            onChange={setKeyName}
            isRequired
            pattern="[a-z0-9]([\-a-z0-9]{0,61}[a-z0-9])?"
            description="Creating a key retires the current one; old signatures still verify."
          />
          {keyServices.length > 0 && (
            <Select
              label="Key held by"
              options={[
                { id: "controller", label: "This controller (generated now)" },
                ...keyServices.map((p) => ({ id: p.id, label: p.name })),
              ]}
              value={keyHome}
              onChange={setKeyHome}
            />
          )}
          {keyHome !== "controller" && (
            <TextField
              label="Key reference"
              value={keyRef}
              onChange={setKeyRef}
              isRequired
              description="Transit: mount/key, such as transit/samuori-release. KMS: key ID, ARN or alias. It must be ECDSA P-256."
            />
          )}
          <Button type="submit">Create signing key</Button>
        </Form>
      </Section>
      <RegistryDialog
        isOpen={dialog === "registry"}
        onClose={() => setDialog(undefined)}
        onDone={(r) => done(`Added registry ${r.name}.`)}
      />
      {editingRegistry && (
        <RegistryDialog
          key={editingRegistry.id}
          existing={editingRegistry}
          isOpen
          onClose={() => setEditingRegistry(undefined)}
          onDone={(r) => done(`Saved registry ${r.name}.`)}
        />
      )}
      {removingRegistry && (
        <DeleteDialog
          name={removingRegistry.name}
          text="Samuori forgets this registry; nothing in the registry itself is touched. It is refused while credentials, destinations, image sources, repository requests or publications refer to it."
          remove={() =>
            api.DELETE("/registries/{registryId}", {
              params: { path: { registryId: removingRegistry.id } },
            })
          }
          onClose={() => setRemovingRegistry(undefined)}
          onDone={() => done(`Deleted registry ${removingRegistry.name}.`)}
        />
      )}
      {editingProfile && (
        <ProfileDialog
          key={editingProfile.id}
          existing={editingProfile}
          isOpen
          registries={registries}
          secrets={secrets}
          onClose={() => setEditingProfile(undefined)}
          onDone={(p) =>
            done(`Saved credentials ${p.name}. Test them again before use.`)
          }
        />
      )}
      {editingDestination && (
        <DestinationDialog
          key={editingDestination.id}
          existing={editingDestination}
          isOpen
          profiles={profiles}
          projects={projects}
          regName={regName}
          onClose={() => setEditingDestination(undefined)}
          onDone={(d) => done(`Saved destination ${d.name}.`)}
        />
      )}
      {removingDestination && (
        <DeleteDialog
          name={removingDestination.name}
          text="Samuori forgets this destination; the registry itself is not touched. It is refused once anything was published to it or requested for it, because publication history keeps it. BuildSpecs that name it can no longer be admitted."
          remove={() =>
            api.DELETE("/destinations/{destinationId}", {
              params: { path: { destinationId: removingDestination.id } },
            })
          }
          onClose={() => setRemovingDestination(undefined)}
          onDone={() =>
            done(`Deleted destination ${removingDestination.name}.`)
          }
        />
      )}
      {removingProfile && (
        <DeleteDialog
          name={removingProfile.name}
          text="Samuori forgets these credentials. It is refused while destinations or image sources use them. Their secret stays reserved for the controller, so builds can never use it; once nothing refers to it you can delete it under Secrets."
          remove={() =>
            api.DELETE("/registry-credential-profiles/{profileId}", {
              params: { path: { profileId: removingProfile.id } },
            })
          }
          onClose={() => setRemovingProfile(undefined)}
          onDone={() => done(`Deleted credentials ${removingProfile.name}.`)}
        />
      )}
      <ProfileDialog
        isOpen={dialog === "profile"}
        registries={registries}
        secrets={secrets}
        onClose={() => setDialog(undefined)}
        onDone={(p) =>
          done(
            `Added credentials ${p.name}. The secret is now reserved for the controller.`,
          )
        }
      />
      <DestinationDialog
        isOpen={dialog === "destination"}
        profiles={profiles}
        projects={projects}
        regName={regName}
        onClose={() => setDialog(undefined)}
        onDone={(d) => done(`Added destination ${d.name}.`)}
      />
      <DownloadPolicy />
    </>
  );
}

const ecrHost =
  /^[0-9]{12}\.dkr\.ecr(-fips)?\.[a-z0-9-]+\.amazonaws\.com(\.cn)?$/;

function profileType(p: Profile) {
  if (p.type !== "ecr") return p.type;
  const method = p.ecr?.authMethod;
  if (method === "workload-identity") return "ECR: this controller's IAM role";
  if (method === "assume-role")
    return `ECR: assumes ${p.ecr?.roleArn ?? "a role"}`;
  if (method === "access-key") return "ECR: access key";
  return "ecr";
}

function DeleteDialog({
  name,
  text,
  remove,
  onClose,
  onDone,
}: {
  name: string;
  text: string;
  remove: () => Promise<{ error?: unknown }>;
  onClose: () => void;
  onDone: () => void;
}) {
  const [error, setError] = useState<string>();
  async function confirm() {
    const { error } = await remove();
    if (error) setError(describeError(error));
    else onDone();
  }
  return (
    <Dialog
      title={`Delete ${name}?`}
      size="confirm"
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      {error && <Alert>{error}</Alert>}
      <p>{text}</p>
      <div className="samuori-actions">
        <Button variant="secondary" onPress={onClose}>
          Cancel
        </Button>
        <Button variant="danger" onPress={() => void confirm()}>
          Delete
        </Button>
      </div>
    </Dialog>
  );
}

function RegistryDialog({
  existing,
  isOpen,
  onClose,
  onDone,
}: {
  existing?: Registry;
  isOpen: boolean;
  onClose: () => void;
  onDone: (r: Registry) => void;
}) {
  const e0 = existing;
  const [name, setName] = useState(e0?.name ?? "");
  const [kind, setKind] = useState<string | null>(e0?.kind ?? "oci");
  const [endpoint, setEndpoint] = useState(
    e0?.kind === "oci" ? (e0.endpoint ?? "") : "",
  );
  const [account, setAccount] = useState(e0?.awsAccountId ?? "");
  const [region, setRegion] = useState(e0?.region ?? "");
  const [plain, setPlain] = useState(e0?.plainHttp ?? false);
  const [priv, setPriv] = useState(e0?.allowPrivateNetwork ?? false);
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (e0) {
      const { data, error } = await api.PUT("/registries/{registryId}", {
        params: { path: { registryId: e0.id } },
        body:
          e0.kind === "ecr"
            ? {
                name,
                awsAccountId: account,
                region,
                plainHttp: false,
                allowPrivateNetwork: false,
              }
            : {
                name,
                endpoint,
                plainHttp: plain,
                allowPrivateNetwork: priv,
              },
      });
      if (error || !data) setError(describeError(error));
      else onDone(data);
      return;
    }
    const { data, error } = await api.POST("/registries", {
      body:
        kind === "ecr"
          ? {
              name,
              kind: "ecr",
              awsAccountId: account,
              region,
              plainHttp: false,
              allowPrivateNetwork: false,
            }
          : {
              name,
              kind: "oci",
              endpoint,
              plainHttp: plain,
              allowPrivateNetwork: priv,
            },
    });
    if (error || !data) setError(describeError(error));
    else onDone(data);
  }
  return (
    <Dialog
      title={e0 ? `Registry ${e0.name}` : "Add registry"}
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        {e0 && (
          <p>
            The endpoint, plain HTTP, account and region cannot change while
            credentials of this registry hold a stored secret, so a password or
            key is never sent somewhere new. Delete those credentials first.
          </p>
        )}
        <TextField label="Name" value={name} onChange={setName} isRequired />
        <Select
          isDisabled={!!e0}
          description={
            e0
              ? "The kind cannot change. Add a new registry for another kind, then delete this one."
              : undefined
          }
          label="Kind"
          options={[
            {
              id: "oci",
              label: "OCI registry (Harbor, Distribution and others)",
            },
            { id: "ecr", label: "Amazon ECR" },
          ]}
          value={kind}
          onChange={setKind}
        />
        {kind === "ecr" ? (
          <>
            <TextField
              label="AWS account ID"
              value={account}
              onChange={setAccount}
              isRequired
            />
            <TextField
              label="Region"
              value={region}
              onChange={setRegion}
              isRequired
              description="Such as eu-west-1. The endpoint is the account's own ECR registry."
            />
          </>
        ) : (
          <>
            <TextField
              label="Endpoint"
              value={endpoint}
              onChange={setEndpoint}
              isRequired
              description="host or host:port, for example registry.example.com"
            />
            {ecrHost.test(endpoint.trim()) && (
              <Alert tone="info">
                This is an Amazon ECR endpoint.{" "}
                {e0
                  ? "Add it again as kind Amazon ECR"
                  : "Choose kind Amazon ECR"}{" "}
                so its credentials can use this controller&apos;s IAM role (EC2
                instance profile) or assume a role instead of a stored password.
              </Alert>
            )}
            <Checkbox isSelected={plain} onChange={setPlain}>
              Plain HTTP (development registries only)
            </Checkbox>
            <Checkbox isSelected={priv} onChange={setPriv}>
              The registry is on a private network
            </Checkbox>
          </>
        )}
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">{e0 ? "Save" : "Add"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function ProfileDialog({
  existing,
  isOpen,
  registries,
  secrets,
  onClose,
  onDone,
}: {
  existing?: Profile;
  isOpen: boolean;
  registries: Registry[];
  secrets: Schemas["LocalSecret"][];
  onClose: () => void;
  onDone: (p: Profile) => void;
}) {
  const e0 = existing;
  const [name, setName] = useState(e0?.name ?? "");
  const [registry, setRegistry] = useState<string | null>(
    e0?.registryId ?? null,
  );
  const [type, setType] = useState<string>(
    e0 && e0.type !== "ecr" ? e0.type : "oci-basic",
  );
  const [username, setUsername] = useState(e0?.username ?? "");
  const [secret, setSecret] = useState<string | null>(e0?.secretRef ?? null);
  const [purpose, setPurpose] = useState<string | null>(e0?.purpose ?? "push");
  const [ecrAuth, setEcrAuth] = useState<string | null>(
    e0?.ecr?.authMethod ?? "workload-identity",
  );
  const [roleArn, setRoleArn] = useState(e0?.ecr?.roleArn ?? "");
  const [externalId, setExternalId] = useState(e0?.ecr?.externalId ?? "");
  const [scopes, setScopes] = useState((e0?.repositoryScopes ?? []).join(", "));
  const [error, setError] = useState<string>();
  const ecr = registries.find((r) => r.id === registry)?.kind === "ecr";
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!registry) return;
    const body: Schemas["RegistryCredentialProfileCreate"] = {
      name,
      registryId: registry,
      type: ecr ? "ecr" : (type as "oci-basic"),
      purpose: purpose === "pull" ? "pull" : "push",
      repositoryScopes: scopes
        .split(",")
        .map((x) => x.trim())
        .filter(Boolean),
    };
    if (ecr) {
      body.ecr = {
        apiEndpoint: e0?.ecr?.apiEndpoint,
        authMethod: ecrAuth as "workload-identity",
        ...(ecrAuth === "assume-role"
          ? { roleArn, externalId: externalId || undefined }
          : {}),
      };
      if (ecrAuth === "access-key" && secret) body.secretRef = secret;
    } else {
      if (type === "oci-basic") body.username = username;
      if (type !== "anonymous" && secret) body.secretRef = secret;
    }
    const { data, error } = e0
      ? await api.PUT("/registry-credential-profiles/{profileId}", {
          params: { path: { profileId: e0.id } },
          body,
        })
      : await api.POST("/registry-credential-profiles", { body });
    if (error || !data) setError(describeError(error));
    else onDone(data);
  }
  return (
    <Dialog
      title={e0 ? `Credentials ${e0.name}` : "Add registry credentials"}
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField label="Name" value={name} onChange={setName} isRequired />
        <Select
          label="Registry"
          options={registries.map((r) => ({ id: r.id, label: r.name }))}
          value={registry}
          onChange={setRegistry}
          isRequired
          isDisabled={!!e0}
          description={
            e0
              ? "The registry and purpose cannot change, so a stored secret is only ever sent to its own registry."
              : "IAM roles are offered for registries of kind Amazon ECR; an ECR endpoint added as kind OCI offers only passwords and tokens."
          }
        />
        <Select
          isDisabled={!!e0}
          label="Purpose"
          options={[
            { id: "push", label: "Push: publication, controller only" },
            { id: "pull", label: "Pull: held by an authorizing mirror" },
          ]}
          value={purpose}
          onChange={setPurpose}
        />
        {ecr ? (
          <>
            <Select
              label="AWS identity"
              options={[
                {
                  id: "workload-identity",
                  label:
                    "This controller's IAM role (EC2 instance profile or the default AWS credentials)",
                },
                {
                  id: "assume-role",
                  label: "Assume a role (also cross-account)",
                },
                {
                  id: "access-key",
                  label: "Access key (long-lived; not recommended)",
                },
              ]}
              value={ecrAuth}
              onChange={setEcrAuth}
              description="The first two store no secret: the controller's own AWS identity signs every request. IAM Roles Anywhere is not available yet."
            />
            {ecrAuth === "assume-role" && (
              <>
                <TextField
                  label="Role ARN"
                  value={roleArn}
                  onChange={setRoleArn}
                  isRequired
                />
                <TextField
                  label="External ID"
                  value={externalId}
                  onChange={setExternalId}
                />
              </>
            )}
            {ecrAuth === "access-key" && (
              <Select
                label="Secret"
                options={secrets.map((s) => ({ id: s.id, label: s.name }))}
                value={secret}
                onChange={setSecret}
                isRequired
                description="Holding ACCESS_KEY_ID:SECRET_ACCESS_KEY; it becomes reserved for the controller."
              />
            )}
          </>
        ) : (
          <Select
            label="Authentication"
            options={[
              { id: "oci-basic", label: "Username and password" },
              { id: "oci-token", label: "Access token" },
              { id: "anonymous", label: "None (development registries)" },
            ]}
            value={type}
            onChange={setType}
          />
        )}
        {!ecr && type === "oci-basic" && (
          <TextField
            label="Username"
            value={username}
            onChange={setUsername}
            isRequired
          />
        )}
        {!ecr && type !== "anonymous" && (
          <Select
            label="Secret"
            options={secrets.map((s) => ({ id: s.id, label: s.name }))}
            value={secret}
            onChange={setSecret}
            isRequired
            description="Store the password or token under Secrets first. It becomes reserved for the controller."
          />
        )}
        <TextField
          label="Repository scopes"
          value={scopes}
          onChange={setScopes}
          description="Optional, comma-separated patterns such as team/*. Empty means any repository."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">{e0 ? "Save" : "Add"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function DestinationDialog({
  existing,
  isOpen,
  profiles,
  projects,
  regName,
  onClose,
  onDone,
}: {
  existing?: Destination;
  isOpen: boolean;
  profiles: Profile[];
  projects: { id: string; label: string }[];
  regName: (id: string) => string;
  onClose: () => void;
  onDone: (d: Destination) => void;
}) {
  const e0 = existing;
  const [name, setName] = useState(e0?.name ?? "");
  const [profile, setProfile] = useState<string | null>(
    e0?.pushProfileId ?? null,
  );
  const [repository, setRepository] = useState(e0?.repository ?? "");
  const [allowed, setAllowed] = useState<string[]>(e0?.allowedProjectIds ?? []);
  const [protectedTags, setProtectedTags] = useState(
    e0 ? (e0.protectedTags ?? []).join(", ") : "latest",
  );
  const [immutable, setImmutable] = useState(e0?.immutableTags ?? false);
  const [approval, setApproval] = useState(e0?.requiresApproval ?? false);
  const [restricted, setRestricted] = useState(e0?.acceptsRestricted ?? false);
  const [sign, setSign] = useState(e0?.signImages ?? false);
  const [provenance, setProvenance] = useState(e0?.attachProvenance ?? false);
  const [promotionOnly, setPromotionOnly] = useState(
    e0?.promotionOnly ?? false,
  );
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    const p = profiles.find((x) => x.id === profile);
    if (!p) return;
    const body: Schemas["DestinationCreate"] = {
      name,
      registryId: e0?.registryId ?? p.registryId,
      repository,
      pushProfileId: p.id,
      allowedProjectIds: allowed,
      protectedTags: protectedTags
        .split(",")
        .map((t) => t.trim())
        .filter(Boolean),
      immutableTags: immutable,
      requiresApproval: approval,
      acceptsRestricted: restricted,
      signImages: sign,
      attachProvenance: provenance,
      promotionOnly,
    };
    const { data, error } = e0
      ? await api.PUT("/destinations/{destinationId}", {
          params: { path: { destinationId: e0.id } },
          body,
        })
      : await api.POST("/destinations", { body });
    if (error || !data) setError(describeError(error));
    else onDone(data);
  }
  return (
    <Dialog
      title={e0 ? `Destination ${e0.name}` : "Add destination"}
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        {e0 && (
          <p>
            The registry ({regName(e0.registryId)}) cannot change. The
            repository cannot change once anything was published to or requested
            for this destination, so its history keeps its meaning; the rules
            can.
          </p>
        )}
        <TextField
          label="Name"
          value={name}
          onChange={setName}
          isRequired
          description="Specs refer to destinations by this name."
        />
        <Select
          label="Push credentials"
          options={profiles
            .filter(
              (p) =>
                p.purpose === "push" && (!e0 || p.registryId === e0.registryId),
            )
            .map((p) => ({
              id: p.id,
              label: `${p.name} (${regName(p.registryId)})`,
            }))}
          value={profile}
          onChange={setProfile}
          isRequired
        />
        <TextField
          label="Repository"
          value={repository}
          onChange={setRepository}
          isRequired
          description="For example payments/orders-api"
        />
        <fieldset className="samuori-fieldset">
          <legend>Projects that may publish here</legend>
          {projects.map((p) => (
            <Checkbox
              key={p.id}
              isSelected={allowed.includes(p.id)}
              onChange={(on) =>
                setAllowed(
                  on ? [...allowed, p.id] : allowed.filter((x) => x !== p.id),
                )
              }
            >
              {p.label}
            </Checkbox>
          ))}
        </fieldset>
        <TextField
          label="Protected tags"
          value={protectedTags}
          onChange={setProtectedTags}
          description="Comma-separated patterns that can never be moved to another image."
        />
        <Checkbox isSelected={immutable} onChange={setImmutable}>
          No tag may be moved to another image
        </Checkbox>
        <Checkbox isSelected={approval} onChange={setApproval}>
          Publications need an approval
        </Checkbox>
        <Checkbox isSelected={sign} onChange={setSign}>
          Sign images with the organization&apos;s signing key
        </Checkbox>
        <Checkbox isSelected={provenance} onChange={setProvenance}>
          Attach signed build provenance (SLSA)
        </Checkbox>
        <Checkbox isSelected={promotionOnly} onChange={setPromotionOnly}>
          Accept only promotions of images published elsewhere
        </Checkbox>
        <Checkbox isSelected={restricted} onChange={setRestricted}>
          Accept restricted images (passed only through an exception)
        </Checkbox>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">{e0 ? "Save" : "Add"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function EcrRepositories({
  registries,
  profiles,
}: {
  registries: Registry[];
  profiles: Profile[];
}) {
  const ecrProfiles = profiles.filter((p) => p.type === "ecr");
  const [profile, setProfile] = useState<string | null>(
    ecrProfiles[0]?.id ?? null,
  );
  const [repos, setRepos] = useState<Schemas["EcrRepository"][]>();
  const [name, setName] = useState("");
  const [message, setMessage] = useState<{
    tone: "error" | "success";
    text: string;
  }>();
  const chosen = ecrProfiles.find((p) => p.id === profile);
  const registry = registries.find((r) => r.id === chosen?.registryId);
  async function list() {
    if (!chosen) return;
    const { data, error } = await api.GET(
      "/registries/{registryId}/repositories",
      {
        params: {
          path: { registryId: chosen.registryId },
          query: { profileId: chosen.id },
        },
      },
    );
    if (error || !data)
      setMessage({ tone: "error", text: describeError(error) });
    else {
      setMessage(undefined);
      setRepos(data.items);
    }
  }
  async function create(e: FormEvent) {
    e.preventDefault();
    if (!chosen) return;
    const { error } = await api.POST("/registries/{registryId}/repositories", {
      params: { path: { registryId: chosen.registryId } },
      body: { profileId: chosen.id, name, immutableTags: true },
    });
    if (error) setMessage({ tone: "error", text: describeError(error) });
    else {
      setMessage({
        tone: "success",
        text: `Created ${name} with immutable tags.`,
      });
      setName("");
      await list();
    }
  }
  return (
    <section aria-labelledby="ecr-heading">
      <h2 id="ecr-heading">ECR repositories</h2>
      {ecrProfiles.length === 0 ? (
        <p className="samuori-muted">
          Add ECR credentials to list or create repositories.
        </p>
      ) : (
        <>
          <Select
            label="ECR credentials"
            options={ecrProfiles.map((p) => ({ id: p.id, label: p.name }))}
            value={profile}
            onChange={setProfile}
          />
          <div className="samuori-actions">
            <Button variant="secondary" onPress={() => void list()}>
              List repositories{registry ? ` in ${registry.name}` : ""}
            </Button>
          </div>
          {message && <Alert tone={message.tone}>{message.text}</Alert>}
          {repos && (
            <Table
              label="ECR repositories"
              rows={repos.map((r) => ({ ...r, id: r.name }))}
              empty="No repositories visible to these credentials."
              columns={[
                {
                  id: "name",
                  name: "Repository",
                  isRowHeader: true,
                  render: (r) => r.name,
                },
                { id: "uri", name: "URI", render: (r) => <code>{r.uri}</code> },
                {
                  id: "tags",
                  name: "Tags",
                  render: (r) => (r.immutableTags ? "immutable" : "mutable"),
                },
              ]}
            />
          )}
          <Form onSubmit={create} className="samuori-form samuori-form--inline">
            <TextField
              label="New repository"
              value={name}
              onChange={setName}
              isRequired
              description="Uses the selected credentials, whose role should only create repositories."
            />
            <Button type="submit">Create repository</Button>
          </Form>
        </>
      )}
    </section>
  );
}

function ImageSources({ profiles }: { profiles: Profile[] }) {
  const { teams } = useTeams();
  const [sources, setSources] = useState<Schemas["ImageSource"][]>();
  const [adding, setAdding] = useState(false);
  const [config, setConfig] = useState<{
    name: string;
    config: string;
    trustBundle: string;
  }>();
  const [message, setMessage] = useState<{
    tone: "error" | "success";
    text: string;
  }>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/image-sources");
    setSources(data?.items ?? []);
  }, []);
  useLoader(load);
  const teamName = (id: string) => teams?.find((t) => t.id === id)?.name ?? id;
  async function setState(s: Schemas["ImageSource"], enabled: boolean) {
    const { error } = await api.POST("/image-sources/{sourceId}/state", {
      params: { path: { sourceId: s.id } },
      body: { enabled },
    });
    setMessage(
      error ? { tone: "error", text: describeError(error) } : undefined,
    );
    await load();
  }
  async function showConfig(s: Schemas["ImageSource"]) {
    const { data, error } = await api.GET(
      "/image-sources/{sourceId}/mirror-config",
      {
        params: { path: { sourceId: s.id } },
      },
    );
    if (error || !data)
      setMessage({ tone: "error", text: describeError(error) });
    else setConfig({ name: s.name, ...data });
  }
  async function rotate() {
    const { error } = await api.POST("/mirror-token-keys/rotations");
    setMessage(
      error
        ? { tone: "error", text: describeError(error) }
        : {
            tone: "success",
            text: "New mirror token key created. Add its certificate to every mirror's trust bundle.",
          },
    );
  }
  return (
    <Section
      title="Approved image sources"
      description="Private base images are pulled through an authorizing mirror. Samuori gives each build a short-lived token for only the repositories its team may use; upstream credentials stay with the mirror."
      actions={
        <>
          <Button variant="secondary" onPress={() => setAdding(true)}>
            Add image source
          </Button>
          <Button variant="secondary" onPress={() => void rotate()}>
            Rotate mirror token key
          </Button>
        </>
      }
    >
      {message && <Alert tone={message.tone}>{message.text}</Alert>}
      {!sources ? (
        <Loading />
      ) : (
        <Table
          label="Approved image sources"
          rows={sources}
          empty="No private image sources. Builds use public images through the configured mirrors."
          columns={[
            {
              id: "name",
              name: "Name",
              isRowHeader: true,
              render: (s) => s.name,
            },
            {
              id: "route",
              name: "Upstream via mirror",
              render: (s) => <code>{`${s.upstream} → ${s.mirror}`}</code>,
            },
            {
              id: "patterns",
              name: "Repositories",
              render: (s) => s.repositoryPatterns.join(", "),
            },
            {
              id: "teams",
              name: "Teams",
              render: (s) => s.teamIds.map(teamName).join(", "),
            },
            { id: "status", name: "Status", render: (s) => s.status },
            {
              id: "actions",
              name: "Actions",
              render: (s) => (
                <div className="samuori-actions">
                  <Button variant="quiet" onPress={() => void showConfig(s)}>
                    Mirror configuration for {s.name}
                  </Button>
                  <Button
                    variant="quiet"
                    onPress={() => void setState(s, s.status !== "active")}
                  >
                    {s.status === "active" ? "Disable" : "Enable"} {s.name}
                  </Button>
                </div>
              ),
            },
          ]}
        />
      )}
      <ImageSourceDialog
        isOpen={adding}
        teams={teams ?? []}
        pullProfiles={profiles.filter((p) => p.purpose === "pull")}
        onClose={() => setAdding(false)}
        onDone={(s) => {
          setAdding(false);
          setMessage({ tone: "success", text: `Added ${s.name}.` });
          void load();
        }}
      />
      {config && (
        <Dialog
          title={`Mirror configuration for ${config.name}`}
          isOpen
          onOpenChange={(o) => !o && setConfig(undefined)}
          size="wide"
        >
          <p className="samuori-muted">
            Run CNCF Distribution with this configuration, set
            REGISTRY_PROXY_PASSWORD from the pull credentials, and save the
            certificates below as /etc/samuori-mirror/trust.pem.
          </p>
          <pre className="samuori-code">{config.config}</pre>
          <pre className="samuori-code">
            {config.trustBundle || "No mirror token key yet: rotate one first."}
          </pre>
          <div className="samuori-actions">
            <Button onPress={() => setConfig(undefined)}>Close</Button>
          </div>
        </Dialog>
      )}
    </Section>
  );
}

function ImageSourceDialog({
  isOpen,
  teams,
  pullProfiles,
  onClose,
  onDone,
}: {
  isOpen: boolean;
  teams: { id: string; name: string }[];
  pullProfiles: Profile[];
  onClose: () => void;
  onDone: (s: Schemas["ImageSource"]) => void;
}) {
  const [name, setName] = useState("");
  const [upstream, setUpstream] = useState("");
  const [mirror, setMirror] = useState("");
  const [patterns, setPatterns] = useState("");
  const [team, setTeam] = useState<string | null>(null);
  const [profile, setProfile] = useState<string | null>(null);
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!team) return;
    const { data, error } = await api.POST("/image-sources", {
      body: {
        name,
        upstream,
        mirror,
        repositoryPatterns: patterns
          .split(",")
          .map((p) => p.trim())
          .filter(Boolean),
        teamIds: [team],
        pullProfileId: profile ?? undefined,
      },
    });
    if (error || !data) setError(describeError(error));
    else onDone(data);
  }
  return (
    <Dialog
      title="Add image source"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField label="Name" value={name} onChange={setName} isRequired />
        <TextField
          label="Upstream registry"
          value={upstream}
          onChange={setUpstream}
          isRequired
          description="As image references name it, such as registry.corp.example"
        />
        <TextField
          label="Mirror"
          value={mirror}
          onChange={setMirror}
          isRequired
          description="host:port of the authorizing mirror, as workers reach it"
        />
        <TextField
          label="Repositories"
          value={patterns}
          onChange={setPatterns}
          isRequired
          description="Comma-separated patterns, such as payments/*"
        />
        <Select
          label="Team"
          options={teams.map((t) => ({ id: t.id, label: t.name }))}
          value={team}
          onChange={setTeam}
          isRequired
          description="One team per mirror unless shared mirrors are enabled."
        />
        {pullProfiles.length > 0 && (
          <Select
            label="Pull credentials the mirror uses"
            options={pullProfiles.map((p) => ({ id: p.id, label: p.name }))}
            value={profile}
            onChange={setProfile}
          />
        )}
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Add</Button>
        </div>
      </Form>
    </Dialog>
  );
}
