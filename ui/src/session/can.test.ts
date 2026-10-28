import assert from "node:assert/strict";
import { test } from "node:test";
import { allows, can, canAny, canDownload } from "./can.ts";
import { operationAccess } from "../api/generated/roles.ts";

type Scope = "organization" | "team" | "project";
const bind = (role: string, scopeType: Scope, scopeId: string) => ({
  role,
  scopeType,
  scopeId,
});
const admin = { roles: [bind("administrator", "organization", "o1")] };
const dev = { roles: [bind("developer", "team", "t1")] };
const projectMaintainer = { roles: [bind("maintainer", "project", "p2")] };

test("public and any-role operations are open to every signed-in user", () => {
  assert.equal(allows("public", []), true);
  assert.equal(allows("any", []), true);
  assert.equal(can({ roles: [] }, "listTeams"), true);
});

test("the coarse check needs one of the declared roles at any scope", () => {
  assert.equal(can(dev, "createBuild"), true);
  assert.equal(
    can(admin, "createBuild"),
    false,
    "an administrator is not a builder",
  );
  assert.equal(can(admin, "createTeam"), true);
  assert.equal(can(dev, "createTeam"), false);
  assert.equal(allows([], admin.roles), false);
});

test("a known scope narrows the check as authz.Can does", () => {
  assert.equal(
    can(dev, "createBuild", { teamId: "t1", projectId: "p1" }),
    true,
  );
  assert.equal(
    can(dev, "createBuild", { teamId: "t2", projectId: "p3" }),
    false,
  );
  assert.equal(
    can(projectMaintainer, "createSpec", { teamId: "t1", projectId: "p2" }),
    true,
  );
  assert.equal(
    can(projectMaintainer, "createSpec", { teamId: "t1", projectId: "p1" }),
    false,
  );
  assert.equal(can(projectMaintainer, "createProject"), true);
  assert.equal(
    can(projectMaintainer, "createProject", { teamId: "t1" }),
    false,
    "a project maintainer cannot create projects in its team",
  );
  assert.equal(can(dev, "createProject", { teamId: "t1" }), false);
  const orgDev = { roles: [bind("developer", "organization", "o1")] };
  assert.equal(can(orgDev, "createBuild", { teamId: "t9" }), true);
});

test("canAny allows a page when one of its operations is allowed", () => {
  const reviewer = { roles: [bind("security-reviewer", "organization", "o1")] };
  assert.equal(canAny(reviewer, ["createBuild", "listBuilds"]), true);
  assert.equal(canAny(reviewer, ["createBuild", "createSpec"]), false);
});

test("the table comes from the API document", () => {
  assert.equal(operationAccess.getHealth, "public");
  assert.deepEqual(operationAccess.createBuild, ["developer", "maintainer"]);
});

test("developers download only when the organization allows it", () => {
  const dev = { roles: [bind("developer", "team", "t1")] };
  assert.equal(canDownload(dev), false);
  assert.equal(canDownload({ ...dev, developerDownloads: true }), true);
  assert.equal(
    canDownload({ roles: [bind("maintainer", "project", "p1")] }),
    true,
  );
  const reviewer = { roles: [bind("security-reviewer", "organization", "o1")] };
  assert.equal(canDownload({ ...reviewer, developerDownloads: true }), false);
});

test("an archived project offers no new work; its maintainers and administrators manage it", () => {
  const maintainer = { roles: [bind("maintainer", "team", "t1")] };
  const live = { teamId: "t1", projectId: "p1" };
  const archived = { ...live, archived: true };
  assert.equal(can(dev, "createBuild", live), true);
  assert.equal(can(dev, "createBuild", archived), false);
  assert.equal(can(dev, "createUploadSession", archived), false);
  assert.equal(can(dev, "listBuilds", archived), true);
  assert.equal(can(dev, "archiveProject", live), false);
  assert.equal(can(dev, "restoreProject", archived), false);
  assert.equal(can(maintainer, "archiveProject", live), true);
  assert.equal(can(maintainer, "archiveProject", archived), false);
  assert.equal(can(maintainer, "restoreProject", archived), true);
  assert.equal(
    can(maintainer, "archiveProject", { teamId: "t2", projectId: "p3" }),
    false,
    "another team's maintainer",
  );
  assert.equal(can(maintainer, "purgeProject", archived), false);
  assert.equal(can(admin, "purgeProject", archived), true);
});
