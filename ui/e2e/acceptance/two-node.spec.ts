// One controller and one remote worker on separate hosts (docs/operators/worker-identity.md#add-a-remote-worker).
// SAMUORI_ACCEPTANCE_SHELL runs commands on the controller host and SAMUORI_ACCEPTANCE_WORKER_SHELL on the worker host, for
// example "limactl shell samuori-clean" and "limactl shell samuori-remote". The controller is installed with --no-local-worker.
import { expect, test, type Page } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { join } from "node:path";
import {
  chooseOption,
  expectAccessible,
  goTo,
  signIn,
  watchConsole,
} from "../helpers";

test.describe.configure({ mode: "serial" });
test.skip(
  !process.env.SAMUORI_ACCEPTANCE_WORKER_SHELL,
  "needs a remote worker host (make test-acceptance-two-node)",
);

const admin = {
  username: "admin",
  password:
    process.env.SAMUORI_ACCEPTANCE_ADMIN_PASSWORD ??
    "correct horse battery staple",
};
const dev = {
  username: "dana",
  password:
    process.env.SAMUORI_ACCEPTANCE_DEV_PASSWORD ??
    "acceptance developer passphrase",
};
const fixture = join(
  import.meta.dirname,
  "..",
  "..",
  "..",
  "examples",
  "orders-api",
);

function on(variable: string, command: string, input?: string): string {
  const [bin, ...args] = (process.env[variable] ?? "")
    .split(" ")
    .filter(Boolean);
  if (!bin) throw new Error(`set ${variable}`);
  return execFileSync(bin, [...args, "sh", "-c", command], {
    input,
    encoding: "utf8",
  });
}

const onController = (c: string, input?: string) =>
  on("SAMUORI_ACCEPTANCE_SHELL", c, input);
const onWorker = (c: string, input?: string) =>
  on("SAMUORI_ACCEPTANCE_WORKER_SHELL", c, input);

async function until(
  page: Page,
  check: () => Promise<boolean>,
  minutes: number,
  what: string,
) {
  const deadline = Date.now() + minutes * 60_000;
  while (Date.now() < deadline) {
    if (await check()) return;
    await page.waitForTimeout(3000);
    await page.reload();
  }
  throw new Error(`timed out waiting for ${what}`);
}

test("the administrator sets up the controller and adds a developer", async ({
  page,
}) => {
  const errors = watchConsole(page);
  await page.goto("/");
  await page
    .getByLabel("Setup code")
    .fill(onController("sudo samuori-setup-code").trim());
  await page.getByLabel("Organization name").fill("Two Node Org");
  await page.getByLabel("Administrator username").fill(admin.username);
  await page.getByLabel("Display name").fill("Avery Admin");
  await page.getByLabel("Password", { exact: true }).fill(admin.password);
  await page.getByLabel("Confirm password").fill(admin.password);
  await page.getByRole("button", { name: "Create organization" }).click();
  await expect(page.getByText("Setup is complete")).toBeVisible();

  await signIn(page, admin.username, admin.password);
  await goTo(page, "Teams");
  await page.getByRole("button", { name: "New team" }).click();
  await page.getByLabel("Team name").fill("payments");
  await page.getByRole("button", { name: "Create team" }).click();
  await expect(page.getByRole("rowheader", { name: "payments" })).toBeVisible();
  await goTo(page, "Users");
  await page.getByRole("button", { name: "New user" }).click();
  await page.getByLabel("Username").fill(dev.username);
  await page.getByLabel("Display name").fill("Dana Developer");
  await page.getByLabel("Initial password").fill(dev.password);
  await page.getByRole("button", { name: "Create user" }).click();
  await page
    .getByRole("button", { name: `Grant role to ${dev.username}` })
    .click();
  await chooseOption(page, "Role", "Maintainer");
  await chooseOption(page, "Applies to", "Team payments");
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Grant role" })
    .click();
  await expect(page.getByText("Maintainer — team payments")).toBeVisible();
  expect(errors).toEqual([]);
});

