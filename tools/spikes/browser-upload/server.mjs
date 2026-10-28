import { createServer } from "node:http";
import { createHash } from "node:crypto";

const uploads = new Map();
const page = `<!doctype html><meta charset="utf-8"><input id="dir" type="file" webkitdirectory multiple>
<script>
const EXCLUDE = [/(^|\\/)node_modules(\\/|$)/, /(^|\\/)\\.git(\\/|$)/, /(^|\\/)\\.env($|\\.)/];
const CHUNK = 1024 * 1024;
window.files = [];
document.getElementById("dir").addEventListener("change", (e) => {
  window.files = [...e.target.files];
  window.manifest = window.files.map((f) => ({ path: f.webkitRelativePath, size: f.size,
    nfc: f.webkitRelativePath === f.webkitRelativePath.normalize("NFC"),
    excluded: EXCLUDE.some((r) => r.test(f.webkitRelativePath)) }));
});
window.upload = async (path, { abortAfterChunks, session } = {}) => {
  const f = window.files.find((x) => x.webkitRelativePath === path);
  const id = encodeURIComponent(session + ":" + path);
  const have = new Set(await (await fetch("/upload/" + id)).json());
  const total = Math.ceil(f.size / CHUNK) || 1;
  const ctrl = new AbortController();
  let sent = 0;
  for (let i = 0; i < total; i++) {
    if (have.has(i)) continue;
    if (abortAfterChunks !== undefined && sent === abortAfterChunks) { ctrl.abort(); return { aborted: true, sent }; }
    await fetch("/upload/" + id + "/" + i + "?total=" + total, { method: "PUT", body: f.slice(i * CHUNK, (i + 1) * CHUNK), signal: ctrl.signal });
    sent++;
  }
  return await (await fetch("/upload/" + id + "/commit", { method: "POST" })).json();
};
</script>`;

createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  const parts = url.pathname.split("/").filter(Boolean);
  if (url.pathname === "/") { res.setHeader("content-type", "text/html"); return res.end(page); }
  if (parts[0] !== "upload") { res.statusCode = 404; return res.end(); }
  const id = decodeURIComponent(parts[1]);
  const u = uploads.get(id) ?? { chunks: new Map(), total: 0 };
  uploads.set(id, u);
  if (req.method === "GET") return res.end(JSON.stringify([...u.chunks.keys()]));
  if (req.method === "PUT") {
    const body = [];
    for await (const c of req) body.push(c);
    u.chunks.set(Number(parts[2]), Buffer.concat(body));
    u.total = Number(url.searchParams.get("total"));
    return res.end("{}");
  }
  if (parts[2] === "commit") {
    if (u.chunks.size !== u.total) { res.statusCode = 409; return res.end(JSON.stringify({ missing: u.total - u.chunks.size })); }
    const data = Buffer.concat([...Array(u.total).keys()].map((i) => u.chunks.get(i)));
    return res.end(JSON.stringify({ bytes: data.length, sha256: createHash("sha256").update(data).digest("hex") }));
  }
  res.statusCode = 400; res.end();
}).listen(4173, "127.0.0.1");
