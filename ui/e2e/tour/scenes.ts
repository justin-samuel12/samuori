// The screenshot tour: every screen worth showing, in the order of docs/screenshots/README.md. A change that adds or
// changes a screen updates its scene here and runs make screenshots; make screenshots-check fails when a route in
// src/App.tsx has neither a scene nor an entry in notCaptured.
import type { Page } from "@playwright/test";
import { chooseOption } from "../helpers.ts";
import type { Seeded, User } from "./seed.ts";

export type Audience = "signin" | "developer" | "admin";

export interface Scene {
  audience: Audience;
  name: string;
  caption: string;
  /** The App.tsx route pattern this scene shows. */
  route: string;
  /** The URL to open when the route has parameters. */
  url?: (ids: Seeded) => string;
  user?: User;
  /** Captures the whole page instead of the first screenful. */
  fullPage?: boolean;
  /** The screen shows an error on purpose, such as a failed build. */
  showsError?: boolean;
  /** Steps after the page has loaded, such as opening a dialog. */
  act?: (page: Page) => Promise<void>;
}

async function openAccountMenuItem(page: Page, item: string) {
  await page.getByRole("button", { name: /^Account menu for/ }).click();
  await page.getByRole("menuitem", { name: item }).click();
  await page.getByRole("dialog").waitFor();
}

async function chooseShop(page: Page) {
  await chooseOption(page, "Project", "shop");
  await page.getByRole("grid", { name: "Images" }).waitFor();
}

const dev = (s: Omit<Scene, "audience" | "user">): Scene => ({
  ...s,
  audience: "developer",
  user: "dana",
});
const admin = (s: Omit<Scene, "audience" | "user">): Scene => ({
  ...s,
  audience: "admin",
  user: "admin",
});

