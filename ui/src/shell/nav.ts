import {
  Activity,
  ArrowLeftRight,
  Bell,
  Blocks,
  Boxes,
  Braces,
  ChefHat,
  Cpu,
  Database,
  DatabaseBackup,
  FileBadge,
  Fingerprint,
  FolderKanban,
  Gauge,
  Globe,
  Hammer,
  HardDrive,
  HeartPulse,
  KeyRound,
  LayoutDashboard,
  LifeBuoy,
  ListOrdered,
  Network,
  Radio,
  Scale,
  Send,
  Server,
  Settings2,
  Share2,
  ShieldAlert,
  ShieldCheck,
  Ship,
  SquarePlus,
  Stethoscope,
  UserRound,
  UsersRound,
  Workflow,
  type LucideIcon,
} from "lucide-react";
import type { Me } from "../session/session";
import { can, canAny } from "../session/can.ts";
import type { OperationId } from "../api/generated/roles";

export interface NavItem {
  href: string;
  label: string;
  icon: LucideIcon;
  /** Organization administrators only, unless operator or reviewer opens it to those roles. */
  admin?: boolean;
  operator?: boolean;
  reviewer?: boolean;
  /** The page's main operations; the entry is shown when the user's roles allow one of them. */
  ops?: OperationId[];
  /** Operations the page cannot work without; all must be allowed. */
  needs?: OperationId[];
}

export interface NavGroup {
  label?: string;
  items: NavItem[];
}

export interface Workspace {
  id: string;
  label: string;
  icon: LucideIcon;
  /** An administration section: its pages wrap into a sub-navigation instead of a scrolling tab strip. */
  admin?: boolean;
  groups: NavGroup[];
}

export const workspaces: Workspace[] = [
  {
    id: "overview",
    label: "Overview",
    icon: LayoutDashboard,
    groups: [{ items: [{ href: "/", label: "Overview", icon: Gauge }] }],
  },
  {
    id: "projects",
    label: "Projects",
    icon: FolderKanban,
    groups: [
      {
        items: [
          {
            href: "/projects",
            label: "Projects",
            icon: FolderKanban,
            ops: ["listProjects"],
          },
          {
            href: "/new-image",
            label: "New image",
            icon: SquarePlus,
            ops: ["createSpec"],
          },
        ],
      },
    ],
  },
  {
    id: "builds",
    label: "Builds",
    icon: Hammer,
    groups: [
      {
        items: [
          {
            href: "/builds",
            label: "Build runs",
            icon: Hammer,
            ops: ["listBuilds"],
          },
          {
            href: "/images",
            label: "Images",
            icon: Boxes,
            ops: ["listProjectImages", "listPublications"],
          },
          {
            href: "/pipelines",
            label: "Pipelines",
            icon: Workflow,
            ops: ["listPipelines"],
          },
          {
            href: "/approvals",
            label: "Approvals",
            icon: ShieldCheck,
            ops: ["listApprovals"],
          },
        ],
      },
    ],
  },
  {
    id: "kubernetes",
    label: "Kubernetes",
    icon: Ship,
    groups: [
      {
        items: [
          {
            href: "/kubernetes",
            label: "Configurations",
            icon: Ship,
            needs: ["listProjects", "listKubeConversions"],
          },
        ],
      },
    ],
  },
  {
    id: "supply",
    label: "Build supply",
    icon: Send,
    admin: true,
    groups: [
      {
        items: [
          {
            href: "/admin/registries",
            label: "Registries & destinations",
            icon: Send,
            admin: true,
          },
          {
            href: "/admin/recipes",
            label: "Recipes & runtimes",
            icon: ChefHat,
            admin: true,
          },
          {
            href: "/admin/policies",
            label: "Build policies",
            icon: Scale,
            admin: true,
          },
          {
            href: "/admin/providers",
            label: "Build & conversion providers",
            icon: Blocks,
            admin: true,
          },
        ],
      },
    ],
  },
  {
    id: "infrastructure",
    label: "Infrastructure",
    icon: Server,
    admin: true,
    groups: [
      {
        items: [
          {
            href: "/admin/workers",
            label: "Workers & pools",
            icon: Server,
            admin: true,
          },
          {
            href: "/admin/servers",
            label: "Servers",
            icon: Network,
            admin: true,
          },
          {
            href: "/admin/capacity",
            label: "Capacity",
            icon: Gauge,
            admin: true,
            operator: true,
          },
          {
            href: "/admin/engines",
            label: "Build engines",
            icon: Cpu,
            admin: true,
            operator: true,
          },
          {
            href: "/admin/queue",
            label: "Build queue",
            icon: ListOrdered,
            admin: true,
            operator: true,
          },
          {
            href: "/admin/storage",
            label: "Storage",
            icon: HardDrive,
            admin: true,
            operator: true,
          },
          {
            href: "/admin/database",
            label: "Database",
            icon: Database,
            admin: true,
          },
          {
            href: "/admin/messaging",
            label: "Messaging",
            icon: Radio,
            admin: true,
            operator: true,
          },
        ],
      },
    ],
  },
  {
    id: "security",
    label: "Security",
    icon: KeyRound,
    admin: true,
    groups: [
      {
        items: [
          {
            href: "/admin/secrets",
            label: "Secrets",
            icon: KeyRound,
            admin: true,
          },
          {
            href: "/admin/certificates",
            label: "Certificates",
            icon: FileBadge,
            admin: true,
          },
          {
            href: "/admin/egress",
            label: "Egress",
            icon: Globe,
            admin: true,
          },
          {
            href: "/admin/quarantines",
            label: "Quarantines",
            icon: ShieldAlert,
            admin: true,
          },
          {
            href: "/admin/observability",
            label: "Audit & observability",
            icon: Activity,
            admin: true,
            operator: true,
            reviewer: true,
          },
        ],
      },
    ],
  },
  {
    id: "organization",
    label: "Organization",
    icon: Settings2,
    admin: true,
    groups: [
      {
        items: [
          {
            href: "/admin/health",
            label: "Health",
            icon: HeartPulse,
            admin: true,
          },
          {
            href: "/admin/users",
            label: "Users",
            icon: UserRound,
            admin: true,
          },
          {
            href: "/admin/teams",
            label: "Teams",
            icon: UsersRound,
            admin: true,
          },
          {
            href: "/admin/identity",
            label: "Identity & sign-in",
            icon: Fingerprint,
            admin: true,
          },
          {
            href: "/admin/notifications",
            label: "Notification channels",
            icon: Bell,
            admin: true,
          },
          {
            href: "/admin/federation",
            label: "Federation",
            icon: Share2,
            admin: true,
          },
          {
            href: "/admin/backups",
            label: "Backups",
            icon: DatabaseBackup,
            admin: true,
          },
          {
            href: "/admin/configuration",
            label: "Export & import",
            icon: ArrowLeftRight,
            admin: true,
          },
          {
            href: "/admin/upgrades",
            label: "Upgrades & diagnostics",
            icon: Stethoscope,
            admin: true,
            operator: true,
          },
        ],
      },
    ],
  },
];

