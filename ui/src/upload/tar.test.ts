import assert from "node:assert/strict";
import { test } from "node:test";
import { buildTar, markExecutables } from "./tar.ts";

const mode = (header: Uint8Array) =>
  new TextDecoder().decode(header.slice(100, 107));

test("folder uploads keep scripts executable: a shebang or .sh makes a file 0755", async () => {
  const entries = await markExecutables([
    { path: "scripts/start.sh", data: new Blob(["node server.js\n"]) },
    { path: "bin/run", data: new Blob(["#!/bin/sh\nexec node .\n"]) },
    { path: "server.js", data: new Blob(["console.log(1)\n"]) },
    { path: "empty", data: new Blob([""]) },
  ]);
  assert.deepEqual(
    entries.map((e) => e.executable),
    [true, true, false, false],
  );
  const tar = new Uint8Array(await buildTar(entries).arrayBuffer());
  const modes: string[] = [];
  for (let off = 0; off + 512 <= tar.length;) {
    const h = tar.slice(off, off + 512);
    if (h.every((b) => b === 0)) break;
    const size = parseInt(new TextDecoder().decode(h.slice(124, 135)), 8);
    modes.push(mode(h));
    off += 512 + Math.ceil(size / 512) * 512;
  }
  assert.deepEqual(modes, ["0000755", "0000755", "0000644", "0000644"]);
});
