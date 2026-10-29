// make screenshots-check: every route in src/App.tsx has a tour scene or a reason in notCaptured.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { notCaptured, scenes } from "./scenes.ts";

const app = readFileSync(
  join(import.meta.dirname, "..", "..", "src", "App.tsx"),
  "utf8",
);
const routes = [...app.matchAll(/\bpath: "([^"]+)"/g)].map((m) => m[1]);
const shown = new Set(scenes.map((s) => s.route));
const missing = routes.filter((r) => r && !shown.has(r) && !(r in notCaptured));
const unknown = [...shown, ...Object.keys(notCaptured)].filter(
  (r) => !routes.includes(r),
);

if (routes.length === 0) throw new Error("no routes found in src/App.tsx");
if (missing.length > 0 || unknown.length > 0) {
  for (const r of missing)
    console.error(`route ${r} has no scene in e2e/tour/scenes.ts`);
  for (const r of unknown)
    console.error(`e2e/tour/scenes.ts names ${r}, which is not a route`);
  process.exit(1);
}
console.log(`screenshot tour covers ${routes.length} routes`);
