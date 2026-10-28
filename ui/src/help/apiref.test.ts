import assert from "node:assert/strict";
import { test } from "node:test";
import {
  curlExample,
  groupByTag,
  operations,
  schemaText,
  search,
  type Json,
} from "./apiref.ts";

const spec: Json = {
  tags: [{ name: "tenancy" }, { name: "system" }],
  paths: {
    "/system/health": {
      get: {
        tags: ["system"],
        operationId: "getHealth",
        "x-samuori-public": true,
        responses: { "200": { description: "ok" } },
      },
    },
    "/projects/{projectId}": {
      parameters: [{ $ref: "#/components/parameters/ProjectId" }],
      patch: {
        tags: ["tenancy"],
        operationId: "updateProject",
        description: "Renames a project.",
        "x-samuori-roles": ["maintainer", "administrator"],
        parameters: [
          {
            name: "X-CSRF-Token",
            in: "header",
            required: true,
            schema: { type: "string" },
          },
        ],
        requestBody: {
          content: {
            "application/json": {
              schema: { $ref: "#/components/schemas/Project" },
            },
          },
        },
        responses: { "404": { $ref: "#/components/responses/Problem" } },
      },
    },
  },
  components: {
    parameters: {
      ProjectId: { name: "projectId", in: "path", required: true },
    },
    responses: {
      Problem: {
        description: "Typed error",
        content: {
          "application/json": {
            schema: { $ref: "#/components/schemas/Problem" },
          },
        },
      },
    },
    schemas: {
      Problem: {
        type: "object",
        required: ["code"],
        properties: { code: { type: "string" }, message: { type: "string" } },
      },
      Project: {
        type: "object",
        required: ["name"],
        properties: {
          name: { type: "string", description: "Shown in lists" },
          parent: { $ref: "#/components/schemas/Project" },
          mode: { type: "string", enum: ["direct", "approval"] },
        },
      },
    },
  },
};

test("operations resolve shared parameters and group in tag order", () => {
  const ops = operations(spec);
  const groups = groupByTag(spec, ops);
  assert.deepEqual(
    groups.map(([tag, items]) => [tag, items.map((o) => o.id)]),
    [
      ["tenancy", ["updateProject"]],
      ["system", ["getHealth"]],
    ],
  );
  const update = ops.find((o) => o.id === "updateProject");
  assert.deepEqual(
    update?.parameters.map((p) => p.name),
    ["projectId", "X-CSRF-Token"],
  );
  assert.equal(update?.responses[0]?.description, "Typed error");
});

test("search matches every word across path, ID and roles", () => {
  const ops = operations(spec);
  assert.deepEqual(
    search(ops, "PATCH maintainer").map((o) => o.id),
    ["updateProject"],
  );
  assert.deepEqual(
    search(ops, "health project").map((o) => o.id),
    [],
  );
  assert.equal(search(ops, "  ").length, 2);
});

test("schemas render readably and stop at recursive references", () => {
  const text = schemaText(spec, { $ref: "#/components/schemas/Project" });
  assert.match(text, /name: string; {2}\/\/ Shown in lists/);
  assert.match(text, /parent\?: Project;/);
  assert.match(text, /mode\?: "direct" \| "approval";/);
});

test("curl uses a bearer token and never a CSRF header", () => {
  const update = operations(spec).find((o) => o.id === "updateProject");
  assert.ok(update);
  const curl = curlExample(update, "https://samuori.example.com");
  assert.match(curl, /Authorization: Bearer \$SAMUORI_TOKEN/);
  assert.match(
    curl,
    /"https:\/\/samuori.example.com\/api\/v1\/projects\/\$PROJECT_ID"/,
  );
  assert.doesNotMatch(curl, /CSRF/);
  const health = operations(spec).find((o) => o.id === "getHealth");
  assert.ok(health);
  assert.doesNotMatch(curlExample(health, ""), /Authorization/);
});
