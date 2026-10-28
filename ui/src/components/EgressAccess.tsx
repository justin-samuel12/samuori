import { useCallback, useState, type FormEvent } from "react";
import { Form } from "react-aria-components";
import { api, describeError, type Schemas } from "../api/client";
import { useLoader } from "../hooks";
import { useCan } from "../session/session";
import { Button } from "./Button";
import { Dialog } from "./Dialog";
import { RadioGroup, TextField } from "./Field";
import { Alert, Badge } from "./Feedback";

type AccessRequest = Schemas["EgressAccessRequest"];
type EvidenceRef = Schemas["EgressEvidenceRef"];
export type BuildMethod = AccessRequest["method"];

/** Where an item's evidence lives; the server recomputes it from stored data (ADR 0052). */
export interface AccessContext {
  projectId: string;
  method?: BuildMethod;
  evidence: EvidenceRef;
  requests: AccessRequest[];
  onRequested: () => void;
}

export function useEgressRequests(projectId?: string) {
  const [items, setItems] = useState<AccessRequest[]>([]);
  const load = useCallback(async () => {
    if (!projectId) return;
    const { data } = await api.GET(
      "/projects/{projectId}/egress-access-requests",
      { params: { path: { projectId } } },
    );
    setItems(data?.items ?? []);
  }, [projectId]);
  useLoader(load);
  return { items, reload: load };
}

function describe(presets: string[], destinations: string[]): string {
  return [...presets.map((p) => `the ${p} preset`), ...destinations].join(", ");
}

type Network = "public" | "internal";

const stateLabel: Record<
  AccessRequest["state"],
  { text: string; tone: "info" | "ok" | "bad" | "neutral" }
> = {
  pending: { text: "Access requested", tone: "info" },
  approved: { text: "Allowed for this project", tone: "ok" },
  rejected: { text: "Request rejected", tone: "bad" },
  expired: { text: "Request expired", tone: "neutral" },
};

/** Request access for one item's presets and destinations, and the state of the latest matching request. */
export function EgressAccess({
  context,
  presets,
  destinations,
  internalOnly = [],
}: {
  context: AccessContext;
  presets: string[];
  destinations: string[];
  /** Destinations that can only be opened as internal: private addresses and non-public names. */
  internalOnly?: string[];
}) {
  const can = useCan();
  const [open, setOpen] = useState(false);
  if (presets.length + destinations.length === 0) return null;
  const latest = context.requests.find(
    (r) =>
      (!context.method || r.method === context.method) &&
      presets.every((p) => r.presets.includes(p)) &&
      destinations.every(
        (d) =>
          r.destinations.includes(d) ||
          (r.internalDestinations ?? []).includes(d),
      ),
  );
  const canRequest = can("requestEgressAccess");
  const again =
    !latest || latest.state === "rejected" || latest.state === "expired";
  return (
    <div className="samuori-actions" data-testid="egress-access">
      {latest && (
        <span>
          <Badge tone={stateLabel[latest.state].tone}>
            {stateLabel[latest.state].text}
          </Badge>
          {latest.state === "rejected" && latest.reason && (
            <span className="samuori-muted"> {latest.reason}</span>
          )}
          {latest.state === "approved" && (
            <span className="samuori-muted"> Run the build again.</span>
          )}
        </span>
      )}
      {canRequest && again && (
        <Button variant="secondary" onPress={() => setOpen(true)}>
          Request access
        </Button>
      )}
      <RequestAccessDialog
        context={context}
        presets={presets}
        destinations={destinations}
        internalOnly={internalOnly}
        isOpen={open}
        onClose={() => setOpen(false)}
        onDone={() => {
          setOpen(false);
          context.onRequested();
        }}
      />
    </div>
  );
}

function RequestAccessDialog({
  context,
  presets,
  destinations,
  internalOnly,
  isOpen,
  onClose,
  onDone,
}: {
  context: AccessContext;
  presets: string[];
  destinations: string[];
  internalOnly: string[];
  isOpen: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const [justification, setJustification] = useState("");
  const [error, setError] = useState<string>();
  const [chosen, setChosen] = useState<Record<string, Network>>({});
  const network = (d: string): Network =>
    internalOnly.includes(d) ? "internal" : (chosen[d] ?? "public");
  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    const { error } = await api.POST(
      "/projects/{projectId}/egress-access-requests",
      {
        params: { path: { projectId: context.projectId } },
        body: {
          method: context.method,
          presets,
          destinations: destinations.filter((d) => network(d) === "public"),
          internalDestinations: destinations.filter(
            (d) => network(d) === "internal",
          ),
          justification,
          evidence: context.evidence,
        },
      },
    );
    if (error) setError(describeError(error));
    else {
      setJustification("");
      setChosen({});
      onDone();
    }
  }
  return (
    <Dialog
      title="Request network access"
      isOpen={isOpen}
      onOpenChange={(o) => !o && onClose()}
    >
      <Form onSubmit={submit} className="samuori-form">
        {error && <Alert>{error}</Alert>}
        <p>
          Ask an organization administrator to open{" "}
          <strong>{describe(presets, destinations)}</strong> for this
          project&apos;s {context.method ? `${context.method} ` : ""}builds
          only. Other projects keep the organization&apos;s egress policy.
        </p>
        {destinations.map((d) => (
          <RadioGroup
            key={d}
            label={`Network for ${d}`}
            value={network(d)}
            onChange={(v) => setChosen({ ...chosen, [d]: v as Network })}
            options={[
              {
                id: "public",
                label: "Public internet",
                description: internalOnly.includes(d)
                  ? "not possible: this is a private address or an internal name"
                  : "the host must resolve to a public address",
                isDisabled: internalOnly.includes(d),
              },
              {
                id: "internal",
                label: "Internal network",
                description:
                  "the host is inside your network and may resolve to a private address; administrators see this as internal network access",
              },
            ]}
          />
        ))}
        <TextField
          label="Why it is needed"
          value={justification}
          onChange={setJustification}
          isRequired
          minLength={20}
          multiline
          description="At least 20 characters. Samuori attaches the prediction or the blocked build as evidence."
        />
        <div className="samuori-actions">
          <Button variant="secondary" onPress={onClose}>
            Cancel
          </Button>
          <Button type="submit">Send request</Button>
        </div>
      </Form>
    </Dialog>
  );
}
