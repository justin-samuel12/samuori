import { useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError } from "../api/client";
import { Button } from "../components/Button";
import { TextField } from "../components/Field";
import { Alert } from "../components/Feedback";
import { useSession } from "../session/session";
import { correctName } from "../format";

export function SetupPage() {
  const { refresh } = useSession();
  const [form, setForm] = useState({
    token: "",
    organizationName: "",
    username: "",
    displayName: "",
    password: "",
    confirm: "",
  });
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof form) => (v: string) =>
    setForm((f) => ({ ...f, [k]: v }));

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    if (form.password !== form.confirm) {
      setError("The passwords do not match.");
      return;
    }
    setBusy(true);
    const { error } = await api.POST("/setup/bootstrap", {
      body: {
        token: form.token.trim(),
        organizationName: form.organizationName,
        username: correctName(form.username, { extra: "._", done: true }),
        displayName: form.displayName || undefined,
        password: form.password,
      },
    });
    setBusy(false);
    if (error) {
      setError(describeError(error));
      return;
    }
    await refresh(
      "Setup is complete. Sign in with the administrator account you just created.",
    );
  }

  return (
    <main className="samuori-auth-page" id="main">
      <div className="samuori-auth-card">
        <h1>Set up Samuori</h1>
        <p className="samuori-muted">
          Create your organization and the first administrator. The one-time
          setup code is in the file <code>bootstrap-token</code> inside the
          server's data directory; the server log shows its full path.
        </p>
        {error && <Alert>{error}</Alert>}
        <Form onSubmit={submit} className="samuori-form">
          <TextField
            label="Setup code"
            value={form.token}
            onChange={set("token")}
            isRequired
            autoComplete="off"
          />
          <TextField
            label="Organization name"
            value={form.organizationName}
            onChange={set("organizationName")}
            isRequired
            maxLength={128}
          />
          <TextField
            label="Administrator username"
            value={form.username}
            onChange={(v) => set("username")(correctName(v, { extra: "._" }))}
            isRequired
            description="Saved in lowercase; dots, dashes and underscores are kept."
            autoComplete="username"
          />
          <TextField
            label="Display name"
            value={form.displayName}
            onChange={set("displayName")}
            maxLength={128}
          />
          <TextField
            label="Password"
            type="password"
            value={form.password}
            onChange={set("password")}
            isRequired
            description="At least 12 characters, not containing the username."
            autoComplete="new-password"
          />
          <TextField
            label="Confirm password"
            type="password"
            value={form.confirm}
            onChange={set("confirm")}
            isRequired
            autoComplete="new-password"
          />
          <Button type="submit" isDisabled={busy}>
            {busy ? "Creating…" : "Create organization"}
          </Button>
        </Form>
      </div>
    </main>
  );
}
