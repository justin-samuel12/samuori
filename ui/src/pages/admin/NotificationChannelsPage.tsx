import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Dialog } from "../../components/Dialog";
import { Checkbox, Select, TextField } from "../../components/Field";
import { Alert, Loading, PageHeader } from "../../components/Feedback";
import { Table } from "../../components/Table";
import { useLoader } from "../../hooks";
import { teamName, useTeams } from "../useTeams";

type Channel = Schemas["NotificationChannel"];
type Rule = Schemas["NotificationRule"];
type Delivery = Schemas["NotificationDelivery"];
type EventType = Schemas["NotificationRuleCreate"]["events"][number];
type Kind = Schemas["NotificationChannelCreate"]["kind"];

export const eventLabels: Record<EventType, string> = {
  "build.succeeded": "Build succeeded",
  "build.failed": "Build failed",
  "validation.passed": "Checks passed",
  "validation.failed": "Checks failed",
  "validation.blocked": "Checks blocked",
  "publication.published": "Image published",
  "publication.failed": "Publication failed",
  "publication.blocked": "Publication blocked",
  "approval.requested": "Approval requested",
  "approval.decided": "Approval decided",
  "exception.requested": "Output exception requested",
  "worker.revoked": "Worker revoked",
  "artifact.quarantined": "Image quarantined",
};

const kindLabels: Record<Kind, string> = {
  webhook: "Signed webhook",
  slack: "Slack",
  teams: "Microsoft Teams",
  email: "Email",
};

const stateLabels: Record<string, string> = {
  pending: "Pending",
  delivered: "Delivered",
  dead: "Failed (dead letter)",
};

