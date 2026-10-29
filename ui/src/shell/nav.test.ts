import assert from "node:assert/strict";
import { test } from "node:test";
import { landing, locate, visibleWorkspaces } from "./nav.ts";
import type { Me } from "../session/session";

const as = (role: string, scopeType = "organization"): Me =>
  ({
    id: "u1",
    username: role,
    roles: [{ role, scopeType, scopeId: "o1" }],
  }) as unknown as Me;

const hrefs = (me: Me) =>
  visibleWorkspaces(me).flatMap((w) =>
    w.groups.flatMap((g) => g.items.map((i) => i.href)),
  );

test("administration pages stay hidden from developers", () => {
  const dev = hrefs(as("maintainer", "team"));
  assert.ok(dev.includes("/projects"));
  assert.ok(dev.includes("/kubernetes"));
  assert.equal(
    dev.filter((h) => h.startsWith("/admin")).length,
    0,
    `developer sees ${dev.join(", ")}`,
  );
});

test("operators and reviewers see only the pages their routes allow", () => {
  const ops = hrefs(as("operator")).filter((h) => h.startsWith("/admin"));
  assert.deepEqual(ops.sort(), [
    "/admin/capacity",
    "/admin/engines",
    "/admin/messaging",
    "/admin/observability",
    "/admin/queue",
    "/admin/storage",
    "/admin/upgrades",
  ]);
  const rev = hrefs(as("security-reviewer")).filter((h) =>
    h.startsWith("/admin"),
  );
  assert.deepEqual(rev, ["/admin/observability"]);
});

test("the Kubernetes page needs both project and conversion access", () => {
  assert.ok(!hrefs(as("operator")).includes("/kubernetes"));
  assert.ok(hrefs(as("administrator")).includes("/kubernetes"));
});

test("paths map to their workspace and section", () => {
  const list = visibleWorkspaces(as("administrator"));
  const at = (path: string) => {
    const { workspace, item } = locate(list, path);
    return `${workspace?.id} ${item?.href}`;
  };
  assert.equal(at("/"), "overview /");
  assert.equal(at("/admin"), "admin /admin");
  assert.equal(at("/admin/teams"), "admin /admin/teams");
  assert.equal(at("/admin/registries"), "registries /admin/registries");
  assert.equal(at("/images/I1"), "builds /images");
  assert.equal(at("/admin/providers"), "workers /admin/providers");
  assert.equal(at("/projects/P1"), "projects /projects");
  assert.equal(at("/projects/P1/upload"), "projects /projects");
  assert.equal(at("/projects/P1/kubernetes/new"), "kubernetes /kubernetes");
  assert.equal(at("/kube-conversions/K1"), "kubernetes /kubernetes");
  assert.equal(at("/builds/B1"), "builds /builds");
  assert.equal(at("/nowhere"), "undefined undefined");
  const dev = visibleWorkspaces(as("developer", "team"));
  assert.equal(locate(dev, "/projects/P1/new-image").item?.href, "/new-image");
  const admin = list.find((w) => w.id === "admin");
  assert.ok(admin);
  assert.equal(landing(admin), "/admin");
});
