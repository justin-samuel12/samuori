import AxeBuilder from "@axe-core/playwright";
import { expect, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

export const admin = {
  username: "admin",
  password: "correct horse battery staple",
};

export function bootstrapToken(): string {
  return readFileSync(
    join(process.env.SAMUORI_E2E_DATA ?? "", "state", "bootstrap-token"),
    "utf8",
  ).trim();
}

/** Fails on WCAG 2.2 A/AA violations detectable by axe on the current page state. */
export async function expectAccessible(page: Page) {
  const result = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .analyze();
  expect(
    result.violations.map(
      (v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`,
    ),
  ).toEqual([]);
}

/** Collects console errors, including Content-Security-Policy violations. */
export function watchConsole(page: Page): string[] {
  const errors: string[] = [];
  page.on("console", (m) => {
    if (
      m.type() === "error" &&
      !/^Failed to load resource: the server responded with a status of 4\d\d/.test(
        m.text(),
      )
    )
      errors.push(m.text());
  });
  page.on("pageerror", (e) => errors.push(e.message));
  return errors;
}

export async function signIn(page: Page, username: string, password: string) {
  await page.goto("/");
  await page.getByLabel("Username").fill(username);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
}

export async function chooseOption(
  page: Page,
  label: string,
  option: string | RegExp,
) {
  await page.getByRole("button", { name: new RegExp(label) }).click();
  await page.getByRole("option", { name: option }).click();
}

export function nav(page: Page) {
  return page.getByRole("navigation", { name: "Main" });
}

/** Opens a page by its navigation label through the search palette, which lists every page the user may open. */
export async function goTo(page: Page, label: string) {
  await page.keyboard.press("ControlOrMeta+k");
  const palette = page.getByRole("dialog", { name: "Search and jump to" });
  await palette.getByLabel("Search pages, projects and guides").fill(label);
  const escaped = label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  await palette
    .getByRole("menuitem", { name: new RegExp(`^${escaped}(,|$)`) })
    .first()
    .click();
}

/** The "orders" project that 02-upload.spec.ts creates. */
export async function ordersProject(page: Page): Promise<string> {
  const projects = await (
    await page.request.get("/api/v1/projects?limit=100")
  ).json();
  return projects.items.find((p: { name: string }) => p.name === "orders").id;
}