export function NotificationChannelsPage() {
  const { teams } = useTeams();
  const [channels, setChannels] = useState<Channel[]>();
  const [rules, setRules] = useState<Rule[]>([]);
  const [deliveries, setDeliveries] = useState<Delivery[]>([]);
  const [filter, setFilter] = useState("");
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [creating, setCreating] = useState<"channel" | "rule">();
  const [editingChannel, setEditingChannel] = useState<Channel>();
  const [editingRule, setEditingRule] = useState<Rule>();
  const [signingKey, setSigningKey] = useState<string>();

  const load = useCallback(async () => {
    const [c, r, d] = await Promise.all([
      api.GET("/notification-channels"),
      api.GET("/notification-rules"),
      api.GET("/notification-deliveries", {
        params: {
          query: {
            state: (filter || undefined) as Delivery["state"] | undefined,
            limit: 100,
          },
        },
      }),
    ]);
    if (c.data) setChannels(c.data.items);
    else setError(describeError(c.error));
    if (r.data) setRules(r.data.items);
    if (d.data) setDeliveries(d.data.items);
  }, [filter]);
  useLoader(load);

  const channelName = (id: string) =>
    channels?.find((c) => c.id === id)?.name ?? "Deleted channel";

  async function setEnabled(c: Channel, enabled: boolean) {
    const { error } = await api.POST(
      "/notification-channels/{channelId}/state",
      { params: { path: { channelId: c.id } }, body: { enabled } },
    );
    if (error) setError(describeError(error));
    else await load();
  }

  async function test(c: Channel) {
    setNotice(undefined);
    setError(undefined);
    const { data, error } = await api.POST(
      "/notification-channels/{channelId}/test",
      { params: { path: { channelId: c.id } } },
    );
    if (error) setError(describeError(error));
    else if (data.status === "ok") setNotice(`${c.name} accepted the test.`);
    else setError(`${c.name}: ${data.detail ?? "the test failed"}`);
  }

  async function removeRule(id: string) {
    const { error } = await api.DELETE("/notification-rules/{ruleId}", {
      params: { path: { ruleId: id } },
    });
    if (error) setError(describeError(error));
    else await load();
  }

  async function retry(id: string) {
    const { error } = await api.POST(
      "/notification-deliveries/{deliveryId}/retry",
      { params: { path: { deliveryId: id } } },
    );
    if (error) setError(describeError(error));
    else await load();
  }

  if (!channels && !error) return <Loading />;
  return (
    <>
      <PageHeader
        title="Notifications"
        description="Channels that receive events, rules that choose which events go where, and the delivery log. Messages carry titles, project names and links only, never logs or failure details."
        actions={
          <>
            <Button onPress={() => setCreating("channel")}>Add channel</Button>
            <Button
              variant="secondary"
              onPress={() => setCreating("rule")}
              isDisabled={!channels?.length}
            >
              Add rule
            </Button>
          </>
        }
      />
      {error && <Alert>{error}</Alert>}
      {notice && <Alert tone="success">{notice}</Alert>}
      {signingKey && (
        <Alert tone="warning">
          <p>
            Signing key for the new webhook. It is shown only once; configure
            the receiver to verify the <code>X-Samuori-Signature</code> header
            with it.
          </p>
          <code className="samuori-secret">{signingKey}</code>
          <div className="samuori-actions">
            <Button
              variant="secondary"
              onPress={() => setSigningKey(undefined)}
            >
              I have stored it
            </Button>
          </div>
        </Alert>
      )}
      <section className="samuori-section" aria-labelledby="channels-h">
        <h2 id="channels-h">Channels</h2>
        <Table
          label="Channels"
          rows={channels ?? []}
          empty="No channels. Users still see their notifications in the app."
          columns={[
            {
              id: "name",
              name: "Name",
              isRowHeader: true,
              render: (c) => c.name,
            },
            {
              id: "kind",
              name: "Kind",
              render: (c) => kindLabels[c.kind as Kind] ?? c.kind,
            },
            {
              id: "target",
              name: "Target",
              render: (c) =>
                c.url ?? c.recipients?.join(", ") ?? "Stored as a secret",
            },
            {
              id: "state",
              name: "State",
              render: (c) => (c.enabled ? "Enabled" : "Disabled"),
            },
            {
              id: "actions",
              name: "Actions",
              render: (c) => (
                <div className="samuori-actions">
                  <Button
                    variant="quiet"
                    onPress={() => setEditingChannel(c)}
                    aria-label={`Edit ${c.name}`}
                  >
                    Edit
                  </Button>
                  <Button variant="quiet" onPress={() => void test(c)}>
                    Send test
                  </Button>
                  <Button
                    variant="quiet"
                    onPress={() => void setEnabled(c, !c.enabled)}
                  >
                    {c.enabled ? "Disable" : "Enable"}
                  </Button>
                </div>
              ),
            },
          ]}
        />
      </section>
      <section className="samuori-section" aria-labelledby="rules-h">
        <h2 id="rules-h">Rules</h2>
        <Table
          label="Rules"
          rows={rules}
          dependencies={[channels, teams]}
          empty="No rules. Channels receive nothing until a rule routes events to them."
          columns={[
            {
              id: "channel",
              name: "Channel",
              isRowHeader: true,
              render: (r) => channelName(r.channelId),
            },
            {
              id: "events",
              name: "Events",
              render: (r) =>
                r.events
                  .map((e) => eventLabels[e as EventType] ?? e)
                  .join(", "),
            },
            {
              id: "scope",
              name: "Scope",
              render: (r) =>
                r.projectId
                  ? "One project"
                  : r.teamId
                    ? `Team ${teamName(teams, r.teamId)}`
                    : "Whole organization",
            },
            {
              id: "actions",
              name: "Actions",
              render: (r) => (
                <div className="samuori-actions">
                  <Button
                    variant="quiet"
                    onPress={() => setEditingRule(r)}
                    aria-label={`Edit rule for ${channelName(r.channelId)}`}
                  >
                    Edit
                  </Button>
                  <Button variant="quiet" onPress={() => void removeRule(r.id)}>
                    Delete
                  </Button>
                </div>
              ),
            },
          ]}
        />
      </section>
      <section className="samuori-section" aria-labelledby="deliveries-h">
        <h2 id="deliveries-h">Deliveries</h2>
        <div className="samuori-toolbar">
          <Select
            label="Show"
            value={filter || "all"}
            onChange={(v) => setFilter(v === "all" ? "" : v)}
            options={[
              { id: "all", label: "All deliveries" },
              { id: "pending", label: "Pending" },
              { id: "delivered", label: "Delivered" },
              { id: "dead", label: "Failed (dead letter)" },
            ]}
          />
        </div>
        <Table
          label="Deliveries"
          rows={deliveries}
          dependencies={[channels]}
          empty="No deliveries."
          columns={[
            {
              id: "event",
              name: "Event",
              isRowHeader: true,
              render: (d) =>
                eventLabels[d.eventType as EventType] ?? d.eventType,
            },
            {
              id: "channel",
              name: "Channel",
              render: (d) => channelName(d.channelId),
            },
            {
              id: "state",
              name: "State",
              render: (d) => stateLabels[d.state] ?? d.state,
            },
            {
              id: "attempts",
              name: "Attempts",
              align: "end",
              render: (d) => String(d.attempts),
            },
            {
              id: "detail",
              name: "Detail",
              render: (d) =>
                d.lastError ??
                (d.nextAttemptAt
                  ? `Next attempt ${new Date(d.nextAttemptAt).toLocaleString()}`
                  : d.deliveredAt
                    ? new Date(d.deliveredAt).toLocaleString()
                    : ""),
            },
            {
              id: "actions",
              name: "Actions",
              render: (d) =>
                d.state === "dead" ? (
                  <Button variant="quiet" onPress={() => void retry(d.id)}>
                    Retry
                  </Button>
                ) : null,
            },
          ]}
        />
      </section>
      <ChannelDialog
        key={editingChannel?.id ?? "new-channel"}
        isOpen={creating === "channel" || editingChannel !== undefined}
        editing={editingChannel}
        onClose={() => {
          setCreating(undefined);
          setEditingChannel(undefined);
        }}
        onDone={(key) => {
          setCreating(undefined);
          setEditingChannel(undefined);
          setSigningKey(key);
          void load();
        }}
      />
      <RuleDialog
        key={editingRule?.id ?? "new-rule"}
        isOpen={creating === "rule" || editingRule !== undefined}
        editing={editingRule}
        channels={channels ?? []}
        teams={teams ?? []}
        onClose={() => {
          setCreating(undefined);
          setEditingRule(undefined);
        }}
        onDone={() => {
          setCreating(undefined);
          setEditingRule(undefined);
          void load();
        }}
      />
    </>
  );
}

