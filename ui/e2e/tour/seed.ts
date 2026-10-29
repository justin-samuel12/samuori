// Example data for the screenshot tour (docs/screenshots/README.md). Everything the public API can create goes
// through it; what only a worker can produce (finished builds, logs, validation, publication) is written straight
// into the tour server's own throwaway SQLite database, the way the API tests seed it.
import { createHash, randomBytes } from "node:crypto";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import createClient from "openapi-fetch";
import type { paths } from "../../src/api/generated/schema";
import { buildTar } from "../../src/upload/tar.ts";

export const users = {
  admin: { username: "admin", password: "admin1" },
  dana: { username: "dana", password: "dana1" },
};
export type User = keyof typeof users;

type Client = ReturnType<typeof createClient<paths>>;

export async function signInCookies(base: string, who: User) {
  const r = await fetch(`${base}/api/v1/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(users[who]),
  });
  if (r.status !== 204) throw new Error(`sign-in as ${who}: ${r.status}`);
  return r.headers.getSetCookie().map((c) => {
    const [pair = ""] = c.split(";");
    const eq = pair.indexOf("=");
    return { name: pair.slice(0, eq), value: pair.slice(eq + 1) };
  });
}

async function client(base: string, who: User): Promise<Client> {
  const cookies = await signInCookies(base, who);
  return createClient<paths>({
    baseUrl: `${base}/api/v1`,
    headers: {
      Cookie: cookies.map((k) => `${k.name}=${k.value}`).join("; "),
      "X-CSRF-Token": cookies.find((k) => k.name === "samuori_csrf")?.value,
    },
  });
}

function must<T>(what: string, r: { data?: T; error?: unknown }): T {
  if (r.data === undefined)
    throw new Error(`${what}: ${JSON.stringify(r.error)}`);
  return r.data;
}

const key = () => crypto.randomUUID();
const sha256 = (data: string) =>
  `sha256:${createHash("sha256").update(data).digest("hex")}`;

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
function newId(): string {
  let t = Date.now();
  let out = "";
  for (let i = 0; i < 10; i++, t = Math.floor(t / 32))
    out = (crockford[t % 32] ?? "0") + out;
  for (const b of randomBytes(16)) out += crockford[b % 32];
  return out;
}

// The format the Go driver writes, so the server parses and orders these timestamps like its own.
function ts(minutesAgo: number): string {
  const d = new Date(Date.now() - minutesAgo * 60_000).toISOString();
  return `${d.slice(0, 10)} ${d.slice(11, 23)} +0000 UTC`;
}

async function upload(
  c: Client,
  projectId: string,
  files: Record<string, string>,
): Promise<string> {
  const archive = buildTar(
    Object.entries(files).map(([path, text]) => ({
      path,
      data: new Blob([text]),
    })),
  );
  const s = must(
    "upload",
    await c.POST("/projects/{projectId}/uploads", {
      params: { path: { projectId } },
      body: {
        fileCount: Object.keys(files).length,
        totalBytes: archive.size,
        excludePatterns: [],
      },
    }),
  );
  for (let i = 0; i < s.maxChunks; i++) {
    const part = archive.slice(i * s.chunkBytes, (i + 1) * s.chunkBytes);
    const chunk = await c.PUT("/uploads/{uploadId}/chunks/{index}", {
      params: { path: { uploadId: s.id, index: i } },
      body: "",
      bodySerializer: () => part,
      headers: { "Content-Type": "application/octet-stream" },
    });
    if (chunk.response.status !== 204)
      throw new Error(`upload chunk: ${chunk.response.status}`);
  }
  let op = must(
    "commit",
    await c.POST("/uploads/{uploadId}/commit", {
      params: {
        path: { uploadId: s.id },
        header: { "Idempotency-Key": key() },
      },
    }),
  );
  while (op.status !== "succeeded" && op.status !== "failed") {
    await new Promise((r) => setTimeout(r, 200));
    op = must(
      "operation",
      await c.GET("/operations/{operationId}", {
        params: { path: { operationId: op.id } },
      }),
    );
  }
  if (op.status !== "succeeded" || !op.resourceId)
    throw new Error(`upload failed: ${JSON.stringify(op)}`);
  return op.resourceId;
}

const shopSource = {
  "package.json": JSON.stringify(
    {
      name: "shop",
      version: "1.4.0",
      private: true,
      scripts: { start: "node server.js" },
      dependencies: { express: "^5.1.0" },
      engines: { node: ">=22" },
    },
    null,
    2,
  ),
  "server.js": `const express = require("express");\nconst app = express();\napp.get("/", (req, res) => res.send("shop"));\napp.listen(3000);\n`,
};

const apiSource = {
  "requirements.txt": "fastapi==0.115.0\nuvicorn==0.30.6\n",
  "main.py": `from fastapi import FastAPI\n\napp = FastAPI()\n\n\n@app.get("/health")\ndef health():\n    return {"ok": True}\n`,
};

function nodeSpec(name: string) {
  return {
    apiVersion: "samuori.invalid/v1alpha1",
    kind: "ImageBuild",
    metadata: { name },
    spec: {
      application: {
        type: "http-service",
        runtime: { language: "nodejs", versionConstraint: ">=22" },
        port: 3000,
      },
      build: {
        method: "recipe",
        recipe: {
          id: "node-service",
          version: "1.0.0",
          install: "npm ci",
          start: ["node", "server.js"],
        },
      },
    },
  };
}

function pythonSpec(name: string) {
  return {
    apiVersion: "samuori.invalid/v1alpha1",
    kind: "ImageBuild",
    metadata: { name },
    spec: {
      application: {
        type: "http-service",
        runtime: { language: "python", versionConstraint: "3.13" },
        port: 8000,
      },
      build: {
        method: "recipe",
        recipe: {
          id: "python-service",
          version: "1.0.0",
          start: ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"],
        },
      },
    },
  };
}

function finding(language: string, framework: string, path: string) {
  return {
    findings: [
      {
        detectorId: language,
        detectorVersion: "1.0.0",
        sourceSubpath: ".",
        language,
        framework,
        applicationKind: "http-service",
        confidence: "high",
        evidence: [
          {
            kind: "manifest",
            path,
            observation: `${framework} is a dependency`,
            weight: 1,
          },
        ],
      },
    ],
    dockerfiles: [],
    warnings: [],
  };
}

const shopLog = `#1 [internal] load build definition
#1 DONE 0.0s
#2 [build 1/5] FROM docker.io/library/node:22-bookworm-slim
#2 DONE 1.8s
#3 [build 2/5] WORKDIR /app
#3 DONE 0.1s
#4 [build 3/5] COPY . .
#4 DONE 0.1s
#5 [build 4/5] RUN npm ci
#5 1.912 added 66 packages, and audited 67 packages in 2s
#5 1.913 found 0 vulnerabilities
#5 DONE 2.4s
#6 [run 1/3] COPY --from=build --chown=10001:10001 /app /app
#6 DONE 0.2s
#7 exporting to oci image format
#7 exporting layers 0.6s done
#7 DONE 0.9s
`;

const apiLog = `#1 [internal] load build definition
#1 DONE 0.0s
#2 [build 1/4] FROM docker.io/library/python:3.13-slim-bookworm
#2 DONE 1.2s
#3 [build 3/4] RUN pip install -r requirements.txt
#3 2.104 ERROR: Could not find a version that satisfies the requirement fastapi==0.115.0
#3 2.105 ERROR: No matching distribution found for fastapi==0.115.0
#3 ERROR: process "/bin/sh -c pip install -r requirements.txt" did not complete successfully: exit code: 1
`;

export interface Seeded {
  shop: string;
  snapshot: string;
  succeeded: string;
  running: string;
  failed: string;
  image: string;
}

export async function seed(base: string, dataDir: string): Promise<Seeded> {
  if (!existsSync(join(dataDir, ".samuori-tour")))
    throw new Error(
      `${dataDir} is not a screenshot-tour data directory; refusing to write to it`,
    );

  const admin = await client(base, "admin");
  const renamed = await admin.PATCH("/me", {
    body: { displayName: "Alex Admin" },
  });
  if (!renamed.response.ok)
    throw new Error(`admin name: ${renamed.response.status}`);
  const web = must(
    "team web",
    await admin.POST("/teams", { body: { name: "web" } }),
  );
  must(
    "team platform",
    await admin.POST("/teams", { body: { name: "platform" } }),
  );
  const dana = must(
    "user dana",
    await admin.POST("/users", {
      body: {
        username: "dana",
        displayName: "Dana Developer",
        password: users.dana.password,
      },
    }),
  );
  must(
    "role",
    await admin.POST("/role-bindings", {
      body: {
        principalId: dana.id,
        role: "developer",
        scopeType: "team",
        scopeId: web.id,
      },
    }),
  );
  const project = async (name: string) =>
    must(
      `project ${name}`,
      await admin.POST("/projects", {
        body: { teamId: web.id, name, publicationMode: "manual" },
      }),
    );
  const shop = await project("shop");
  const api = await project("api");

  const registry = must(
    "registry",
    await admin.POST("/registries", {
      body: {
        name: "example",
        kind: "oci",
        endpoint: "registry.example.com",
        plainHttp: false,
        allowPrivateNetwork: false,
      },
    }),
  );
  const push = must(
    "push profile",
    await admin.POST("/registry-credential-profiles", {
      body: {
        name: "example-push",
        registryId: registry.id,
        type: "anonymous",
        purpose: "push",
      },
    }),
  );
  const destination = must(
    "destination",
    await admin.POST("/destinations", {
      body: {
        name: "shop-releases",
        registryId: registry.id,
        repository: "web/shop",
        pushProfileId: push.id,
        acceptsRestricted: false,
        allowedProjectIds: [shop.id],
        immutableTags: true,
        requiresApproval: false,
        signImages: false,
        attachProvenance: false,
        promotionOnly: false,
      },
    }),
  );
  const providers = must(
    "secret providers",
    await admin.GET("/secret-providers"),
  );
  const local = providers.items.find((p) => p.type === "local");
  if (!local) throw new Error("no local secret store");
  for (const name of ["npm-token", "pypi-token"])
    must(
      `secret ${name}`,
      await admin.POST("/secret-providers/{providerId}/local-secrets", {
        params: {
          path: { providerId: local.id },
          header: { "Idempotency-Key": key() },
        },
        body: { name, type: "text", value: randomBytes(24).toString("hex") },
      }),
    );
  const pool = must(
    "pool",
    await admin.POST("/worker-pools", {
      body: {
        name: "linux-builders",
        isolationProfile: "rootless-dedicated-vm",
        trustDomain: "default",
        trustedWorkloadsOnly: false,
      },
    }),
  );

  const dev = await client(base, "dana");
  const shopOld = await upload(dev, shop.id, shopSource);
  const shopNew = await upload(dev, shop.id, {
    ...shopSource,
    "server.js": shopSource["server.js"].replace('"shop"', '"shop 1.5"'),
  });
  const apiSnap = await upload(dev, api.id, apiSource);
  const spec = async (projectId: string, document: object) =>
    must(
      "spec",
      await dev.POST("/projects/{projectId}/specs", {
        params: { path: { projectId } },
        body: { document: document as never, editorMode: "guided" },
      }),
    );
  const shopSpec = await spec(shop.id, nodeSpec("shop"));
  const apiSpec = await spec(api.id, pythonSpec("api"));
  const build = async (
    s: { specId: string; revision: number },
    snapshotId: string,
  ) =>
    must(
      "build",
      await dev.POST("/builds", {
        params: { header: { "Idempotency-Key": key() } },
        body: {
          specId: s.specId,
          specRevision: s.revision,
          snapshotId,
          cleanBuild: false,
        },
      }),
    );
  const succeeded = await build(shopSpec, shopOld);
  const failed = await build(apiSpec, apiSnap);
  const running = await build(shopSpec, shopNew);
  must(
    "base image request",
    await dev.POST("/projects/{projectId}/base-image-requests", {
      params: { path: { projectId: shop.id } },
      body: {
        repository: "docker.io/library/redis",
        justification: "Session cache for the shop checkout",
      },
    }),
  );

  const db = new DatabaseSync(join(dataDir, "samuori.db"));
  db.exec("PRAGMA busy_timeout = 5000");
  const run = (sql: string, ...args: (string | number | null)[]) =>
    db.prepare(sql).run(...args);
  run(`UPDATE controllers SET hostname = 'controller-1'`);
  const { id: adminId } = db
    .prepare("SELECT id FROM users WHERE username = 'admin'")
    .get() as { id: string };
  const { org_id: org } = db
    .prepare("SELECT org_id FROM projects WHERE id = ?")
    .get(shop.id) as { org_id: string };

  for (const [projectId, snapshotId, result] of [
    [shop.id, shopOld, finding("javascript", "Express", "package.json")],
    [shop.id, shopNew, finding("javascript", "Express", "package.json")],
    [api.id, apiSnap, finding("python", "FastAPI", "requirements.txt")],
  ] as const)
    run(
      `INSERT INTO discoveries (id, org_id, project_id, snapshot_id, status, result, sandbox, created_by, created_at, completed_at) VALUES (?, ?, ?, ?, 'completed', ?, ?, ?, ?, ?)`,
      newId(),
      org,
      projectId,
      snapshotId,
      JSON.stringify(result),
      JSON.stringify({ platform: "linux", seccomp: true, namespaces: true }),
      dana.id,
      ts(90),
      ts(90),
    );

  const workerId = newId();
  const configDigest = sha256("builder-1 configuration");
  run(
    `INSERT INTO workers (id, org_id, pool_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, last_seen_at, created_at, host_arch) VALUES (?, ?, ?, 'builder-1', 'enrolled', 'active', ?, ?, ?, ?, 0, ?, ?, 'arm64')`,
    workerId,
    org,
    pool.id,
    randomBytes(8).toString("hex"),
    ts(-60 * 24 * 30),
    JSON.stringify({
      os: "linux",
      architectures: ["arm64", "amd64"],
      engines: ["buildkit"],
      isolationProfiles: ["rootless-dedicated-vm"],
      daemonSlots: 2,
      strictEgress: true,
      configDigest,
    }),
    (await admin.GET("/me")).response.headers.get("X-Samuori-Version") ?? "",
    ts(0),
    ts(60 * 24),
  );
  run(
    `INSERT INTO pool_self_tests (id, org_id, pool_id, worker_id, status, failed_checks, started_at, finished_at, profile, config_digest) VALUES (?, ?, ?, ?, 'passed', '[]', ?, ?, 'rootless-dedicated-vm', ?)`,
    newId(),
    org,
    pool.id,
    workerId,
    ts(60 * 3),
    ts(60 * 3),
    configDigest,
  );

  const putBlob = (data: string) => {
    const digest = sha256(data);
    const hex = digest.slice(7);
    const dir = join(dataDir, "blobs", "sha256", hex.slice(0, 2));
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, hex), data);
    return { digest, size: Buffer.byteLength(data) };
  };
  const attempt = (
    runId: string,
    state: string,
    started: number,
    ended: number | null,
    extra: { failure?: string; detail?: string; digest?: string } = {},
  ) => {
    const id = newId();
    run(
      `INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, failure_class, detail, artifact_digest, cleanup, started_at, ended_at) VALUES (?, ?, 1, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      id,
      runId,
      workerId,
      pool.id,
      state,
      ts(-60),
      extra.failure ?? null,
      extra.detail ?? null,
      extra.digest ?? null,
      ended === null ? "pending" : "destroyed",
      ts(started),
      ended === null ? null : ts(ended),
    );
    return id;
  };
  const artifact = (
    runId: string,
    attemptId: string,
    kind: string,
    blob: { digest: string; size: number },
    at: number,
  ) => {
    const id = newId();
    run(
      `INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?)`,
      id,
      org,
      runId,
      attemptId,
      kind,
      blob.digest,
      blob.size,
      ts(at),
    );
    return id;
  };
  const finish = (
    runId: string,
    state: string,
    attemptId: string,
    started: number,
    ended: number | null,
    extra: { failure?: string; digest?: string; version?: string } = {},
  ) => {
    run(
      `UPDATE build_runs SET state = ?, blocked_reason = NULL, failure_class = ?, current_attempt_id = ?, attempts_used = 1, artifact_digest = ?, version = ?, isolation_profile = 'rootless-dedicated-vm', lease_worker_id = ?, lease_deadline = ?, created_at = ?, updated_at = ? WHERE id = ?`,
      state,
      extra.failure ?? null,
      attemptId,
      extra.digest ?? null,
      extra.version ?? null,
      workerId,
      ended === null ? ts(-60) : null,
      ts(started + 1),
      ts(ended ?? started),
      runId,
    );
    run(`DELETE FROM build_events WHERE run_id = ?`, runId);
    const events: [number, string][] = [
      [started + 1, "queued"],
      [started, "assigned"],
      [started - 0.2, "building"],
    ];
    if (ended !== null) events.push([ended, state]);
    events.forEach(([at, to], i) =>
      run(
        `INSERT INTO build_events (run_id, seq, at, kind, data) VALUES (?, ?, ?, 'state', ?)`,
        runId,
        i + 1,
        ts(at),
        JSON.stringify({ state: to }),
      ),
    );
  };

  const image = {
    digest: sha256(`shop image ${succeeded.id}`),
    size: 48_213_504,
  };
  const okAttempt = attempt(succeeded.id, "completed", 75, 72, {
    digest: image.digest,
  });
  finish(succeeded.id, "succeeded", okAttempt, 75, 72, {
    digest: image.digest,
    version: `${new Date().toISOString().slice(0, 10)}.1`,
  });
  const imageId = artifact(succeeded.id, okAttempt, "image", image, 72);
  artifact(succeeded.id, okAttempt, "log", putBlob(shopLog), 72);
  const validationId = newId();
  run(
    `INSERT INTO validation_runs (id, org_id, project_id, artifact_id, artifact_digest, build_run_id, trigger_kind, state, verdict, plan, policy_versions, created_at, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, 'build', 'completed', 'passed', '{}', '{}', ?, ?, ?)`,
    validationId,
    org,
    shop.id,
    imageId,
    image.digest,
    succeeded.id,
    ts(72),
    ts(72),
    ts(70),
  );
  for (const [checkId, category, origin, reason, tool] of [
    [
      "runtime-http-startup",
      "test",
      "worker",
      "the container answered on port 3000 within 4s",
      "samuori-runtime",
    ],
    [
      "sensitive-content",
      "sensitive-content",
      "controller",
      "no private keys, credentials or secret files found",
      "samuori-imagescan",
    ],
    ["sbom", "sbom", "controller", "SPDX document generated", "syft"],
    [
      "vulnerabilities",
      "scan",
      "controller",
      "no vulnerabilities at or above the policy threshold",
      "grype",
    ],
  ] as const)
    run(
      `INSERT INTO check_results (id, validation_run_id, check_id, category, origin, required, outcome, reason, tool, findings, created_at, vulnerabilities) VALUES (?, ?, ?, ?, ?, 1, 'passed', ?, ?, '[]', ?, '[]')`,
      newId(),
      validationId,
      checkId,
      category,
      origin,
      reason,
      JSON.stringify({ name: tool, version: "1.0.0" }),
      ts(70),
    );

  const badAttempt = attempt(failed.id, "failed", 40, 38, {
    failure: "build",
    detail: "RUN pip install -r requirements.txt exited with status 1",
  });
  finish(failed.id, "failed", badAttempt, 40, 38, { failure: "build" });
  artifact(failed.id, badAttempt, "log", putBlob(apiLog), 38);

  const liveAttempt = attempt(running.id, "building", 2, null);
  finish(running.id, "building", liveAttempt, 2, null);
  run(
    `INSERT INTO build_live_logs (attempt_id, generation, offset_bytes, data, created_at) VALUES (?, 1, 0, ?, ?)`,
    liveAttempt,
    shopLog.split("#5 DONE")[0] ?? "",
    ts(1),
  );

  run(
    `INSERT INTO publications (id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, gate_decisions, image_digest, destination_ref, requested_by, idempotency_key, created_at, updated_at, validation_run_id) VALUES (?, ?, ?, 'publish', ?, ?, ?, '["1.4.0"]', 'published', '[]', ?, ?, ?, ?, ?, ?, ?)`,
    newId(),
    org,
    shop.id,
    imageId,
    image.digest,
    destination.id,
    image.digest,
    `registry.example.com/web/shop@${image.digest}`,
    adminId,
    key(),
    ts(65),
    ts(64),
    validationId,
  );
  db.close();
  return {
    shop: shop.id,
    snapshot: shopNew,
    succeeded: succeeded.id,
    running: running.id,
    failed: failed.id,
    image: imageId,
  };
}
