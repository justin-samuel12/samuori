import {
  operationAccess,
  type Access,
  type OperationId,
} from "../api/generated/roles.ts";

interface Binding {
  role: string;
  scopeType: "organization" | "team" | "project";
  scopeId: string;
}

/** Where an object sits; a binding on its team or project, or any organization binding, covers it. */
export interface Scope {
  teamId?: string;
  projectId?: string;
}

/**
 * Whether the server's role check would let these bindings call an operation: the coarse check every request
 * passes (internal/api/middleware.go), narrowed to the scope when the page knows it, as authz.Can does. It only
 * hides what would be refused; the server still authorizes every call.
 */
export function allows(
  access: Access,
  roles: readonly Binding[],
  scope?: Scope,
): boolean {
  if (access === "public" || access === "any") return true;
  return roles.some(
    (b) =>
      access.includes(b.role) &&
      (!scope ||
        b.scopeType === "organization" ||
        (b.scopeType === "team" && b.scopeId === scope.teamId) ||
        (b.scopeType === "project" && b.scopeId === scope.projectId)),
  );
}

export function can(
  me: { roles: readonly Binding[] },
  op: OperationId,
  scope?: Scope,
): boolean {
  return allows(operationAccess[op], me.roles, scope);
}

/** True when any of the operations is allowed, for a page or section that offers several. */
export function canAny(
  me: { roles: readonly Binding[] },
  ops: readonly OperationId[],
  scope?: Scope,
): boolean {
  return ops.some((op) => can(me, op, scope));
}

/** Developers download images only when the organization's publication policy allows it (ADR 0053). */
export function canDownload(me: {
  roles: readonly Binding[];
  developerDownloads?: boolean;
}): boolean {
  return (
    can(me, "downloadArtifact") &&
    (allows(["maintainer", "publisher", "administrator"], me.roles) ||
      me.developerDownloads === true)
  );
}
