import { useEffect, useRef, useState } from "react";
import { DropZone, FileTrigger } from "react-aria-components";
import { useQueryClient } from "@tanstack/react-query";
import { api, readCookie } from "../../api/client";
import { Button } from "../../components/Button";
import { Alert, PageHeader, Section } from "../../components/Feedback";
import { BrandMark } from "../../components/BrandMark";
import { useBranding } from "../../components/OperatorLogo";

const MAX_BYTES = 512 * 1024;
const ACCEPT = ["image/png", "image/svg+xml"];

type Variant = "primary" | "dark";

interface Inspection {
  type: "png" | "svg" | "other";
  bytes: number;
  width: number;
  height: number;
  aspect: number;
  transparent: boolean;
  darkOnTransparent: boolean;
}

interface Pending {
  file: File;
  url: string;
  info: Inspection;
}

function kb(n: number): string {
  return `${Math.round(n / 1024)} KB`;
}

async function inspect(file: File): Promise<Inspection> {
  const type =
    file.type === "image/png"
      ? "png"
      : file.type === "image/svg+xml" ||
          file.name.toLowerCase().endsWith(".svg")
        ? "svg"
        : "other";
  const base: Inspection = {
    type,
    bytes: file.size,
    width: 0,
    height: 0,
    aspect: 0,
    transparent: false,
    darkOnTransparent: false,
  };
  if (type === "other") return base;

  const url = URL.createObjectURL(file);
  try {
    const img = new Image();
    img.src = url;
    await img.decode().catch(() => undefined);
    let width = img.naturalWidth;
    let height = img.naturalHeight;
    if (type === "svg" && (!width || !height)) {
      const text = await file.text();
      const vb = /viewBox\s*=\s*["']([-\d.\s,]+)["']/.exec(text);
      if (vb?.[1]) {
        const p = vb[1].split(/[\s,]+/).map(Number);
        if (p.length === 4) {
          width = width || p[2] || 0;
          height = height || p[3] || 0;
        }
      }
    }
    base.width = Math.round(width);
    base.height = Math.round(height);
    base.aspect = height ? width / height : 0;

    try {
      const w = Math.min(160, width || 160);
      const h = width ? Math.round((w * height) / width) || 40 : 40;
      const canvas = document.createElement("canvas");
      canvas.width = w;
      canvas.height = h;
      const ctx = canvas.getContext("2d", { willReadFrequently: true });
      if (ctx) {
        ctx.drawImage(img, 0, 0, w, h);
        const { data } = ctx.getImageData(0, 0, w, h);
        let clear = 0;
        let opaque = 0;
        let lum = 0;
        for (let i = 0; i < data.length; i += 4) {
          const a = data[i + 3] ?? 0;
          if (a < 24) {
            clear++;
            continue;
          }
          opaque++;
          lum +=
            0.2126 * (data[i] ?? 0) +
            0.7152 * (data[i + 1] ?? 0) +
            0.0722 * (data[i + 2] ?? 0);
        }
        const total = clear + opaque || 1;
        base.transparent = clear / total > 0.15;
        base.darkOnTransparent =
          base.transparent && opaque > 0 && lum / opaque < 90;
      }
    } catch {
      // Canvas inspection is best-effort guidance; a failure just omits the contrast hint.
    }
  } finally {
    URL.revokeObjectURL(url);
  }
  return base;
}

async function putLogo(file: File, variant: Variant): Promise<string | null> {
  const r = await fetch(
    `${window.location.origin}/api/v1/branding/logo?variant=${variant}`,
    {
      method: "POST",
      body: file,
      credentials: "same-origin",
      headers: {
        "Content-Type": file.type || "application/octet-stream",
        "X-CSRF-Token": readCookie("samuori_csrf") ?? "",
      },
    },
  );
  if (r.ok) return null;
  if (r.status === 413) return "That file is larger than 512 KB.";
  if (r.status === 415) return "Upload a PNG or SVG image.";
  if (r.status === 422) return "That file is not a valid PNG or SVG.";
  return "The upload failed. Try again.";
}

export function BrandingPage() {
  const { data } = useBranding();
  const qc = useQueryClient();
  const refresh = () => qc.invalidateQueries({ queryKey: ["branding"] });

  const hasPrimary = Boolean(data?.customLogo && data.logo);
  const hasDark = Boolean(data?.logoDark);
  const [suggestDark, setSuggestDark] = useState(false);
  const showDarkSlot = hasDark || suggestDark;

  return (
    <>
      <PageHeader
        help="branding"
        title="Branding"
        description="Show your own logo in the portal top bar and on the sign-in page, in place of the Samuori mark. Samuori's colours and the About dialog are unchanged."
      />
      <Alert tone="info">
        PNG or SVG, up to 512 KB. A transparent background is recommended, and a
        wide logo (about 3:1 to 4:1) fits the top bar best. Leave it empty to
        use the Samuori logo.
      </Alert>

      <LogoSlot
        variant="primary"
        title="Logo"
        description="Shown in the top bar and on the sign-in page. Clearing it reverts to the Samuori logo everywhere."
        currentUrl={hasPrimary ? data?.logo?.url : undefined}
        onChanged={refresh}
        onContrast={(flag) => setSuggestDark((s) => s || flag)}
      />

      {showDarkSlot ? (
        <LogoSlot
          variant="dark"
          title="Dark-mode logo (optional)"
          description="Used on the dark top bar when your main logo would be hard to see there. Clearing it falls back to the main logo."
          currentUrl={hasDark ? data?.logoDark?.url : undefined}
          onChanged={refresh}
        />
      ) : (
        <Section
          title="Dark-mode logo (optional)"
          description="If your logo is dark and would be hard to see on the dark top bar, add a lighter version for dark mode."
        >
          <Button variant="secondary" onPress={() => setSuggestDark(true)}>
            Add a dark-mode logo
          </Button>
        </Section>
      )}
    </>
  );
}

function LogoSlot({
  variant,
  title,
  description,
  currentUrl,
  onChanged,
  onContrast,
}: {
  variant: Variant;
  title: string;
  description: string;
  currentUrl?: string;
  onChanged: () => void;
  onContrast?: (flag: boolean) => void;
}) {
  const [pending, setPending] = useState<Pending>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const lastUrl = useRef<string | undefined>(undefined);

  useEffect(
    () => () => {
      if (lastUrl.current) URL.revokeObjectURL(lastUrl.current);
    },
    [],
  );

  async function choose(file: File | undefined) {
    if (!file) return;
    setError(undefined);
    setSaved(false);
    const info = await inspect(file);
    if (info.type === "other") {
      setError("Choose a PNG or SVG image.");
      return;
    }
    if (info.bytes > MAX_BYTES) {
      setError(`That file is ${kb(info.bytes)}; the limit is 512 KB.`);
      return;
    }
    if (lastUrl.current) URL.revokeObjectURL(lastUrl.current);
    const url = URL.createObjectURL(file);
    lastUrl.current = url;
    setPending({ file, url, info });
    if (variant === "primary") onContrast?.(info.darkOnTransparent);
  }

  async function save() {
    if (!pending) return;
    setBusy(true);
    setError(undefined);
    const err = await putLogo(pending.file, variant);
    setBusy(false);
    if (err) {
      setError(err);
      return;
    }
    setPending(undefined);
    setSaved(true);
    onChanged();
  }

  async function remove() {
    setBusy(true);
    setError(undefined);
    if (variant === "dark") await api.DELETE("/branding/logo/dark");
    else await api.DELETE("/branding");
    setBusy(false);
    setPending(undefined);
    setSaved(false);
    onChanged();
  }

  const info = pending?.info;
  const previewUrl = pending?.url ?? currentUrl;

  return (
    <Section title={title} description={description}>
      <DropZone
        className="samuori-dropzone"
        aria-label={`Drop a ${title.toLowerCase()} here`}
        isDisabled={busy}
        onDrop={(e) => {
          const f = e.items.find(
            (i): i is Extract<typeof i, { kind: "file" }> => i.kind === "file",
          );
          if (f) void f.getFile().then((file) => void choose(file));
        }}
      >
        <p className="m-0 font-medium">
          Drop a PNG or SVG here, or choose one.
        </p>
        <p className="samuori-muted m-0 text-sm">
          Up to 512 KB. Transparent background recommended; about 3:1 to 4:1
          suits the top bar.
        </p>
        <div className="samuori-actions samuori-actions--start">
          <FileTrigger
            acceptedFileTypes={ACCEPT}
            onSelect={(l) => void choose(l?.[0] ?? undefined)}
          >
            <Button variant="secondary" isDisabled={busy}>
              Choose image
            </Button>
          </FileTrigger>
        </div>
      </DropZone>

      {error && <Alert>{error}</Alert>}
      {saved && <Alert tone="success">Saved. The portal shows it now.</Alert>}

      {info && (
        <ul className="samuori-plain-list mt-3 grid gap-1 text-sm">
          <li>
            {info.type.toUpperCase()}, {kb(info.bytes)}
            {info.width > 0 && (
              <>
                {" · "}
                {info.width}×{info.height} px
              </>
            )}
          </li>
          {info.aspect > 0 && (
            <li>
              Aspect ratio {info.aspect.toFixed(2)}:1 —{" "}
              {info.aspect >= 2.5 && info.aspect <= 4.5
                ? "a good fit for the top bar."
                : info.aspect < 2.5
                  ? "narrower than the top bar's wide strip; it will show smaller."
                  : "wider than the top bar's strip; it will show smaller."}
            </li>
          )}
          <li>
            {info.transparent
              ? "Transparent background detected."
              : "No transparent background detected; a solid background may look like a box."}
          </li>
          {variant === "primary" && info.darkOnTransparent && (
            <li className="text-warn">
              This logo looks dark on a transparent background and may be hard
              to see on the dark top bar. Add a dark-mode logo below.
            </li>
          )}
        </ul>
      )}

      {previewUrl && (
        <div className="mt-4 grid gap-4 sm:grid-cols-2">
          <PreviewTile label="Top bar">
            <div
              className="flex h-12 items-center rounded-lg border border-line px-4"
              style={{ background: "var(--samuori-bar-bg)" }}
            >
              <img
                src={previewUrl}
                alt=""
                className="max-h-8 w-[120px] object-contain object-left"
              />
            </div>
          </PreviewTile>
          <PreviewTile label="Sign-in card">
            <div className="grid place-items-center rounded-lg border border-line bg-surface p-6">
              <img
                src={previewUrl}
                alt=""
                className="max-h-20 w-[200px] object-contain"
              />
            </div>
          </PreviewTile>
        </div>
      )}
      {previewUrl && (
        <p className="samuori-muted mt-2 text-sm">
          Previews use the current theme. Switch the theme from the top-right
          menu to check the other, or add a dark-mode logo.
        </p>
      )}

      <div className="samuori-actions samuori-actions--start mt-4">
        <Button isDisabled={!pending || busy} onPress={() => void save()}>
          Save logo
        </Button>
        {currentUrl && (
          <Button
            variant="danger"
            isDisabled={busy}
            onPress={() => void remove()}
          >
            {variant === "dark"
              ? "Remove dark-mode logo"
              : "Reset to Samuori logo"}
          </Button>
        )}
      </div>

      {!currentUrl && !pending && variant === "primary" && (
        <div className="mt-4">
          <p className="samuori-muted mb-2 text-sm">
            No custom logo set; the portal shows the Samuori logo:
          </p>
          <div
            className="inline-flex items-center rounded-lg border border-line px-4 py-2"
            style={{ background: "var(--samuori-bar-bg)" }}
          >
            <BrandMark kind="logo" className="w-[120px]" />
          </div>
        </div>
      )}
    </Section>
  );
}

function PreviewTile({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <p className="samuori-muted mb-1 text-xs font-semibold tracking-wide uppercase">
        {label}
      </p>
      {children}
    </div>
  );
}
