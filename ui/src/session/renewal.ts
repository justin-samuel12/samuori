/**
 * Session renewal (ADR 0068): before a session reaches its maximum, the person confirms who they are and gets a new
 * session. The notice appears this long before the maximum, which is never shorter than the idle warning (at most
 * 10 minutes; docs/architecture/identity.md#sessions).
 */
export const renewalLeadMs = 10 * 60_000;

export function renewalDue(
  absoluteExpiresAt: number,
  warningMs: number,
  now: number,
): boolean {
  const left = absoluteExpiresAt - now;
  return left > 0 && left <= Math.max(renewalLeadMs, warningMs);
}

/**
 * A page to come back to after an identity-provider sign-in, or "/" when the value is not a same-origin path of the
 * web UI. The server validates it again; this only avoids sending what it would refuse.
 */
export function returnPath(path: string): string {
  if (
    !path.startsWith("/") ||
    path.startsWith("//") ||
    path.length > 2048 ||
    /[\\\s\p{Cc}]/u.test(path) ||
    path === "/api" ||
    path.startsWith("/api/") ||
    path.startsWith("/api?")
  )
    return "/";
  return path;
}

/** The page shown now, without the sign-in result the server adds after a failed provider sign-in. */
export function currentPage(
  loc: Pick<Location, "pathname" | "search" | "hash">,
): string {
  const query = new URLSearchParams(loc.search);
  query.delete("signin");
  const search = query.toString();
  return returnPath(loc.pathname + (search ? `?${search}` : "") + loc.hash);
}

export function oidcStartUrl(providerId: string, page: string): string {
  return `/api/v1/auth/oidc/${encodeURIComponent(providerId)}/start?returnTo=${encodeURIComponent(returnPath(page))}`;
}
