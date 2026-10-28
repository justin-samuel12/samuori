import assert from "node:assert/strict";
import { test } from "node:test";
import {
  currentPage,
  oidcStartUrl,
  renewalDue,
  renewalLeadMs,
  returnPath,
} from "./renewal.ts";

test("the renewal notice shows in the last ten minutes, or the longer idle warning", () => {
  const end = 1_000_000_000;
  assert.equal(renewalDue(end, 120_000, end - renewalLeadMs - 1), false);
  assert.equal(renewalDue(end, 120_000, end - renewalLeadMs), true);
  assert.equal(renewalDue(end, 120_000, end - 1), true);
  assert.equal(renewalDue(end, 120_000, end), false);
  assert.equal(renewalDue(end, 15 * 60_000, end - 12 * 60_000), true);
});

test("return paths stay on this site", () => {
  for (const ok of ["/", "/projects/abc?tab=builds", "/builds/b1#log"])
    assert.equal(returnPath(ok), ok);
  for (const bad of [
    "",
    "https://evil.example/",
    "//evil.example/x",
    "/\\evil.example",
    "javascript:alert(1)",
    "/api/v1/auth/logout",
    "/api",
    "/x\nLocation: https://evil.example",
  ])
    assert.equal(returnPath(bad), "/", bad);
  assert.equal(
    currentPage({ pathname: "/projects/p1", search: "?tab=specs", hash: "" }),
    "/projects/p1?tab=specs",
  );
  assert.equal(
    currentPage({ pathname: "/", search: "?signin=failed", hash: "" }),
    "/",
  );
  assert.equal(
    oidcStartUrl("prov 1", "/projects/p1?tab=specs"),
    "/api/v1/auth/oidc/prov%201/start?returnTo=%2Fprojects%2Fp1%3Ftab%3Dspecs",
  );
  assert.equal(
    oidcStartUrl("p", "https://evil.example"),
    "/api/v1/auth/oidc/p/start?returnTo=%2F",
  );
});
