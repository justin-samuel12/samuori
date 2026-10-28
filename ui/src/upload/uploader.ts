import { api, readCookie, type Schemas } from "../api/client";

export type Operation = Schemas["Operation"];

export interface Progress {
  sentBytes: number;
  totalBytes: number;
}

export class UploadError extends Error {}

async function problemMessage(r: Response, fallback: string): Promise<string> {
  try {
    const p = (await r.json()) as { message?: string };
    if (p.message)
      return p.message.charAt(0).toUpperCase() + p.message.slice(1);
  } catch {
    // Non-JSON error bodies fall back to the generic message.
  }
  return fallback;
}

async function putChunk(
  uploadId: string,
  index: number,
  data: Blob,
  signal: AbortSignal,
): Promise<void> {
  for (let attempt = 1; ; attempt++) {
    let r: Response;
    try {
      r = await fetch(
        `${window.location.origin}/api/v1/uploads/${uploadId}/chunks/${index}`,
        {
          method: "PUT",
          body: data,
          credentials: "same-origin",
          headers: {
            "Content-Type": "application/octet-stream",
            "X-CSRF-Token": readCookie("samuori_csrf") ?? "",
          },
          signal,
        },
      );
    } catch (e) {
      if (signal.aborted) throw e;
      if (attempt >= 3)
        throw new UploadError(
          "The connection failed while uploading. Check your network and try again.",
        );
      await new Promise((res) => setTimeout(res, 500 * attempt));
      continue;
    }
    if (r.status === 204) return;
    if (r.status >= 500 && attempt < 3) continue;
    throw new UploadError(
      await problemMessage(r, `Uploading part ${index + 1} failed.`),
    );
  }
}

export async function waitFor(
  opId: string,
  signal?: AbortSignal,
): Promise<Operation> {
  for (;;) {
    const { data } = await api.GET("/operations/{operationId}", {
      params: { path: { operationId: opId } },
      signal,
    });
    if (data && (data.status === "succeeded" || data.status === "failed"))
      return data;
    await new Promise((res) => setTimeout(res, 500));
  }
}

/** Sends archive in exact-size chunks, commits it and waits for the snapshot (docs/architecture/sources.md). */
export async function uploadArchive(
  projectId: string,
  archive: Blob,
  fileCount: number,
  excludePatterns: string[],
  onProgress: (p: Progress) => void,
  signal: AbortSignal,
): Promise<Operation> {
  const created = await api.POST("/projects/{projectId}/uploads", {
    params: { path: { projectId } },
    body: {
      fileCount: Math.max(1, fileCount),
      totalBytes: archive.size,
      excludePatterns,
    },
    signal,
  });
  if (!created.data)
    throw new UploadError(
      created.error?.message ?? "The upload could not be started.",
    );
  const s = created.data;
  for (let i = 0; i < s.maxChunks; i++) {
    const start = i * s.chunkBytes;
    await putChunk(
      s.id,
      i,
      archive.slice(start, Math.min(start + s.chunkBytes, archive.size)),
      signal,
    );
    onProgress({
      sentBytes: Math.min(start + s.chunkBytes, archive.size),
      totalBytes: archive.size,
    });
  }
  const commit = await api.POST("/uploads/{uploadId}/commit", {
    params: {
      path: { uploadId: s.id },
      header: { "Idempotency-Key": crypto.randomUUID() },
    },
    signal,
  });
  if (!commit.data)
    throw new UploadError(
      commit.error?.message ?? "The upload could not be finished.",
    );
  return waitFor(commit.data.id, signal);
}
