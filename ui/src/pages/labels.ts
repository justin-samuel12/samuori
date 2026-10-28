import type { Role } from "../session/session";
import type { Team } from "./useTeams";

const roles: Record<Role, string> = {
  developer: "Developer",
  maintainer: "Maintainer",
  publisher: "Publisher",
  operator: "Operator",
  "security-reviewer": "Security reviewer",
  administrator: "Administrator",
};

export const roleOptions = (Object.keys(roles) as Role[]).map((id) => ({
  id,
  label: roles[id],
}));

export function roleLabel(r: Role): string {
  return roles[r];
}

export function scopeLabel(
  type: "organization" | "team" | "project",
  id: string,
  teams?: Team[],
): string {
  if (type === "organization") return "whole organization";
  if (type === "team")
    return `team ${teams?.find((t) => t.id === id)?.name ?? id}`;
  return `project ${id}`;
}