function ChannelDialog({
  isOpen,
  editing,
  onClose,
  onDone,
}: {
  isOpen: boolean;
  editing?: Channel;
  onClose: () => void;
  onDone: (signingKey?: string) => void;
}) {
  const [name, setName] = useState(editing?.name ?? "");
  const [kind, setKind] = useState<Kind>((editing?.kind as Kind) ?? "webhook");
  const [url, setUrl] = useState(editing?.url ?? "");
  const [secret, setSecret] = useState("");
  const [recipients, setRecipients] = useState(
    editing?.recipients?.join(", ") ?? "",
  );
  const [allowPrivate, setAllowPrivate] = useState(
    editing?.allowPrivateNetwork ?? false,
  );
  const [error, setError] = useState<string>();
  const recipientList = () =>
    recipients
      .split(/[,\s]+/)
      .map((r) => r.trim())
      .filter(Boolean);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    if (editing) {
      const { error } = await api.PUT("/notification-channels/{channelId}", {
        params: { path: { channelId: editing.id } },
        body: {
          name,
          url: kind === "webhook" ? url : undefined,
          secret:
            (kind === "slack" || kind === "teams") && secret
              ? secret
              : undefined,
          recipients: kind === "email" ? recipientList() : undefined,
          allowPrivateNetwork: kind === "email" ? undefined : allowPrivate,
        },
      });
      if (error) setError(describeError(error));
      else onDone();
      return;
    }
    const { data, error } = await api.POST("/notification-channels", {
      body: {
        name,
        kind,
        url: kind === "webhook" ? url : undefined,
        secret: kind === "slack" || kind === "teams" ? secret : undefined,
        recipients: kind === "email" ? recipientList() : undefined,
        allowPrivateNetwork: allowPrivate,
      },
    });
    if (data) {
      setName("");
      setUrl("");
      setSecret("");
      setRecipients("");
      onDone(data.signingKey);
    } else setError(describeError(error));
  }

  return (
    <Dialog
      title={editing ? `Edit ${editing.name}` : "Add a channel"}
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <TextField label="Name" value={name} onChange={setName} isRequired />
        <Select
          label="Kind"
          value={kind}
          isDisabled={editing !== undefined}
          description={editing ? "A channel's kind cannot change." : undefined}
          onChange={(v) => setKind(v as Kind)}
          options={Object.entries(kindLabels).map(([id, label]) => ({
            id,
            label,
          }))}
        />
        {kind === "webhook" && (
          <TextField
            label="Endpoint URL"
            description="HTTPS only. Each request is signed with a key shown once after you add the channel."
            value={url}
            onChange={setUrl}
            type="url"
            isRequired
          />
        )}
        {(kind === "slack" || kind === "teams") && (
          <TextField
            label={
              editing
                ? "New incoming webhook URL (optional)"
                : "Incoming webhook URL"
            }
            description={
              editing
                ? "Leave empty to keep the stored URL. A new one is stored as a secret and never shown again."
                : "Stored as a secret on the server and never shown again."
            }
            value={secret}
            onChange={setSecret}
            type="password"
            isRequired={!editing}
          />
        )}
        {kind === "email" && (
          <TextField
            label="Recipients"
            description="Addresses separated by commas. Requires SMTP to be configured on the server."
            value={recipients}
            onChange={setRecipients}
            isRequired
          />
        )}
        {kind !== "email" && (
          <Checkbox isSelected={allowPrivate} onChange={setAllowPrivate}>
            Allow private network addresses (internal receivers only)
          </Checkbox>
        )}
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">{editing ? "Save" : "Add channel"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}

function RuleDialog({
  isOpen,
  editing,
  channels,
  teams,
  onClose,
  onDone,
}: {
  isOpen: boolean;
  editing?: Rule;
  channels: Channel[];
  teams: Schemas["Team"][];
  onClose: () => void;
  onDone: () => void;
}) {
  const [channelId, setChannelId] = useState<string | null>(
    editing?.channelId ?? null,
  );
  const [events, setEvents] = useState<EventType[]>(
    (editing?.events as EventType[]) ?? [],
  );
  const [teamId, setTeamId] = useState(editing?.teamId ?? "");
  const [error, setError] = useState<string>();

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    if (!channelId || events.length === 0) {
      setError("Choose a channel and at least one event.");
      return;
    }
    const body = {
      channelId,
      events,
      teamId: teamId || undefined,
      projectId: editing?.projectId,
    };
    const { error } = editing
      ? await api.PUT("/notification-rules/{ruleId}", {
          params: { path: { ruleId: editing.id } },
          body,
        })
      : await api.POST("/notification-rules", { body });
    if (error) setError(describeError(error));
    else {
      setEvents([]);
      onDone();
    }
  }

  return (
    <Dialog
      title={editing ? "Edit rule" : "Add a rule"}
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <Select
          label="Channel"
          value={channelId}
          onChange={setChannelId}
          options={channels.map((c) => ({ id: c.id, label: c.name }))}
          isRequired
        />
        <Select
          label="Scope"
          value={teamId || "all"}
          onChange={(v) => setTeamId(v === "all" ? "" : v)}
          options={[
            { id: "all", label: "Whole organization" },
            ...teams.map((t) => ({ id: t.id, label: `Team ${t.name}` })),
          ]}
        />
        <fieldset className="samuori-fieldset">
          <legend>Events</legend>
          {(Object.keys(eventLabels) as EventType[]).map((ev) => (
            <Checkbox
              key={ev}
              isSelected={events.includes(ev)}
              onChange={(on) =>
                setEvents((cur) =>
                  on ? [...cur, ev] : cur.filter((x) => x !== ev),
                )
              }
            >
              {eventLabels[ev]}
            </Checkbox>
          ))}
        </fieldset>
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">{editing ? "Save" : "Add rule"}</Button>
        </div>
      </Form>
    </Dialog>
  );
}
