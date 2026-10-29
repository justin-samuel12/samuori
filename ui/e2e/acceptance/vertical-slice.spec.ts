// Vertical slice acceptance (WP23, acceptance criteria 1, 2 and 8) on a freshly installed all-in-one host.
// SAMUORI_ACCEPTANCE_SHELL runs commands on that host, for example "limactl shell samuori-clean".
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

const admin = { username: "admin", password: "correct horse battery staple" };
const dev = { username: "dana", password: "acceptance developer passphrase" };
// The registry endpoint as samuori-server reaches it can differ per install shape.
const registryEndpoint =
  process.env.SAMUORI_ACCEPTANCE_REGISTRY ?? "127.0.0.1:5001";
const fixture = join(
  import.meta.dirname,
  "..",
  "..",
  "..",
  "examples",
  "orders-api",
);

function onHost(command: string, input?: string): string {
  const shell = (process.env.SAMUORI_ACCEPTANCE_SHELL ?? "")
    .split(" ")
    .filter(Boolean);
  const [bin, ...args] = shell;
  if (!bin) throw new Error("set SAMUORI_ACCEPTANCE_SHELL");
  return execFileSync(bin, [...args, "sh", "-c", command], {
    input,
    encoding: "utf8",
  });
}

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

test("the administrator completes first-run setup and adds a developer", async ({
  page,
}) => {
  const errors = watchConsole(page);
  await page.goto("/");
  await page
    .getByLabel("Setup code")
    .fill(onHost("sudo samuori-setup-code").trim());
  await page.getByLabel("Organization name").fill("Acceptance Org");
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

test("the developer uploads a Node.js API; it cannot build before a pool is verified", async ({
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
  await expect(
    page.getByRole("article", { name: /javascript in \./ }),
  ).toContainText("Express");
  await expectAccessible(page);

  await reviewSpec(page);
  await page.getByRole("button", { name: "Check if it can build" }).click();
  await expect(page.getByText(/cannot be built yet/)).toBeVisible();
  await expect(page.getByText(/No verified worker can build/)).toBeVisible();
  await page.getByRole("button", { name: "Build", exact: true }).click();
  await expect(page.getByTestId("run-state")).toHaveText("Blocked");
  expect(errors).toEqual([]);
});

// Builds for the host architecture; a spec without a target builds linux/amd64.
async function reviewSpec(page: Page) {
  const projects = await (
    await page.request.get("/api/v1/projects?limit=100")
  ).json();
  const project = projects.items.find(
    (p: { name: string }) => p.name === "orders-api",
  );
  await page.goto(`/projects/${project.id}/new-image`);
  await expect(
    page.getByRole("heading", { name: "Step 1: Upload source" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Next" }).click();
  await page.getByText(/Express · javascript/).click();
  await page.getByRole("button", { name: "Next" }).click();
  await expect(page.getByLabel("Image name")).toHaveValue("orders-api");
  await page.getByLabel("Port the app listens on").fill("3000");
  for (let i = 0; i < 4; i++)
    await page.getByRole("button", { name: "Next" }).click();
  await expect(
    page.getByRole("heading", { name: "Step 7: Review & build" }),
  ).toBeVisible();
  const arch = onHost("uname -m").trim() === "aarch64" ? "arm64" : "amd64";
  await page.getByRole("button", { name: "Advanced: edit as YAML" }).click();
  const editor = page.getByLabel("BuildSpec YAML");
  const text = await editor.inputValue();
  expect(text).not.toContain("target:");
  await editor.fill(
    text.replace(
      "spec:\n",
      `spec:\n  target:\n    platforms: [linux/${arch}]\n`,
    ),
  );
  await expect(page.getByRole("status").getByText("Valid")).toBeVisible();
  await page.getByRole("button", { name: "Apply YAML changes" }).click();
  await page.getByRole("button", { name: "Back to the form" }).click();
  await expect(
    page.getByRole("heading", { name: "Step 7: Review & build" }),
  ).toBeVisible();
}

test("the local worker enrolls by itself; the administrator enables its pool and it passes its self-test", async ({
  page,
}) => {
  test.setTimeout(30 * 60_000);
  const errors = watchConsole(page);
  await signIn(page, admin.username, admin.password);
  await goTo(page, "Workers & pools");
  await until(
    page,
    async () =>
      /local\s*rootless-dedicated-vm/.test(
        await page.getByRole("grid", { name: "Worker pools" }).innerText(),
      ),
    3,
    "the controller to create the local pool",
  );
  const workers = page.getByRole("grid", { name: "Workers" });
  await until(
    page,
    async () => /enrolled, active/.test(await workers.innerText()),
    3,
    "the local worker to enroll by itself",
  );
  await page.getByRole("button", { name: "Enable local" }).click();
  await page.getByLabel("Reason").fill("Dedicated all-in-one acceptance VM");
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Enable pool" })
    .click();
  const pool = page
    .getByRole("grid", { name: "Worker pools" })
    .getByRole("row", { name: /local/ });
  await expect(pool).toContainText("Yes");
  // Enabling starts the worker's own self-test (OD-22); the pool counts verified workers.
  const worker = workers.getByRole("row").nth(1);
  await until(
    page,
    async () =>
      (await pool.innerText()).includes("1 of 1") ||
      /\bfailed(: |$)/m.test(await worker.innerText()),
    20,
    "the worker self-test",
  );
  await expect(pool).toContainText("1 of 1");
  await expect(workers).toContainText("Yes");
  await expectAccessible(page);
  expect(errors).toEqual([]);
});

test("the administrator adds the local registry as a destination", async ({
  page,
}) => {
  const errors = watchConsole(page);
  await signIn(page, admin.username, admin.password);
  await goTo(page, "Registries & destinations");
  await page.getByRole("button", { name: "Add registry" }).click();
  await page.getByLabel("Name", { exact: true }).fill("local");
  await page.getByLabel("Endpoint").fill(registryEndpoint);
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
  await page.getByRole("button", { name: "Test local-push" }).click();
  await expect(page.getByRole("row", { name: /local-push/ })).toContainText(
    /ok|passed|succeeded/i,
  );
  await page.getByLabel("New key name").fill("release");
  await page.getByRole("button", { name: "Create signing key" }).click();
  await expect(page.getByRole("grid", { name: "Signing keys" })).toContainText(
    "active",
  );
  await page.getByRole("button", { name: "Add destination" }).click();
  await page.getByLabel("Name", { exact: true }).fill("local-orders");
  await chooseOption(page, "Push credentials", "local-push (local)");
  await page.getByLabel("Repository").fill("payments/orders-api");
  await page.locator("label", { hasText: "orders-api" }).click();
  await page
    .locator("label", { hasText: "Sign images with the organization" })
    .click();
  await page
    .locator("label", { hasText: "Attach signed build provenance" })
    .click();
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect(page.getByText("Added destination local-orders.")).toBeVisible();
  expect(errors).toEqual([]);
});

test("the developer builds, validates and publishes the image using only the browser", async ({
  page,
}) => {
  test.setTimeout(45 * 60_000);
  const errors = watchConsole(page);
  await signIn(page, dev.username, dev.password);
  await goTo(page, "Build runs");
  await page
    .getByRole("grid", { name: "Build runs" })
    .getByRole("link")
    .first()
    .click();
  await expect(page.getByTestId("run-state")).toHaveText("Blocked");
  await page.getByRole("button", { name: "Build again" }).click();
  await expect(page.getByTestId("run-state")).not.toHaveText("Blocked");
  const state = page.getByTestId("run-state");
  await until(
    page,
    async () => /Built|Failed|Blocked|Timed out/.test(await state.innerText()),
    30,
    "the build",
  );
  await expect(state).toHaveText("Built");
  const verdict = page.getByTestId("validation-verdict");
  await until(
    page,
    async () =>
      /Passed|Failed|Blocked|Incomplete/.test(await verdict.innerText()),
    10,
    "validation",
  );
  await expect(verdict).toHaveText("Passed");
  await expect(page.getByRole("grid", { name: "Checks" })).toContainText(
    "runtime",
  );
  await expectAccessible(page);

  await goTo(page, "Images");
  await chooseOption(page, "Project", "orders-api");
  await expect(page.getByRole("grid", { name: "Images" })).toContainText(
    "Passed",
  );
  await expect(page.getByRole("grid", { name: "Images" })).toContainText("—");
  await page
    .getByRole("grid", { name: "Images" })
    .getByRole("link")
    .first()
    .click();
  await expect(page.locator("dd code").first()).toContainText("sha256:");
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
  await expect(publication).toContainText("Signature and provenance");
  await expect(publication).toContainText(
    /payments\/orders-api@sha256:[0-9a-f]{64}/,
  );
  const ref = (await publication.innerText()).match(
    /payments\/orders-api@(sha256:[0-9a-f]{64})/,
  );
  expect(ref).toBeTruthy();
  const published = ref?.[1] ?? "";
  await expectAccessible(page);

  const tags = JSON.parse(
    onHost("curl -fsS http://127.0.0.1:5001/v2/payments/orders-api/tags/list"),
  );
  expect(tags.tags).toContain("1.0.0");
  const cosignTag = published.replace(":", "-");
  expect(tags.tags).toContain(`${cosignTag}.sig`);
  expect(tags.tags).toContain(`${cosignTag}.att`);
  const head = onHost(
    "curl -fsSI -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json' http://127.0.0.1:5001/v2/payments/orders-api/manifests/1.0.0",
  );
  expect(head.toLowerCase()).toContain(`docker-content-digest: ${published}`);

  await page
    .getByRole("button", { name: /^Deprecate \S*payments\/orders-api@/ })
    .click({ timeout: 30_000 });
  await page.getByLabel("Reason").fill("superseded by the next release");
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Deprecate" })
    .click();
  await expect(publication).toContainText("Deprecated");
  await page
    .getByRole("button", { name: /^Retire \S*payments\/orders-api@/ })
    .click({ timeout: 30_000 });
  await page.getByLabel("Reason").fill("end of support for 1.0.0");
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Retire" })
    .click();
  await expect(publication).toContainText("Retired");
  await expect(page.getByRole("button", { name: /^Promote from/ })).toHaveCount(
    0,
  );
  expect(
    JSON.parse(
      onHost(
        "curl -fsS http://127.0.0.1:5001/v2/payments/orders-api/tags/list",
      ),
    ).tags,
  ).toContain("1.0.0");
  await expectAccessible(page);
  expect(errors).toEqual([]);
});
