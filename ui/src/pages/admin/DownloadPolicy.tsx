import { useCallback, useState } from "react";
import { api, describeError, type Schemas } from "../../api/client";
import { Button } from "../../components/Button";
import { Checkbox } from "../../components/Field";
import { Alert, Section } from "../../components/Feedback";
import { useLoader } from "../../hooks";

type PublicationDocument = { developerDownloads: boolean };

/** The organization's publication policy: whether developers may download validated images (ADR 0053). */
export function DownloadPolicy() {
  const [policy, setPolicy] = useState<Schemas["Policy"] | null>();
  const [allowed, setAllowed] = useState(false);
  const [message, setMessage] = useState<{
    tone: "info" | "error";
    text: string;
  }>();
  const load = useCallback(async () => {
    const { data } = await api.GET("/policies");
    const p = data?.items.find((x) => x.kind === "publication") ?? null;
    setPolicy(p);
    if (p) setAllowed(!!(p.document as PublicationDocument).developerDownloads);
  }, []);
  useLoader(load);
  if (!policy) return null;
  const saved = !!(policy.document as PublicationDocument).developerDownloads;

  async function save() {
    if (!policy) return;
    setMessage(undefined);
    const { error } = await api.PUT("/policies/{policyId}", {
      params: {
        path: { policyId: policy.id },
        header: { "If-Match": `"${policy.version}"` },
      },
      body: {
        kind: "publication",
        document: { developerDownloads: allowed },
        reason: allowed
          ? "Developers may download validated images"
          : "Only maintainers, publishers and administrators download images",
      },
    });
    if (error) setMessage({ tone: "error", text: describeError(error) });
    else {
      setMessage({ tone: "info", text: "Download setting saved." });
      void load();
    }
  }

  return (
    <Section
      title="Image downloads"
      description="Who may download a validated image as an archive, outside any publication."
    >
      {message && <Alert tone={message.tone}>{message.text}</Alert>}
      <Checkbox isSelected={allowed} onChange={setAllowed}>
        Developers may download validated images of their projects
      </Checkbox>
      <p className="samuori-muted">
        Maintainers, publishers and administrators can always download. A
        download skips publication approvals, signing and tag rules, so the
        image can be run or pushed anywhere; every download is audited, and
        restricted or quarantined images are never downloadable.
      </p>
      <div className="samuori-actions samuori-actions--start">
        <Button isDisabled={allowed === saved} onPress={() => void save()}>
          Save download setting
        </Button>
      </div>
    </Section>
  );
}
