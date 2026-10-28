import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Checkbox, Select, TextField } from "../../components/Field";
import { Alert, Loading, PageHeader, Section } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { slug } from "../../format";
import { useLoader } from "../../hooks";
import { orgId, useMe, type Role } from "../../session/session";
import { roleLabel, roleOptions } from "../labels";
import { useTeams } from "../useTeams";

type Provider = Schemas["IdentityProvider"];
type ServiceIdentity = Schemas["ServiceIdentity"];
type Message = { tone: "success" | "error"; text: string };

export function IdentityPage() {
  const [providers, setProviders] = useState<Provider[]>();
  const [services, setServices] = useState<ServiceIdentity[]>([]);
  const [settings, setSettings] = useState<Schemas["IdentitySettings"]>();
  const [secrets, setSecrets] = useState<Schemas["LocalSecret"][]>([]);
  const [localProvider, setLocalProvider] = useState<string>();
  const [message, setMessage] = useState<Message>();
  const [dialog, setDialog] = useState<"provider" | "service">();
  const [mapping, setMapping] = useState<Provider>();
  const [editing, setEditing] = useState<Provider>();
  const [issuing, setIssuing] = useState<ServiceIdentity>();
  const [testing, setTesting] = useState<string>();
  const [tested, setTested] = useState<Record<string, Message | undefined>>({});

  const load = useCallback(async () => {
    const [p, s, st, prov] = await Promise.all([
      api.GET("/identity-providers"),
      api.GET("/service-identities"),
      api.GET("/identity-settings"),
      api.GET("/secret-providers"),
    ]);
    setProviders(p.data?.items ?? []);
    setServices(s.data?.items ?? []);
    setSettings(st.data);
    const local = prov.data?.items.find((x) => x.type === "local");
    setLocalProvider(local?.id);
    if (local) {
      const ls = await api.GET("/secret-providers/{providerId}/local-secrets", {
        params: { path: { providerId: local.id } },
      });
      setSecrets(ls.data?.items ?? []);
    }
  }, []);
  useLoader(load);

  const report = (error: unknown, ok: string) => {
    setMessage(
      error
        ? { tone: "error", text: describeError(error) }
        : { tone: "success", text: ok },
    );
    void load();
  };

  async function setEnabled(p: Provider, enabled: boolean) {
    const { error } = await api.POST("/identity-providers/{providerId}/state", {
      params: { path: { providerId: p.id } },
      body: { enabled },
    });
    report(error, `${p.displayName} ${enabled ? "enabled" : "disabled"}.`);
  }

  async function test(p: Provider) {
    setTesting(p.id);
    let result: Message;
    try {
      const { data, error } = await api.POST(
        "/identity-providers/{providerId}/test",
        { params: { path: { providerId: p.id } } },
      );
      result =
        error || !data
          ? { tone: "error", text: describeError(error) }
          : data.status === "ok"
            ? { tone: "success", text: "Connection test passed." }
            : { tone: "error", text: data.detail ?? "The test failed." };
    } catch (err) {
      result = { tone: "error", text: describeError(err) };
    } finally {
      setTesting(undefined);
    }
    setTested((t) => ({ ...t, [p.id]: result }));
    setMessage({ tone: result.tone, text: `${p.displayName}: ${result.text}` });
  }

  async function setLocalLogin(localLogin: string) {
    const { data, error } = await api.PUT("/identity-settings", {
      body: { localLogin: localLogin as "all" | "administrators" },
    });
    if (data) setSettings(data);
    report(error, "Sign-in settings saved.");
  }

  async function setServiceEnabled(s: ServiceIdentity, enabled: boolean) {
    const { error } = await api.POST("/service-identities/{identityId}/state", {
      params: { path: { identityId: s.id } },
      body: { enabled },
    });
    report(error, `${s.username} ${enabled ? "enabled" : "disabled"}.`);
  }

  async function revoke(s: ServiceIdentity, tokenId: string) {
    const { error } = await api.DELETE(
      "/service-identities/{identityId}/api-tokens/{tokenId}",
      { params: { path: { identityId: s.id, tokenId } } },
    );
    report(error, "Token revoked.");
  }

  if (!providers || !settings) return <Loading />;
  return (
    <>
      <PageHeader
        help="identity"
        title="Identity"
        description="Sign-in through OIDC or LDAP, group-to-role mappings, and service identities for automation."
      />
      {message && <Alert tone={message.tone}>{message.text}</Alert>}

      <section aria-labelledby="signin-heading">
        <h2 id="signin-heading">Sign-in</h2>
        <Select
          label="Local passwords"
          options={[
            { id: "all", label: "Everyone with a local account" },
            {
              id: "administrators",
              label: "Administrators only (recovery access)",
            },
          ]}
          value={settings.localLogin}
          onChange={(v) => void setLocalLogin(v)}
          description="With recovery access, people sign in through an identity provider; administrators keep local passwords for when it is unavailable, and each such sign-in is audited."
        />
        <p className="samuori-muted">
          {settings.oidcRedirectUrl ? (
            <>
              Register this redirect URL with OIDC providers:{" "}
              <code>{settings.oidcRedirectUrl}</code>
            </>
          ) : (
            "OIDC sign-in needs the controller's external URL (externalUrl) to be configured."
          )}
        </p>
      </section>

      <SessionSettings
        key={JSON.stringify(settings.session)}
        current={settings.session}
        onSaved={(data, error) => {
          if (data) setSettings(data);
          report(error, "Session settings saved.");
        }}
      />

      <Section
        title="Identity providers"
        actions={
          <Button variant="secondary" onPress={() => setDialog("provider")}>
            Add identity provider
          </Button>
        }
      >
        <Table
          label="Identity providers"
          rows={providers}
          empty="No identity providers. Everyone signs in with a local account."
          columns={[
            {
              id: "name",
              name: "Provider",
              isRowHeader: true,
              render: (p) => (
                <>
                  <strong>{p.displayName}</strong>
                  <div className="samuori-muted">
                    {p.kind === "oidc" ? p.oidc?.issuer : p.ldap?.url}
                  </div>
                </>
              ),
            },
            {
              id: "kind",
              name: "Kind",
              render: (p) => p.kind.toUpperCase(),
            },
            {
              id: "state",
              name: "State",
              render: (p) => (p.enabled ? "enabled" : "disabled"),
            },
            {
              id: "actions",
              name: "Actions",
              render: (p) => (
                <div className="samuori-actions">
                  {tested[p.id] && (
                    <span className="samuori-muted" role="status">
                      {tested[p.id]?.text}
                    </span>
                  )}
                  <Button
                    variant="quiet"
                    isDisabled={testing === p.id}
                    onPress={() => void test(p)}
                  >
                    {testing === p.id ? "Testing…" : `Test ${p.displayName}`}
                  </Button>
                  <Button variant="quiet" onPress={() => setEditing(p)}>
                    Edit {p.displayName}
                  </Button>
                  <Button variant="quiet" onPress={() => setMapping(p)}>
                    Group mappings for {p.displayName}
                  </Button>
                  <Button
                    variant="quiet"
                    onPress={() => void setEnabled(p, !p.enabled)}
                  >
                    {p.enabled ? "Disable" : "Enable"} {p.displayName}
                  </Button>
                </div>
              ),
            },
          ]}
        />
      </Section>

      <Section
        title="Service identities"
        description="Accounts for automation. They cannot sign in; they use expiring API tokens. Grant their roles under Users."
        actions={
          <Button variant="secondary" onPress={() => setDialog("service")}>
            Add service identity
          </Button>
        }
      >
        <Table
          label="Service identities"
          rows={services}
          empty="No service identities."
          columns={[
            {
              id: "name",
              name: "Identity",
              isRowHeader: true,
              render: (s) => (
                <>
                  <strong>{s.displayName}</strong>
                  <div className="samuori-muted">{s.username}</div>
                </>
              ),
            },
            {
              id: "tokens",
              name: "Tokens",
              render: (s) =>
                s.tokens.length === 0 ? (
                  "none"
                ) : (
                  <ul className="samuori-plain-list">
                    {s.tokens.map((t) => (
                      <li key={t.id}>
                        {t.name}, expires{" "}
                        {new Date(t.expiresAt).toLocaleDateString()}{" "}
                        <Button
                          variant="quiet"
                          onPress={() => void revoke(s, t.id)}
                        >
                          Revoke {t.name}
                        </Button>
                      </li>
                    ))}
                  </ul>
                ),
            },
            {
              id: "state",
              name: "State",
              render: (s) => (s.disabled ? "disabled" : "active"),
            },
            {
              id: "actions",
              name: "Actions",
              render: (s) => (
                <div className="samuori-actions">
                  <Button variant="quiet" onPress={() => setIssuing(s)}>
                    Issue token for {s.username}
                  </Button>
                  <Button
                    variant="quiet"
                    onPress={() => void setServiceEnabled(s, s.disabled)}
                  >
                    {s.disabled ? "Enable" : "Disable"} {s.username}
                  </Button>
                </div>
              ),
            },
          ]}
        />
      </Section>

      <ProviderDialog
        isOpen={dialog === "provider"}
        secrets={secrets}
        localProvider={localProvider}
        onClose={() => setDialog(undefined)}
        onDone={(p) => {
          setDialog(undefined);
          void load();
          report(
            undefined,
            `${p.displayName} added. Test it, add group mappings, then enable it.`,
          );
        }}
      />
      {editing && (
        <ProviderDialog
          isOpen
          editing={editing}
          secrets={secrets}
          localProvider={localProvider}
          onClose={() => setEditing(undefined)}
          onDone={(p) => {
            setEditing(undefined);
            setTested((t) => ({ ...t, [p.id]: undefined }));
            report(undefined, `${p.displayName} saved. Test it again.`);
          }}
        />
      )}
      <ServiceDialog
        isOpen={dialog === "service"}
        onClose={() => setDialog(undefined)}
        onDone={(s) => {
          setDialog(undefined);
          report(undefined, `${s.username} added.`);
        }}
      />
      {mapping && (
        <MappingDialog
          provider={mapping}
          onClose={() => setMapping(undefined)}
        />
      )}
      {issuing && (
        <TokenDialog
          service={issuing}
          onClose={() => {
            setIssuing(undefined);
            void load();
          }}
        />
      )}
    </>
  );
}

