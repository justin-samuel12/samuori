export interface TarEntry {
  path: string;
  data: Blob;
  executable?: boolean;
}

const encoder = new TextEncoder();
const BLOCK = 512;

function octal(value: number, width: number): string {
  return value.toString(8).padStart(width - 1, "0") + "\0";
}

function header(
  name: string,
  size: number,
  type: "0" | "x",
  mode = 0o644,
): Uint8Array {
  const h = new Uint8Array(BLOCK);
  const put = (offset: number, text: string) =>
    h.set(encoder.encode(text), offset);
  put(0, name);
  put(100, octal(mode, 8));
  put(108, octal(0, 8));
  put(116, octal(0, 8));
  put(124, octal(size, 12));
  put(136, octal(0, 12));
  put(148, "        ");
  put(156, type);
  put(257, "ustar\0");
  put(263, "00");
  let sum = 0;
  for (const b of h) sum += b;
  put(148, sum.toString(8).padStart(6, "0") + "\0 ");
  return h;
}

function padding(size: number): Uint8Array {
  return new Uint8Array((BLOCK - (size % BLOCK)) % BLOCK);
}

/** PAX "path" record; its length prefix counts itself, so iterate until stable. */
function paxRecord(path: string): Uint8Array {
  const body = ` path=${path}\n`;
  const bodyLen = encoder.encode(body).length;
  let len = bodyLen + 1;
  while (String(len).length + bodyLen !== len)
    len = String(len).length + bodyLen;
  return encoder.encode(`${len}${body}`);
}

function asciiName(path: string): string | null {
  return /^[\x20-\x7e]{1,100}$/.test(path) ? path : null;
}

/**
 * Builds an uncompressed POSIX tar as a Blob. File contents stay as lazy Blob slices, so large folders are not
 * read into memory. Timestamps are fixed; a file is executable only when the caller says so.
 */
export function buildTar(entries: TarEntry[]): Blob {
  const parts: BlobPart[] = [];
  for (const e of entries) {
    const plain = asciiName(e.path);
    if (!plain) {
      const pax = paxRecord(e.path);
      parts.push(
        header("PaxHeader", pax.length, "x") as BlobPart,
        pax as BlobPart,
        padding(pax.length) as BlobPart,
      );
    }
    const fallback = plain ?? e.path.replace(/[^\x20-\x7e]/g, "_").slice(-100);
    parts.push(
      header(
        fallback,
        e.data.size,
        "0",
        e.executable ? 0o755 : 0o644,
      ) as BlobPart,
      e.data,
      padding(e.data.size) as BlobPart,
    );
  }
  parts.push(new Uint8Array(BLOCK * 2) as BlobPart);
  return new Blob(parts, { type: "application/x-tar" });
}

/**
 * Browsers do not expose file modes, so a folder upload guesses which files are scripts: those starting with "#!"
 * and those ending in .sh. Only the first two bytes are read.
 */
export async function looksExecutable(
  path: string,
  data: Blob,
): Promise<boolean> {
  if (path.endsWith(".sh")) return true;
  if (data.size < 2) return false;
  const head = new Uint8Array(await data.slice(0, 2).arrayBuffer());
  return head[0] === 0x23 && head[1] === 0x21;
}

export async function markExecutables(
  entries: TarEntry[],
): Promise<TarEntry[]> {
  const out: TarEntry[] = [];
  for (let i = 0; i < entries.length; i += 64) {
    const batch = entries.slice(i, i + 64);
    const flags = await Promise.all(
      batch.map((e) => looksExecutable(e.path, e.data)),
    );
    batch.forEach((e, j) => out.push({ ...e, executable: flags[j] }));
  }
  return out;
}