/** Old addresses of moved pages, so bookmarks and links in guides keep working. */
export const redirects: Record<string, string> = {
  "/admin": "/admin/health",
};

export function redirectFor(path: string): string | undefined {
  const to = redirects[path.replace(/\/+$/, "") || "/"];
  return to !== path ? to : undefined;
}

export const accountNav: NavItem[] = [
  { href: "/account", label: "Account", icon: UserRound },
  { href: "/notifications", label: "Notifications", icon: Bell },
  { href: "/help", label: "Help", icon: LifeBuoy },
  { href: "/help/api-reference", label: "API reference", icon: Braces },
];

function hasOrgRole(me: Me, role: string): boolean {
  return me.roles.some(
    (r) => r.role === role && r.scopeType === "organization",
  );
}

export function isOperator(me: Me): boolean {
  return hasOrgRole(me, "operator");
}

function isOrgAdmin(me: Me): boolean {
  return hasOrgRole(me, "administrator");
}

export function isReviewer(me: Me): boolean {
  return hasOrgRole(me, "security-reviewer");
}

function visible(me: Me, i: NavItem): boolean {
  if (i.ops && !canAny(me, i.ops)) return false;
  if (i.needs && !i.needs.every((op) => can(me, op))) return false;
  if (!i.admin) return true;
  return (
    isOrgAdmin(me) ||
    (i.operator === true && isOperator(me)) ||
    (i.reviewer === true && isReviewer(me))
  );
}

/**
 * Workspaces and sections the signed-in user may use: entries whose main operations their roles allow (from the API
 * document's x-samuori-roles), and administration pages for organization administrators, operators and reviewers.
 * Hiding is not authorization; the server still checks every call.
 */
export function visibleWorkspaces(me: Me): Workspace[] {
  return workspaces
    .map((w) => ({
      ...w,
      groups: w.groups
        .map((g) => ({ ...g, items: g.items.filter((i) => visible(me, i)) }))
        .filter((g) => g.items.length > 0),
    }))
    .filter((w) => w.groups.length > 0);
}

export function items(w: Workspace): NavItem[] {
  return w.groups.flatMap((g) => g.items);
}

export function landing(w: Workspace): string {
  return items(w)[0]?.href ?? "/";
}

export function isCurrent(href: string, path: string): boolean {
  return href === "/"
    ? path === href
    : path === href || path.startsWith(href + "/");
}

const kubernetesPaths = /^\/(kube-conversions\/|projects\/[^/]+\/kubernetes)/;
const wizardPaths = /^\/projects\/[^/]+\/new-image$/;

/** The workspace and section a path belongs to, among the user's workspaces. */
export function locate(
  list: Workspace[],
  path: string,
): { workspace?: Workspace; item?: NavItem } {
  if (kubernetesPaths.test(path)) {
    const workspace = list.find((w) => w.id === "kubernetes");
    if (workspace) return { workspace, item: items(workspace)[0] };
  }
  if (wizardPaths.test(path))
    for (const workspace of list) {
      const item = items(workspace).find((i) => i.href === "/new-image");
      if (item) return { workspace, item };
    }
  let best: { workspace?: Workspace; item?: NavItem } = {};
  for (const workspace of list)
    for (const item of items(workspace))
      if (
        isCurrent(item.href, path) &&
        item.href.length > (best.item?.href.length ?? -1)
      )
        best = { workspace, item };
  return best;
}