export const scenes: Scene[] = [
  {
    audience: "signin",
    name: "sign-in",
    caption: "Sign in",
    route: "/",
  },

  dev({ name: "overview", caption: "Overview", route: "/" }),
  dev({ name: "projects", caption: "Projects", route: "/projects" }),
  dev({
    name: "project",
    caption: "A project: uploads, specs and builds",
    route: "/projects/:id",
    url: (ids) => `/projects/${ids.shop}`,
  }),
  dev({
    name: "upload",
    caption: "Upload source",
    route: "/projects/:id/upload",
    url: (ids) => `/projects/${ids.shop}/upload`,
  }),
  dev({
    name: "snapshot",
    caption: "Uploaded source and what discovery found",
    route: "/projects/:id/snapshots/:sid",
    url: (ids) => `/projects/${ids.shop}/snapshots/${ids.snapshot}`,
  }),
  dev({
    name: "new-image",
    caption: "New image: pick a project",
    route: "/new-image",
  }),
  dev({
    name: "wizard-upload",
    caption: "New image wizard, first step",
    route: "/projects/:id/new-image",
    url: (ids) => `/projects/${ids.shop}/new-image`,
  }),
  dev({
    name: "wizard-describe",
    caption: "New image wizard: describe the app",
    route: "/projects/:id/new-image",
    url: (ids) => `/projects/${ids.shop}/new-image`,
    act: async (page) => {
      await page.getByRole("button", { name: "Next" }).click();
      await page.getByText(/Express · javascript/).click();
      await page.getByRole("button", { name: "Next" }).click();
      await page.getByLabel("Image name").waitFor();
    },
  }),
  dev({ name: "builds", caption: "Build runs", route: "/builds" }),
  dev({
    name: "build",
    caption: "A build with its log and validation checks",
    route: "/builds/:id",
    url: (ids) => `/builds/${ids.succeeded}`,
    fullPage: true,
  }),
  dev({
    name: "build-running",
    caption: "A build in progress, with its live log",
    route: "/builds/:id",
    url: (ids) => `/builds/${ids.running}`,
  }),
  dev({
    name: "build-failed",
    caption: "A failed build explains what went wrong",
    route: "/builds/:id",
    url: (ids) => `/builds/${ids.failed}`,
    showsError: true,
  }),
  dev({
    name: "images",
    caption: "Images",
    route: "/images",
    act: (page) => chooseShop(page),
  }),
  dev({
    name: "deployment-files",
    caption: "Deployment files for an image",
    route: "/images",
    act: async (page) => {
      await chooseShop(page);
      await page
        .getByRole("button", { name: /Deployment files/ })
        .first()
        .click();
      await page.getByRole("dialog").waitFor();
    },
  }),
  dev({
    name: "image",
    caption: "An image, its validation and publications",
    route: "/images/:id",
    url: (ids) => `/images/${ids.image}`,
  }),
  dev({
    name: "requests",
    caption: "Requests and approvals",
    route: "/approvals",
  }),
  dev({ name: "pipelines", caption: "Pipelines", route: "/pipelines" }),
  dev({
    name: "kubernetes",
    caption: "Kubernetes configurations",
    route: "/kubernetes",
  }),
  dev({
    name: "project-kubernetes",
    caption: "A project's Kubernetes configurations",
    route: "/projects/:id/kubernetes",
    url: (ids) => `/projects/${ids.shop}/kubernetes`,
  }),
  dev({
    name: "kubernetes-new",
    caption: "Generate Kubernetes configuration",
    route: "/projects/:id/kubernetes/new",
    url: (ids) => `/projects/${ids.shop}/kubernetes/new`,
  }),
  dev({ name: "account", caption: "Account", route: "/account" }),
  dev({
    name: "notifications",
    caption: "Notifications",
    route: "/notifications",
  }),
  dev({ name: "help", caption: "Help", route: "/help" }),

  admin({
    name: "overview",
    caption: "Overview, with every workspace",
    route: "/",
  }),
  admin({
    name: "administration",
    caption: "Administration overview",
    route: "/admin",
  }),
  admin({ name: "teams", caption: "Teams", route: "/admin/teams" }),
  admin({ name: "users", caption: "Users & roles", route: "/admin/users" }),
  admin({
    name: "identity",
    caption: "Identity providers",
    route: "/admin/identity",
  }),
  admin({
    name: "workers",
    caption: "Nodes & workers",
    route: "/admin/workers",
  }),
  admin({ name: "servers", caption: "Servers", route: "/admin/servers" }),
  admin({
    name: "capacity",
    caption: "Capacity & activity",
    route: "/admin/capacity",
  }),
  admin({ name: "engines", caption: "Build engines", route: "/admin/engines" }),
  admin({
    name: "providers",
    caption: "Build & conversion providers",
    route: "/admin/providers",
  }),
  admin({ name: "queue", caption: "Build queue", route: "/admin/queue" }),
  admin({
    name: "registries",
    caption: "Registries",
    route: "/admin/registries",
  }),
  admin({
    name: "recipes",
    caption: "Recipes & base images",
    route: "/admin/recipes",
  }),
  admin({
    name: "policies",
    caption: "Build policies",
    route: "/admin/policies",
  }),
  admin({ name: "secrets", caption: "Secrets", route: "/admin/secrets" }),
  admin({
    name: "certificates",
    caption: "Certificates",
    route: "/admin/certificates",
  }),
  admin({ name: "egress", caption: "Network egress", route: "/admin/egress" }),
  admin({
    name: "quarantines",
    caption: "Quarantined images",
    route: "/admin/quarantines",
  }),
  admin({
    name: "observability",
    caption: "Observability & Audit",
    route: "/admin/observability",
  }),
  admin({
    name: "storage",
    caption: "Artifact storage",
    route: "/admin/storage",
  }),
  admin({ name: "messaging", caption: "Messaging", route: "/admin/messaging" }),
  admin({
    name: "notification-channels",
    caption: "Notification channels",
    route: "/admin/notifications",
  }),
  admin({
    name: "federation",
    caption: "Federation",
    route: "/admin/federation",
  }),
  admin({ name: "database", caption: "Database", route: "/admin/database" }),
  admin({
    name: "backups",
    caption: "Backup & Restore",
    route: "/admin/backups",
  }),
  admin({
    name: "configuration",
    caption: "Export & import configuration",
    route: "/admin/configuration",
  }),
  admin({
    name: "upgrades",
    caption: "Upgrades & Diagnostics",
    route: "/admin/upgrades",
  }),
  admin({
    name: "about",
    caption: "About Samuori",
    route: "/",
    act: (page) => openAccountMenuItem(page, "About Samuori"),
  }),
];

/** Routes without a scene, each with the reason. */
export const notCaptured: Record<string, string> = {
  "/help/:topic":
    "help articles are long text; the Help scene shows the layout",
  "/help/api-reference": "the generated API reference, not a product screen",
  "/kube-conversions/:id":
    "needs a finished conversion, which only a worker with Move2Kube or Kompose produces",
};
