import { useCallback, useState, type FormEvent } from "react";
import { Form, Tab, TabList, TabPanel, Tabs } from "react-aria-components";
import { api, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { TextField } from "../components/Field";
import { Alert, Loading } from "../components/Feedback";
import { ScanSearch, Send, ShieldCheck } from "lucide-react";
import { BrandMark } from "../components/BrandMark";
import { Spinner } from "../components/Spinner";
import { useLoader } from "../hooks";
import { broadcast, rememberReturn } from "../session/broadcast";
import { useSession } from "../session/session";

type SignInProvider = Schemas["SignInProviderList"]["items"][number];

const query = new URLSearchParams(
  typeof window !== "undefined" ? window.location.search : "",
);
const ssoFailed = query.get("signin") === "failed";
// While a directory or SSO provider is enabled, local accounts sit behind a recovery link; /?local opens them
// directly (docs/architecture/identity.md#recovery-access).
const forceLocal = query.has("local");

export function LoginPage({ notice }: { notice?: string }) {
  const { refresh } = useSession();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | undefined>(
    ssoFailed
      ? "Sign-in with the identity provider failed. Try again, or contact an administrator."
      : undefined,
  );
  const [busy, setBusy] = useState(false);
  const [providers, setProviders] = useState<SignInProvider[]>();
  const [chosen, setChosen] = useState<string>();
  const [showLocal, setShowLocal] = useState(forceLocal);
  const load = useCallback(async () => {
    const { data } = await api.GET("/auth/providers");
    setProviders(data?.items ?? []);
  }, []);
  useLoader(load);
  const directories = (providers ?? []).filter((p) => p.kind === "ldap");
  const sso = (providers ?? []).filter((p) => p.kind === "oidc");
  const tabs = [
    ...directories.map((p) => ({ id: p.id, label: p.name })),
    ...(showLocal || (providers && providers.length === 0)
      ? [{ id: "local", label: "Local account" }]
      : []),
  ];
  const account = tabs.some((t) => t.id === chosen) ? chosen : tabs[0]?.id;
  const accountLabel = tabs.find((t) => t.id === account)?.label;

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    const { response } = await api.POST("/auth/login", {
      body: {
        username,
        password,
        providerId: account && account !== "local" ? account : undefined,
      },
    });
    setBusy(false);
    if (response.status === 204) {
      setPassword("");
      await refresh();
      broadcast({ type: "signed-in" });
      return;
    }
    if (response.status === 429) {
      const after = Number(response.headers.get("Retry-After") ?? "60");
      setError(
        `Too many sign-in attempts. Try again in ${Math.max(1, Math.ceil(after / 60))} minute(s).`,
      );
    } else if (response.status === 401) {
      setError("The username or password is incorrect.");
    } else {
      setError("Sign-in failed. Try again, or contact an administrator.");
    }
  }

  const form = (
    <Form onSubmit={submit} className="samuori-form">
      <TextField
        label="Username"
        value={username}
        onChange={setUsername}
        isRequired
        autoComplete="username"
        autoFocus
      />
      <TextField
        label="Password"
        type="password"
        value={password}
        onChange={setPassword}
        isRequired
        autoComplete="current-password"
      />
      <Button type="submit" isDisabled={busy}>
        {busy && <Spinner decorative />}
        {busy ? "Signing in…" : "Sign in"}
      </Button>
    </Form>
  );

  return (
    <main className="samuori-auth-page samuori-auth-page--split" id="main">
      <div className="samuori-auth-split">
        <section className="samuori-auth-brand" aria-label="About Samuori">
          <div className="samuori-auth-brand-head">
            <BrandMark
              kind="stacked"
              finish="3d"
              className="samuori-auth-brand-logo w-[280px]"
            />
            <p className="samuori-motto">Woven into one</p>
          </div>
          <p className="samuori-auth-brand-lead">
            Upload a project and get a tested, governed container image, ready
            for your registries.
          </p>
          <ul className="samuori-auth-points">
            <li>
              <ShieldCheck size={18} aria-hidden="true" />
              Builds run isolated; each secret reaches only the step that needs
              it.
            </li>
            <li>
              <ScanSearch size={18} aria-hidden="true" />
              Every image is run, inventoried and checked for leaked secrets.
            </li>
            <li>
              <Send size={18} aria-hidden="true" />
              Images go only to registries your administrators approve.
            </li>
          </ul>
        </section>
        <div className="samuori-auth-card">
          <div className="samuori-auth-card-brand">
            <BrandMark kind="stacked" finish="3d" className="w-[240px]" />
            <p className="samuori-motto">Woven into one</p>
          </div>
          <h1>Sign in</h1>
          {notice && <Alert tone="info">{notice}</Alert>}
          {error && <Alert>{error}</Alert>}
          {!providers && <Loading />}
          {sso.map((p) => (
            <a
              key={p.id}
              href={`/api/v1/auth/oidc/${p.id}/start`}
              onClick={rememberReturn}
              className="samuori-button samuori-button--secondary"
            >
              Sign in with {p.name}
            </a>
          ))}
          {tabs.length > 1 ? (
            <Tabs
              selectedKey={account}
              onSelectionChange={(k) => setChosen(String(k))}
            >
              <TabList aria-label="Account" className="samuori-tablist">
                {tabs.map((t) => (
                  <Tab key={t.id} id={t.id} className="samuori-tab">
                    {t.label}
                  </Tab>
                ))}
              </TabList>
              {tabs.map((t) => (
                <TabPanel key={t.id} id={t.id}>
                  {form}
                </TabPanel>
              ))}
            </Tabs>
          ) : (
            account && (
              <>
                <p className="samuori-muted">
                  {account === "local"
                    ? "Sign in with a local account."
                    : `Sign in with your ${accountLabel} account.`}
                </p>
                {form}
              </>
            )
          )}
          {providers && providers.length > 0 && !showLocal && (
            <Button
              variant="quiet"
              onPress={() => {
                setShowLocal(true);
                setChosen("local");
              }}
            >
              Local administrator sign-in
            </Button>
          )}
        </div>
      </div>
    </main>
  );
}
