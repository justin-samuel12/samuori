import { useCallback, useState } from "react";
import { api, describeError, type Schemas } from "../api/client";
import { useLoader } from "../hooks";
import { Button } from "./Button";
import { Dialog } from "./Dialog";
import { Alert, Loading } from "./Feedback";
import { Select } from "./Field";

type Info = Schemas["DeploymentFiles"];

/** Starting Compose and Helm files that reference a built image (ADR 0059). */
export function DeploymentFiles({
  artifactId,
  variant = "secondary",
}: {
  artifactId: string;
  variant?: "secondary" | "quiet";
}) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button variant={variant} onPress={() => setOpen(true)}>
        Deployment files
      </Button>
      {open && (
        <DeploymentFilesDialog
          artifactId={artifactId}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

function DeploymentFilesDialog({
  artifactId,
  onClose,
}: {
  artifactId: string;
  onClose: () => void;
}) {
  const [publicationId, setPublicationId] = useState<string>();
  const [info, setInfo] = useState<Info | null>();
  const [error, setError] = useState<string>();
  const load = useCallback(async () => {
    const { data, error } = await api.GET("/artifacts/{artifactId}/deploy", {
      params: {
        path: { artifactId },
        query: publicationId ? { publicationId } : {},
      },
    });
    setInfo(data ?? null);
    setError(error ? describeError(error) : undefined);
  }, [artifactId, publicationId]);
  useLoader(load);

  const query = info?.publicationId
    ? `?publicationId=${encodeURIComponent(info.publicationId)}`
    : "";
  const base = `/api/v1/artifacts/${artifactId}/deploy`;
  return (
    <Dialog
      title="Deployment files"
      isOpen
      onOpenChange={(o) => !o && onClose()}
    >
      <div className="grid gap-4">
        <p className="samuori-muted m-0">
          Starting files that run this image. Review them before use; Samuori
          does not deploy anything.
        </p>
        {error && <Alert>{error}</Alert>}
        {info === undefined && <Loading />}
        {info && (
          <>
            {info.publications.length > 1 && (
              <Select
                label="Publication"
                placeholder="Choose a publication"
                value={info.publicationId ?? null}
                onChange={setPublicationId}
                options={info.publications.map((p) => ({
                  id: p.publicationId,
                  label: `${p.destinationName ?? p.reference} · ${new Date(p.publishedAt).toLocaleString()}`,
                }))}
              />
            )}
            <p className="m-0 text-sm">
              {info.published
                ? "The files use the published image, by tag and digest: "
                : "This image is not published, so the files use the name docker load gives it. Download the image and load it first: "}
              <code className="break-all">{info.reference}</code>
            </p>
            <div className="samuori-actions samuori-actions--start">
              <a
                className="samuori-button samuori-button--secondary no-underline"
                href={`${base}/compose${query}`}
                download={info.composeFilename}
              >
                Docker Compose ({info.composeFilename})
              </a>
              <a
                className="samuori-button samuori-button--secondary no-underline"
                href={`${base}/helm${query}`}
                download={info.chartFilename}
              >
                Helm chart (.tgz)
              </a>
            </div>
          </>
        )}
        <div className="samuori-actions">
          <Button onPress={onClose}>Close</Button>
        </div>
      </div>
    </Dialog>
  );
}