function ProviderDialog({
  isOpen,
  editing,
  secrets,
  localProvider,
  onClose,
  onDone,
}: {
  isOpen: boolean;
  editing?: Provider;
  secrets: Schemas["LocalSecret"][];
  localProvider?: string;
  onClose: () => void;
  onDone: (p: Provider) => void;
}) {
  const l = editing?.ldap;
  const [kind, setKind] = useState<string>(editing?.kind ?? "oidc");
  const [name, setName] = useState(editing?.name ?? "");
  const [display, setDisplay] = useState(editing?.displayName ?? "");
  const [secret, setSecret] = useState<string | null>(null);
  const [issuer, setIssuer] = useState(editing?.oidc?.issuer ?? "");
  const [clientId, setClientId] = useState(editing?.oidc?.clientId ?? "");
  const [url, setUrl] = useState(l?.url ?? "");
  const [startTls, setStartTls] = useState(l?.startTls ?? false);
  const [bindDn, setBindDn] = useState(l?.bindDn ?? "");
  const [userBaseDn, setUserBaseDn] = useState(l?.userBaseDn ?? "");
  const [userFilter, setUserFilter] = useState(
    l?.userFilter ?? "(&(objectClass=person)(uid={username}))",
  );
  const [groupBaseDn, setGroupBaseDn] = useState(l?.groupBaseDn ?? "");
  const [groupFilter, setGroupFilter] = useState(l?.groupFilter ?? "");
  const [directory, setDirectory] = useState(
    l?.userFilter.includes("sAMAccountName") ? "ad" : "openldap",
  );
  const [usernameAttr, setUsernameAttr] = useState(
    l ? (l.usernameAttribute ?? "") : "uid",
  );
  const [displayAttr, setDisplayAttr] = useState(
    l ? (l.displayNameAttribute ?? "") : "cn",
  );
  const [caCert, setCaCert] = useState(l?.caCertPem ?? "");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  // Active Directory names users by sAMAccountName and lists nested groups through the in-chain matching rule.
  function chooseDirectory(d: string) {
    setDirectory(d);
    if (d === "ad") {
      setUserFilter("(&(objectClass=user)(sAMAccountName={username}))");
      setUsernameAttr("sAMAccountName");
      setDisplayAttr("displayName");
      setGroupFilter("(member:1.2.840.113556.1.4.1941:={dn})");
    } else {
      setUserFilter("(&(objectClass=person)(uid={username}))");
      setUsernameAttr("uid");
      setDisplayAttr("cn");
      setGroupFilter("");
    }
  }
  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    if (!slug(name)) {
      setError("Give the provider a name with at least one letter or digit.");
      return;
    }
    if (
      kind === "ldap" &&
      caCert.trim() &&
      !caCert.includes("-----BEGIN CERTIFICATE-----")
    ) {
      setError(
        "The CA certificate box holds no -----BEGIN CERTIFICATE----- block.",
      );
      return;
    }
    let secretId = secret ?? undefined;
    // A password typed here is stored as a local secret first; the provider then reserves it for the controller.
    if (password) {
      if (!localProvider) {
        setError("The local secret store is not available.");
        return;
      }
      const { data: stored, error: storeError } = await api.POST(
        "/secret-providers/{providerId}/local-secrets",
        {
          params: {
            path: { providerId: localProvider },
            header: { "Idempotency-Key": crypto.randomUUID() },
          },
          body: {
            // Secret names are at most 63 characters.
            // A replacement gets a fresh suffix, since a stored secret's value cannot be changed.
            name: `identity-${slug(name).slice(0, 30)}-${kind === "oidc" ? "client-secret" : "bind-password"}${editing ? `-${crypto.randomUUID().slice(0, 4)}` : ""}`,
            type: "text",
            value: password,
          },
        },
      );
      if (storeError || !stored) {
        setError(describeError(storeError));
        return;
      }
      secretId = stored.id;
    }
    const body: Schemas["IdentityProviderCreate"] = {
      kind: kind as "oidc" | "ldap",
      name: slug(name),
      displayName: display || name.trim() || undefined,
      secretId,
    };
    if (kind === "oidc") body.oidc = { issuer, clientId };
    else
      body.ldap = {
        url,
        startTls,
        bindDn,
        userBaseDn,
        userFilter,
        groupBaseDn: groupBaseDn || undefined,
        groupFilter: groupFilter || undefined,
        usernameAttribute: usernameAttr || undefined,
        displayNameAttribute: displayAttr || undefined,
        // Only the certificate blocks: pasted openssl output or comments around them are dropped.
        caCertPem:
          caCert
            .match(
              /-----BEGIN CERTIFICATE-----[\s\S]+?-----END CERTIFICATE-----/g,
            )
            ?.join("\n") || undefined,
      };
    const { data, error } = editing
      ? await api.PUT("/identity-providers/{providerId}", {
          params: { path: { providerId: editing.id } },
          body: {
            displayName: body.displayName,
            secretId: body.secretId,
            oidc: body.oidc,
            ldap: body.ldap,
          },
        })
      : await api.POST("/identity-providers", { body });
    if (error || !data) setError(describeError(error));
    else onDone(data);
  }
  return (
    <Dialog
      title={editing ? `Edit ${editing.displayName}` : "Add identity provider"}
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <Select
          label="Kind"
          options={[
            { id: "oidc", label: "OpenID Connect" },
            { id: "ldap", label: "LDAP or Active Directory" },
          ]}
          value={kind}
          onChange={setKind}
          isDisabled={!!editing}
        />
        <TextField
          label="Name"
          value={name}
          onChange={setName}
          isRequired
          isDisabled={!!editing}
          description={
            editing
              ? "The name and kind cannot change."
              : slug(name)
                ? `Saved as ${slug(name)}.`
                : "Such as Company AD."
          }
        />
        <TextField
          label="Label on the sign-in page"
          value={display}
          onChange={setDisplay}
        />
        {kind === "oidc" ? (
          <>
            <TextField
              label="Issuer URL"
              value={issuer}
              onChange={setIssuer}
              isRequired
            />
            <TextField
              label="Client ID"
              value={clientId}
              onChange={setClientId}
              isRequired
            />
          </>
        ) : (
          <>
            <Select
              label="Directory"
              options={[
                { id: "openldap", label: "OpenLDAP" },
                { id: "ad", label: "Active Directory" },
              ]}
              value={directory}
              onChange={chooseDirectory}
              description="Fills in the filters and attributes below; change any of them."
            />
            <TextField
              label="Server URLs"
              description="ldaps://host:636, or ldap://host with StartTLS. List several domain controllers separated by spaces; they are tried in order."
              value={url}
              onChange={setUrl}
              isRequired
            />
            <Checkbox isSelected={startTls} onChange={setStartTls}>
              Use StartTLS
            </Checkbox>
            <TextField
              label="Bind DN"
              value={bindDn}
              onChange={setBindDn}
              isRequired
            />
            <TextField
              label="User base DN"
              value={userBaseDn}
              onChange={setUserBaseDn}
              isRequired
            />
            <TextField
              label="User filter"
              description="{username} is replaced by the escaped username."
              value={userFilter}
              onChange={setUserFilter}
              isRequired
            />
            <TextField
              label="Group base DN"
              value={groupBaseDn}
              onChange={setGroupBaseDn}
            />
            <TextField
              label="Group filter"
              description="Such as (member={dn}); {dn} is escaped."
              value={groupFilter}
              onChange={setGroupFilter}
            />
            <TextField
              label="Username attribute"
              value={usernameAttr}
              onChange={setUsernameAttr}
            />
            <TextField
              label="Display name attribute"
              value={displayAttr}
              onChange={setDisplayAttr}
            />
            <TextField
              label="CA certificate (PEM)"
              description="The CA that signed the directory's TLS certificate, when the hosts do not already trust it."
              value={caCert}
              onChange={setCaCert}
              multiline
            />
          </>
        )}
        <TextField
          label={kind === "oidc" ? "Client secret" : "Bind password"}
          type="password"
          value={password}
          onChange={setPassword}
          isRequired={kind === "ldap" && !secret && !editing?.hasSecret}
          description={
            editing?.hasSecret
              ? "Leave empty to keep the stored one."
              : "Stored encrypted and reserved for the controller; it is never shown again."
          }
        />
        {secrets.length > 0 && (
          <Select
            label="Or use a stored secret"
            options={secrets.map((s) => ({ id: s.id, label: s.name }))}
            value={secret}
            onChange={setSecret}
            description="Leave the password empty to use one of these."
          />
        )}
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">{editing ? "Save" : "Add"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function MappingDialog({
  provider,
  onClose,
}: {
  provider: Provider;
  onClose: () => void;
}) {
  const me = useMe();
  const { teams } = useTeams();
  const [items, setItems] = useState<Schemas["GroupMapping"][]>();
  const [group, setGroup] = useState("");
  const [role, setRole] = useState<string | null>("developer");
  const [scope, setScope] = useState<string | null>("organization");
  const [error, setError] = useState<string>();
  const path = { params: { path: { providerId: provider.id } } };
  const load = useCallback(async () => {
    const { data } = await api.GET(
      "/identity-providers/{providerId}/group-mappings",
      { params: { path: { providerId: provider.id } } },
    );
    setItems(data?.items ?? []);
  }, [provider.id]);
  useLoader(load);
  const scopes = [
    { id: "organization", label: "Whole organization" },
    ...(teams ?? []).map((t) => ({ id: t.id, label: `Team ${t.name}` })),
  ];
  const scopeName = (m: Schemas["GroupMapping"]) =>
    m.scopeType === "organization"
      ? "organization"
      : `team ${teams?.find((t) => t.id === m.scopeId)?.name ?? m.scopeId}`;
  async function add(e: FormEvent) {
    e.preventDefault();
    if (!role || !scope) return;
    const org = scope === "organization";
    const { error } = await api.POST(
      "/identity-providers/{providerId}/group-mappings",
      {
        ...path,
        body: {
          group,
          role: role as Schemas["GroupMappingCreate"]["role"],
          scopeType: org ? "organization" : "team",
          scopeId: org ? (orgId(me) ?? "") : scope,
        },
      },
    );
    setError(error ? describeError(error) : undefined);
    if (!error) setGroup("");
    await load();
  }
  async function remove(id: string) {
    await api.DELETE(
      "/identity-providers/{providerId}/group-mappings/{mappingId}",
      { params: { path: { providerId: provider.id, mappingId: id } } },
    );
    await load();
  }
  return (
    <Dialog
      title={`Group mappings for ${provider.displayName}`}
      size="wide"
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <p className="samuori-muted">
        Roles from mappings are replaced at every sign-in, so removing someone
        from a group removes the role at their next sign-in.
      </p>
      {!items ? (
        <Loading />
      ) : (
        <Table
          label="Group mappings"
          rows={items}
          empty="No mappings: people from this provider get no roles."
          columns={[
            {
              id: "group",
              name: "Group",
              isRowHeader: true,
              render: (m) => m.group,
            },
            {
              id: "role",
              name: "Role",
              render: (m) => `${roleLabel(m.role as Role)} on ${scopeName(m)}`,
            },
            {
              id: "remove",
              name: "Actions",
              render: (m) => (
                <Button variant="quiet" onPress={() => void remove(m.id)}>
                  Remove {m.group}
                </Button>
              ),
            },
          ]}
        />
      )}
      <Form onSubmit={add} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          label="Group"
          value={group}
          onChange={setGroup}
          isRequired
          description={
            provider.kind === "ldap"
              ? "The group's common name (cn), such as Samuori-Admins, or its full DN. Capitals do not matter."
              : "The value in the groups claim, exactly as the provider sends it."
          }
        />
        <Select
          label="Role"
          options={roleOptions}
          value={role}
          onChange={setRole}
        />
        <Select
          label="Scope"
          options={scopes}
          value={scope}
          onChange={setScope}
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Close
          </Button>
          <Button type="submit">Add mapping</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function ServiceDialog({
  isOpen,
  onClose,
  onDone,
}: {
  isOpen: boolean;
  onClose: () => void;
  onDone: (s: ServiceIdentity) => void;
}) {
  const [username, setUsername] = useState("");
  const [display, setDisplay] = useState("");
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!slug(username)) {
      setError("Give the identity a name with at least one letter or digit.");
      return;
    }
    const { data, error } = await api.POST("/service-identities", {
      body: {
        username: slug(username),
        displayName: display || username.trim() || undefined,
      },
    });
    if (error || !data) setError(describeError(error));
    else onDone(data);
  }
  return (
    <Dialog
      title="Add service identity"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField
          label="Name"
          description={
            slug(username)
              ? `Saved as ${slug(username)}.`
              : "Such as CI pipeline."
          }
          value={username}
          onChange={setUsername}
          isRequired
        />
        <TextField label="Display name" value={display} onChange={setDisplay} />
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

function TokenDialog({
  service,
  onClose,
}: {
  service: ServiceIdentity;
  onClose: () => void;
}) {
  const [name, setName] = useState("");
  const [days, setDays] = useState("90");
  const [write, setWrite] = useState(false);
  const [token, setToken] = useState<string>();
  const [error, setError] = useState<string>();
  async function submit(e: FormEvent) {
    e.preventDefault();
    const expiresAt = new Date(
      Date.now() + (Number(days) || 0) * 86_400_000,
    ).toISOString();
    const { data, error } = await api.POST(
      "/service-identities/{identityId}/api-tokens",
      {
        params: { path: { identityId: service.id } },
        body: { name, expiresAt, scopes: write ? ["read", "write"] : ["read"] },
      },
    );
    if (error || !data) setError(describeError(error));
    else setToken(data.token);
  }
  return (
    <Dialog
      title={`Issue a token for ${service.username}`}
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      {token ? (
        <>
          <Alert tone="warning">
            Copy this token now. It is shown only once.
          </Alert>
          <code className="samuori-secret">{token}</code>
          <div className="samuori-actions">
            <Button onPress={onClose}>Done</Button>
          </div>
        </>
      ) : (
        <Form onSubmit={submit} className="samuori-form">
          {error && <Alert>{error}</Alert>}
          <TextField
            label="Token name"
            value={name}
            onChange={setName}
            isRequired
          />
          <TextField
            label="Valid for (days)"
            description="At most 365."
            inputMode="numeric"
            value={days}
            onChange={setDays}
            isRequired
          />
          <Checkbox isSelected={write} onChange={setWrite}>
            Allow changes (otherwise read-only)
          </Checkbox>
          <div className="samuori-actions">
            <Button variant="secondary" onPress={onClose}>
              Cancel
            </Button>
            <Button type="submit">Issue token</Button>
          </div>
        </Form>
      )}
    </Dialog>
  );
}

function SessionSettings({
  current,
  onSaved,
}: {
  current: Schemas["SessionTimeouts"];
  onSaved: (
    data: Schemas["IdentitySettings"] | undefined,
    error: unknown,
  ) => void;
}) {
  const [idle, setIdle] = useState(String(current.idleTimeoutMinutes));
  const [max, setMax] = useState(String(current.maxSessionHours));
  const [warning, setWarning] = useState(String(current.warningSeconds));
  const [busy, setBusy] = useState(false);

  async function save(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    const { data, error } = await api.PUT("/identity-settings", {
      body: {
        session: {
          idleTimeoutMinutes: Number(idle),
          maxSessionHours: Number(max),
          warningSeconds: Number(warning),
        },
      },
    });
    setBusy(false);
    onSaved(data, error);
  }

  return (
    <section aria-labelledby="sessions-heading">
      <h2 id="sessions-heading">Sessions</h2>
      <p className="samuori-muted">
        Browser sessions end after a period without activity, and always at the
        maximum length. Activity renews only the inactivity limit. Changes apply
        to existing sessions at once.
      </p>
      <Form onSubmit={save} className="samuori-form">
        <TextField
          label="Sign out after inactivity (minutes)"
          type="number"
          inputMode="numeric"
          value={idle}
          onChange={setIdle}
          isRequired
          description="15 to 120 minutes, and not longer than the maximum session."
        />
        <TextField
          label="Maximum session (hours)"
          type="number"
          inputMode="numeric"
          value={max}
          onChange={setMax}
          isRequired
          description="1 to 24 hours. People sign in again after this, whatever they are doing."
        />
        <TextField
          label="Warn before signing out (seconds)"
          type="number"
          inputMode="numeric"
          value={warning}
          onChange={setWarning}
          isRequired
          description="30 to 600 seconds. The warning offers Stay signed in."
        />
        <Button type="submit" isDisabled={busy}>
          {busy ? "Saving…" : "Save session settings"}
        </Button>
      </Form>
    </section>
  );
}
