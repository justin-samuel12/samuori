// Captures every scene in light and dark (make screenshots; docs/screenshots/README.md).
import { expect, test, type Browser, type Page } from "@playwright/test";
import { readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { watchConsole } from "../helpers";
import { scenes, type Audience, type Scene } from "./scenes.ts";
import { seed, signInCookies, type Seeded, type User } from "./seed.ts";

const out = join(import.meta.dirname, "..", "..", "..", "docs", "screenshots");
const base = process.env.SAMUORI_TOUR_URL ?? "";
const schemes = ["light", "dark"] as const;
const file = (s: Scene, scheme: string) =>
  `${s.audience}-${s.name}-${scheme}.png`;

test.describe.configure({ mode: "serial" });

let ids: Seeded;
const sessions = new Map<
  User,
  { name: string; value: string; url: string }[]
>();
test.beforeAll(async () => {
  ids = await seed(base, process.env.SAMUORI_TOUR_DATA ?? "");
  for (const u of ["dana", "admin"] as const)
    sessions.set(
      u,
      (await signInCookies(base, u)).map((c) => ({ ...c, url: base })),
    );
});

function writeIfChanged(path: string, data: Buffer) {
  let old: Buffer | undefined;
  try {
    old = readFileSync(path);
  } catch {
    old = undefined;
  }
  if (!old?.equals(data)) writeFileSync(path, data);
}

const headings: Record<Audience, string> = {
  signin: "Signing in",
  developer: "For developers",
  admin: "For administrators",
};

function gallery(): string {
  const parts = [
    "# Screenshots",
    "",
    "Every screen of the web UI, with example data on a local install. `make screenshots` regenerates the images",
    "and this page from `ui/e2e/tour/scenes.ts` ([local-dev.md](../developers/local-dev.md#screenshots)).",
    "Each image links to its dark version.",
  ];
  for (const audience of Object.keys(headings) as Audience[]) {
    parts.push("", `## ${headings[audience]}`);
    for (const s of scenes.filter((x) => x.audience === audience))
      parts.push(
        "",
        `### ${s.caption}`,
        "",
        `[<img src="${file(s, "light")}" alt="${s.caption}" width="720">](${file(s, "dark")})`,
      );
  }
  return parts.join("\n") + "\n";
}

async function open(browser: Browser, s: Scene, scheme: string) {
  const context = await browser.newContext({
    baseURL: base,
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: 1,
    colorScheme: scheme as "light" | "dark",
    reducedMotion: "reduce",
  });
  if (s.user) await context.addCookies(sessions.get(s.user) ?? []);
  return context;
}

async function settle(page: Page) {
  await expect(page.locator(".samuori-page-spinner")).toHaveCount(0);
  await expect(
    page.getByRole("status").filter({ hasText: /^Loading/ }),
  ).toHaveCount(0);
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(400);
}

for (const s of scenes)
  for (const scheme of schemes)
    test(file(s, scheme), async ({ browser }) => {
      const context = await open(browser, s, scheme);
      const page = await context.newPage();
      const errors = watchConsole(page);
      await page.goto(s.url ? s.url(ids) : s.route);
      await expect(page.locator("main").first()).toBeVisible();
      await settle(page);
      if (s.act) {
        await s.act(page);
        await settle(page);
      }
      if (!s.showsError)
        await expect(page.locator(".samuori-alert--error")).toHaveCount(0);
      const png = await page.screenshot({
        animations: "disabled",
        caret: "hide",
        fullPage: s.fullPage,
      });
      writeIfChanged(join(out, file(s, scheme)), png);
      await context.close();
      expect(errors).toEqual([]);
    });

test.afterAll(() => {
  const wanted = new Set(scenes.flatMap((s) => schemes.map((c) => file(s, c))));
  for (const f of readdirSync(out))
    if (f.endsWith(".png") && !wanted.has(f)) rmSync(join(out, f));
  writeIfChanged(join(out, "README.md"), Buffer.from(gallery()));
});
