import { BrandMark } from "../components/BrandMark";
import { Button } from "../components/Button";
import { Dialog } from "../components/Dialog";
import { loadedServerVersion } from "../api/client";

export const creator = "Justin Samuel";
export const creatorEmail = "jsamuel12@gmail.com";

export function AboutDialog({
  isOpen,
  onOpenChange,
}: {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const version = loadedServerVersion();
  return (
    <Dialog title="About Samuori" isOpen={isOpen} onOpenChange={onOpenChange}>
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
            <p className="samuori-muted m-0">
              Self-hosted container image factory
            </p>
            <p className="samuori-motto samuori-about-motto">Woven into one</p>
          </div>
        </div>
        <p>
          Created, architected and maintained by <strong>{creator}</strong> (
          <a href={`mailto:${creatorEmail}`}>{creatorEmail}</a>).
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
          . The third-party components Samuori includes, and their licenses, are
          listed in <code>THIRD_PARTY_NOTICES.txt</code>, published with every
          release. The wordmark is outlined from DejaVu Sans Bold (
          <a
            href="/licenses/DejaVu-font-notice.txt"
            target="_blank"
            rel="noreferrer"
          >
            font notice
          </a>
          ).
        </p>
        <div className="samuori-actions">
          <Button onPress={() => onOpenChange(false)}>Close</Button>
        </div>
      </div>
    </Dialog>
  );
}
