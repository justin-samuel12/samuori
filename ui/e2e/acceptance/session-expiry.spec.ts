import { expect, test, type Page } from "@playwright/test";
import { signIn } from "../helpers";

// Session expiry in a real browser (architecture/identity.md#sessions): the warning appears in every tab, and
// "Stay signed in" in one renews the idle deadline for all of them. Waits about five minutes.
const username = process.env.SAMUORI_ACCEPTANCE_ADMIN ?? "admin";
const password = process.env.SAMUORI_ACCEPTANCE_ADMIN_PASSWORD ?? "admin1";

async function csrf(page: Page) {
  const c = (await page.context().cookies()).find(
    (c) => c.name === "samuori_csrf",
  );
  return c?.value ?? "";
}

async function idleDeadline(page: Page) {
  const r = await page.request.get("/api/v1/auth/session");
  expect(r.ok()).toBeTruthy();
  return Date.parse((await r.json()).idleExpiresAt);
}

test("the idle warning appears in every tab and Stay signed in renews the session", async ({
  page,
}) => {
  test.setTimeout(12 * 60_000);
  await signIn(page, username, password);
  const settings = await page.request.get("/api/v1/identity-settings");
  const before = (await settings.json()).session;
  const token = await csrf(page);
  const set = await page.request.put("/api/v1/identity-settings", {
    headers: { "X-CSRF-Token": token },
    data: {
      session: {
        idleTimeoutMinutes: 15,
        maxSessionHours: 8,
        warningSeconds: 600,
      },
    },
  });
  expect(set.ok()).toBeTruthy();
  try {
    await page.reload();
    const other = await page.context().newPage();
    await other.goto("/");
    await expect(other.getByRole("navigation", { name: "Main" })).toBeVisible();
    const first = await idleDeadline(page);

    const warning = (p: Page) =>
      p.getByRole("dialog", { name: "Are you still there?" });
    await expect(warning(page)).toBeVisible({ timeout: 7 * 60_000 });
    await expect(warning(other)).toBeVisible({ timeout: 30_000 });

    await warning(page).getByRole("button", { name: "Stay signed in" }).click();
    await expect(warning(page)).toBeHidden({ timeout: 15_000 });
    await expect(warning(other)).toBeHidden({ timeout: 15_000 });
    expect(await idleDeadline(page)).toBeGreaterThan(first);
  } finally {
    await page.request.put("/api/v1/identity-settings", {
      headers: { "X-CSRF-Token": await csrf(page) },
      data: { session: before },
    });
  }
});
