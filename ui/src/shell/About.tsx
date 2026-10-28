import { useEffect, useState } from "react";
import { BrandMark } from "../components/BrandMark";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { Alert, Loading } from "../components/Feedback";
import { loadedServerVersion } from "../api/client";

export const creator = "Justin Samuel";

const noticeFiles = [
  "/licenses/THIRD_PARTY_NOTICES.txt",
  "/licenses/DejaVu-font-notice.txt",
];

/** Reads a notice file; an unknown path falls back to the app's HTML, which is treated as missing. */
async function readNotice(path: string): Promise<string | undefined> {
  const r = await fetch(path, { credentials: "same-origin" });
  const type = r.headers.get("Content-Type") ?? "";
  return r.ok && type.startsWith("text/plain") ? r.text() : undefined;
}

function Notices({ onBack }: { onBack: () => void }) {
  const [texts, setTexts] = useState<(string | undefined)[]>();
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    Promise.all(noticeFiles.map(readNotice)).then(setTexts, () =>
      setFailed(true),
    );
  }, []);
  const [deps, font] = texts ?? [];
  return (
    <div className="samuori-form">
      {failed && <Alert>The notices could not be loaded. Try again.</Alert>}
      {!texts && !failed && <Loading />}
      {texts && !deps && (
        <Alert tone="info">
          This build does not include <code>THIRD_PARTY_NOTICES.txt</code>.
          Release builds (<code>make release</code>) add it, and every release
          also publishes it beside its archives.
        </Alert>
      )}
      {texts && (deps || font) && (
        <pre className="samuori-notices" tabIndex={0}>
          {[deps, font].filter(Boolean).join("\n\n")}
        </pre>
      )}
      <div className="samuori-actions">
        {deps && (
          <a
            className="samuori-button samuori-button--quiet"
            href={noticeFiles[0]}
            target="_blank"
            rel="noreferrer"
          >
            Open as text
          </a>
        )}
        <Button onPress={onBack}>Back</Button>
      </div>
    </div>
  );
}

export function AboutDialog({
  isOpen,
  onOpenChange,
}: {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const version = loadedServerVersion();
  const [showNotices, setShowNotices] = useState(false);
  const change = (open: boolean) => {
    if (!open) setShowNotices(false);
    onOpenChange(open);
  };
  return (
    <Dialog
      title={showNotices ? "Third-party notices" : "About Samuori"}
      isOpen={isOpen}
      onOpenChange={change}
      wide={showNotices}
    >
      {showNotices ? (
        <Notices onBack={() => setShowNotices(false)} />
      ) : (
        <div className="samuori-form">
          <div className="samuori-about-brand">
            <BrandMark finish="3d" alt="" className="w-[72px] flex-none" />
            <div>
              <p className="m-0">
                <strong>Samuori</strong>
                {version && (
                  <span className="samuori-muted"> · version {version}</span>
                )}
              </p>
              <p className="samuori-motto samuori-about-motto">
                Woven into one
              </p>
            </div>
          </div>
          <p>
            Conceived by <strong>{creator}</strong>.
          </p>
          <p>
            Licensed under the{" "}
            <a
              href="https://www.apache.org/licenses/LICENSE-2.0"
              target="_blank"
              rel="noreferrer"
            >
              Apache License, Version 2.0
            </a>
            . The third-party components Samuori includes keep their own
            licenses.
          </p>
          <div className="samuori-actions">
            <Button variant="secondary" onPress={() => setShowNotices(true)}>
              View third-party notices
            </Button>
            <Button onPress={() => change(false)}>Close</Button>
          </div>
        </div>
      )}
    </Dialog>
  );
}
