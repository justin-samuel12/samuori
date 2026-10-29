import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { test } from "node:test";
import {
  accountNav,
  helpPlaces,
  helpSectionsAfter,
  helpSectionsBefore,
  items,
  locate,
  offeredHelp,
  workspaces,
} from "../shell/nav.ts";
import type { Me } from "../session/session";

const dir = new URL("../../../internal/help/content/", import.meta.url);

interface Topic {
  id: string;
  section: string;
  roles: string[];
  body: string;
}

const topics: Topic[] = readdirSync(dir)
  .filter((f) => f.endsWith(".md"))
  .map((f) => {
    const raw = readFileSync(new URL(f, dir), "utf8");
    const [, head = "", body = ""] =
      /^---\n([\s\S]*?)\n---\n([\s\S]*)$/.exec(raw) ?? [];
    const field = (k: string) =>
      new RegExp(`^${k}: (.*)$`, "m").exec(head)?.[1]?.trim() ?? "";
    return {
      id: f.slice(0, -3),
      section: field("section"),
      roles: field("roles")
        .replace(/[[\]]/g, "")
        .split(",")
        .map((r) => r.trim()),
      body,
    };
  });

const places = helpPlaces(workspaces);
const pages = workspaces.flatMap((w) =>
  items(w).map((item) => ({ workspace: w, item })),
);
const pageLabels = new Set(pages.map((p) => p.item.label));

// Sections and pages the menu no longer has.
const formerNames = [
  "Administration",
  "Registries & Recipes",
  "Recipes & base images",
  "Security & Secrets",
  "Network egress",
  "Backup & Restore",
  "Upgrades & Diagnostics",
  "Workers",
  "Registries",
  "Identity",
];

const bold = (text: string) =>
  [...text.matchAll(/\*\*([^*]+)\*\*/g)].map((m) =>
    (m[1] ?? "").replace(/\s+/g, " ").trim(),
  );

function expectNone(problems: string[]) {
  assert.deepEqual(problems, []);
}

test("every page's help topic exists and is filed under that page's section", () => {
  const problems: string[] = [];
  const ids = new Set(topics.map((t) => t.id));
  for (const [id, { workspace, item }] of places) {
    if (!ids.has(id)) problems.push(`${item.href} names missing topic ${id}`);
    const t = topics.find((x) => x.id === id);
    if (t && t.section !== workspace.label)
      problems.push(
        `${id}.md: section "${t.section}", menu "${workspace.label}"`,
      );
  }
  for (const t of topics)
    if (
      !places.has(t.id) &&
      ![...helpSectionsBefore, ...helpSectionsAfter].includes(t.section)
    )
      problems.push(
        `${t.id}.md belongs to no page, yet its section is "${t.section}"`,
      );
  expectNone(problems);
});

test("help topics name pages and sections as the menu does", () => {
  const problems: string[] = [];
  for (const t of topics) {
    const own = pages
      .filter((p) => p.item.help?.includes(t.id))
      .map(({ workspace, item }) =>
        workspace.label === item.label
          ? item.label
          : `${workspace.label} > ${item.label}`,
      );
    if (
      own.length > 0 &&
      !bold(t.body).some((b) =>
        own.some((w) => b === w || b.startsWith(`${w} >`)),
      )
    )
      problems.push(`${t.id}.md never names its page as **${own[0]}**`);
    for (const b of bold(t.body)) {
      const [first = "", second = ""] = b.split(" > ");
      if (formerNames.includes(first))
        problems.push(`${t.id}.md names the former location **${b}**`);
      if (!b.includes(" > ")) continue;
      const w = workspaces.find((x) => x.label === first);
      if (w && !items(w).some((i) => i.label === second))
        problems.push(`${t.id}.md: **${b}**: ${first} has no page ${second}`);
      if (!w && !pageLabels.has(first))
        problems.push(`${t.id}.md: **${b}** starts with no section or page`);
    }
    for (const m of t.body.matchAll(/\]\((\/[^)\s]*)\)/g)) {
      const path = m[1] ?? "";
      if (
        path !== "/" &&
        !accountNav.some((i) => i.href === path) &&
        !locate(workspaces, path).item
      )
        problems.push(`${t.id}.md links to ${path}, which is not in the menu`);
    }
  }
  expectNone(problems);
});

const person = (role: string): Me =>
  ({
    id: "u1",
    username: role,
    roles: [
      {
        role,
        scopeType: ["developer", "maintainer", "publisher"].includes(role)
          ? "team"
          : "organization",
        scopeId: "o1",
      },
    ],
  }) as unknown as Me;

test("a topic is for the roles that can see one of its pages", () => {
  const problems: string[] = [];
  for (const t of topics)
    if (places.has(t.id))
      for (const role of t.roles)
        if (offeredHelp(person(role), [t]).length !== 1)
          problems.push(
            `${t.id}.md is for ${role}, who sees none of its pages`,
          );
  expectNone(problems);

  const dev = offeredHelp(person("developer"), topics).map((t) => t.id);
  assert.ok(dev.includes("new-image"));
  assert.ok(dev.includes("getting-started"));
  assert.ok(!dev.includes("workers"));
  const op = offeredHelp(person("operator"), topics);
  assert.equal(op.find((t) => t.id === "workers")?.section, "Infrastructure");
  assert.equal(op[0]?.id, "getting-started");
});