test("the developer creates a project and uploads the Node.js API", async ({
  page,
}) => {
  const errors = watchConsole(page);
  await signIn(page, dev.username, dev.password);
  await goTo(page, "Projects");
  await page.getByRole("button", { name: "New project" }).click();
  await page.getByLabel("Project name").fill("orders-api");
  await chooseOption(page, "Team", "payments");
  await page.getByRole("button", { name: "Create project" }).click();
  await expect(page).toHaveURL(/\/projects\/[0-9A-Z]+$/);
  await page.getByRole("button", { name: "Upload source" }).click();
  const chooser = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: "Choose folder" }).click();
  await (await chooser).setFiles(fixture);
  await page.getByRole("button", { name: "Upload and analyse" }).click();
  await expect(
    page.getByRole("heading", { name: "Uploaded source" }),
  ).toBeVisible({ timeout: 60_000 });

  expect(errors).toEqual([]);
});

test("the remote worker joins with a single-use token and passes its self-test", async ({
  page,
}) => {
  test.setTimeout(30 * 60_000);
  const errors = watchConsole(page);
  await signIn(page, admin.username, admin.password);
  await goTo(page, "Workers & pools");
  await page.getByRole("button", { name: "New pool" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("textbox", { name: "Name" }).fill("builders");
  await dialog.getByRole("textbox", { name: "Trust domain" }).fill("default");
  await chooseOption(page, "Isolation profile", /rootless-dedicated-vm/);
  await dialog.getByRole("button", { name: "Create pool" }).click();
  const pools = page.getByRole("grid", { name: "Worker pools" });
  await expect(pools).toContainText("builders");

  await page.getByRole("button", { name: "Enroll a worker" }).click();
  const command = (await page.getByTestId("enroll-command").innerText()).trim();
  const pin = command.split(" ").pop() ?? "";
  expect(pin).toMatch(/^sha256:/);
  // The worker VM holds only the release folder: samuori-join installs and enrolls it in one step (ADR 0045).
  const join = (
    await page.getByTestId("enroll-join-command").innerText()
  ).trim();
  expect(join).toMatch(/^sudo \.\/samuori-join worker samuoriwj_\S+$/);
  onWorker(`cd ~/samuori-release && ${join} --confirm-dedicated-vm`);

  const workers = page.getByRole("grid", { name: "Workers" });
  await until(
    page,
    async () => /enrolled, active/.test(await workers.innerText()),
    3,
    "the remote worker to enroll",
  );
  // A worker joins unassigned; assigning it to the pool, which its creation approved, starts its self-test.
  await expect(workers).toContainText("Unassigned");
  await workers.getByRole("button", { name: /^Assign .* to a pool$/ }).click();
  await chooseOption(page, "Pool$", /builders/);
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Assign to pool" })
    .click();
  const pool = pools.getByRole("row", { name: /builders/ });
  await expect(pool).toContainText("Yes");
  await expect(workers).not.toContainText("Unassigned");
  await until(
    page,
    async () =>
      (await pool.innerText()).includes("1 of 1") ||
      /\bfailed(: |$)/m.test(await workers.innerText()),
    20,
    "the remote worker self-test",
  );
  await expect(pool).toContainText("1 of 1");
  await expect(workers).not.toContainText("local");
  await expectAccessible(page);
  expect(errors).toEqual([]);
});

test("the administrator adds the controller's registry as a destination", async ({
  page,
}) => {
  const errors = watchConsole(page);
  await signIn(page, admin.username, admin.password);
  await goTo(page, "Registries & destinations");
  await page.getByRole("button", { name: "Add registry" }).click();
  await page.getByLabel("Name", { exact: true }).fill("local");
  await page.getByLabel("Endpoint").fill("127.0.0.1:5001");
  await page.locator("label", { hasText: "Plain HTTP" }).click();
  await page
    .locator("label", { hasText: "The registry is on a private network" })
    .click();
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect(page.getByText("Added registry local.")).toBeVisible();
  await page.getByRole("button", { name: "Add credentials" }).click();
  await page.getByLabel("Name", { exact: true }).fill("local-push");
  await chooseOption(page, "Registry", "local");
  await chooseOption(page, "Authentication", "None (development registries)");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await page.getByRole("button", { name: "Add destination" }).click();
  await page.getByLabel("Name", { exact: true }).fill("local-orders");
  await chooseOption(page, "Push credentials", "local-push (local)");
  await page.getByLabel("Repository").fill("payments/orders-api");
  await page.locator("label", { hasText: "orders-api" }).click();
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect(page.getByText("Added destination local-orders.")).toBeVisible();
  expect(errors).toEqual([]);
});

test("the developer's build runs on the remote worker, validates and publishes", async ({
  page,
}) => {
  test.setTimeout(45 * 60_000);
  const errors = watchConsole(page);
  await signIn(page, dev.username, dev.password);
  const projects = await (
    await page.request.get("/api/v1/projects?limit=100")
  ).json();
  const project = projects.items.find(
    (p: { name: string }) => p.name === "orders-api",
  );
  const projectURL = `/projects/${project.id}`;
  await page.goto(`${projectURL}/new-image`);
  await page.getByRole("button", { name: "Next" }).click();
  await page.getByText(/Express · javascript/).click();
  await page.getByRole("button", { name: "Next" }).click();
  await page.getByLabel("Port the app listens on").fill("3000");
  for (let i = 0; i < 4; i++)
    await page.getByRole("button", { name: "Next" }).click();
  const arch = onWorker("uname -m").trim() === "aarch64" ? "arm64" : "amd64";
  await page.getByRole("button", { name: "Advanced: edit as YAML" }).click();
  const editor = page.getByLabel("BuildSpec YAML");
  await editor.fill(
    (await editor.inputValue()).replace(
      "spec:\n",
      `spec:\n  target:\n    platforms: [linux/${arch}]\n`,
    ),
  );
  await page.getByRole("button", { name: "Apply YAML changes" }).click();
  await page.getByRole("button", { name: "Back to the form" }).click();
  await page.getByRole("button", { name: "Build", exact: true }).click();

  const state = page.getByTestId("run-state");
  await until(
    page,
    async () => /Built|Failed|Blocked|Timed out/.test(await state.innerText()),
    30,
    "the build",
  );
  await expect(state).toHaveText("Built");
  const buildID = page.url().split("/").pop() ?? "";
  const verdict = page.getByTestId("validation-verdict");
  await until(
    page,
    async () =>
      /Passed|Failed|Blocked|Incomplete/.test(await verdict.innerText()),
    10,
    "validation",
  );
  await expect(verdict).toHaveText("Passed");

  await goTo(page, "Images");
  await chooseOption(page, "Project", "orders-api");
  await page
    .getByRole("grid", { name: "Images" })
    .getByRole("link")
    .first()
    .click();
  await page.getByRole("button", { name: "Publish image" }).click();
  await chooseOption(page, "Destination", /local-orders/);
  await page.getByLabel("Tags").fill("1.0.0");
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  const publication = page
    .getByRole("grid", { name: "Publications" })
    .getByRole("row")
    .nth(1);
  await until(
    page,
    async () => /Published|Blocked|Failed/.test(await publication.innerText()),
    10,
    "publication",
  );
  await expect(publication).toContainText("Published");
  expect(errors).toEqual([]);

  await page.context().clearCookies();
  await signIn(page, admin.username, admin.password);
  const attempts = await (
    await page.request.get(`/api/v1/builds/${buildID}/attempts`)
  ).json();
  const workers = await (await page.request.get("/api/v1/workers")).json();
  const remote = workers.items.find(
    (w: { name: string }) => w.name === onWorker("hostname -s").trim(),
  );
  expect(remote, "the remote worker is enrolled").toBeTruthy();
  expect(
    attempts.items.every((a: { workerId: string }) => a.workerId === remote.id),
  ).toBe(true);
});
