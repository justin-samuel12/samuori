import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { BrandMark } from "./BrandMark";

const PRIMARY = "/api/v1/branding/logo";
const DARK = "/api/v1/branding/logo/dark";

/** The operator branding, cached for the session; undefined while loading, null when none is set. */
export function useBranding() {
  return useQuery({
    queryKey: ["branding"],
    queryFn: async () => (await api.GET("/branding")).data ?? null,
    staleTime: 5 * 60 * 1000,
  });
}

/**
 * The portal top-bar logo: the operator's wide logo when one is set, otherwise the Samuori mark (ADR 0072). Both
 * theme variants render and CSS shows the one matching the theme (.samuori-brand-img).
 */
export function TopBarLogo({ className }: { className?: string }) {
  const { data } = useBranding();
  if (!data?.customLogo || !data.logo) {
    return <BrandMark kind="logo" className={className} />;
  }
  const dark = data.logoDark?.url ?? data.logo.url;
  return (
    <>
      <img
        src={data.logo.url}
        alt="Home"
        className={`samuori-brand-img samuori-brand-img--light ${className ?? ""}`}
      />
      <img
        src={dark}
        alt=""
        className={`samuori-brand-img samuori-brand-img--dark ${className ?? ""}`}
      />
    </>
  );
}

/**
 * The login-page logo, shown before sign-in. It loads the public logo endpoint directly; if none is set the image
 * fails and the Samuori fallback is shown instead (ADR 0072). The dark image falls back to the primary when no dark
 * variant exists.
 */
export function LoginLogo({
  fallback,
  className,
}: {
  fallback: ReactNode;
  className?: string;
}) {
  const [state, setState] = useState<"trying" | "none">("trying");
  if (state === "none") return <>{fallback}</>;
  return (
    <>
      <img
        src={PRIMARY}
        alt=""
        className={`samuori-brand-img samuori-brand-img--light ${className ?? ""}`}
        onError={() => setState("none")}
      />
      <img
        src={DARK}
        alt=""
        className={`samuori-brand-img samuori-brand-img--dark ${className ?? ""}`}
        onError={(e) => {
          const img = e.currentTarget;
          if (!img.src.endsWith(PRIMARY)) img.src = PRIMARY;
        }}
      />
    </>
  );
}
