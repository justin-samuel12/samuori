import assert from "node:assert/strict";
import { test } from "node:test";
import { isArchiveName } from "./exclude.ts";

test("dropped files are accepted only as the archive kinds the server extracts", () => {
  for (const name of ["app.zip", "app.tar", "app.tgz", "App.TAR.GZ"])
    assert.ok(isArchiveName(name), name);
  for (const name of ["app.rar", "app.7z", "server.js", "zip", "app.gz"])
    assert.ok(!isArchiveName(name), name);
});
