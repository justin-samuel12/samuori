# Source Uploads and Snapshots

Status: **Authoritative** (implemented in WP7). Threats: TM-09 to TM-14, TM-17. Related: ADR 0004, `security.md`.

## Upload protocol

1. `POST /projects/{id}/uploads` with the exact archive size (`totalBytes`), file count and optional extra exclude
   patterns. The server checks project access, limits and the caller's open-upload quota, and returns `chunkBytes`
   and `maxChunks`.
2. `PUT /uploads/{id}/chunks/{index}` sends raw bytes. Every chunk except the last is exactly `chunkBytes` long;
   the last holds the remainder. Re-sending an index replaces it. `GET /uploads/{id}` lists received chunks so a
   client can resume.
3. `POST /uploads/{id}/commit` with an `Idempotency-Key` returns `202` and an operation. Repeating the call with
   the same key returns the same operation; any other commit of that upload is a `409`.
4. Poll `GET /operations/{id}`. On success, `resourceId` is the new snapshot.

The payload is one TAR, gzip-compressed TAR or ZIP archive, detected by its magic bytes. For a selected folder the
browser builds an uncompressed POSIX TAR (PAX records for long or non-ASCII names) from lazy file slices, after
applying the exclusion rules from `GET /upload-policy` so the preview matches what the server keeps. Browsers do not
expose file modes, so folder uploads mark a file executable when it starts with `#!` or ends in `.sh` (reading only
its first two bytes) and every other file non-executable; upload an archive to keep exact modes.
Browsers resolve symbolic links inside a selected folder, so linked files arrive as ordinary file contents. Only the uploader can see or change an upload, and only while they can still write
to the project. Uploads expire 24 hours after creation; expired and failed uploads lose their staged chunks.
A commit interrupted by a restart is failed with code `interrupted` and must be uploaded again.

## Entry rules

| Entry | Handling |
|---|---|
| Regular file, directory | Accepted after path normalization |
| Symlink, hardlink, device, FIFO, socket | Refused, counted in `rejectedEntries`, warning |
| Absolute, `..`, `.`, empty segment, backslash, drive letter, control or bidi-control character, invalid UTF-8, path > 1024 bytes, segment > 255 bytes, depth > 48 | Refused, counted, warning |
| Case-folding or Unicode (NFC) collision, file/directory overlap, duplicate path | Whole upload rejected (`path_collision`) |
| Nested archive | Stored as an ordinary file; never extracted |
| Encrypted ZIP entry | Whole upload rejected |

Paths are stored in NFC form. Only the executable bit of the mode is kept; owners and timestamps are dropped so the
snapshot digest depends on content alone.

## Exclusions

Always excluded: `.git`, `.hg`, `.svn`, `node_modules`, `__pycache__`, `.venv`, `.DS_Store`, `Thumbs.db`, `.env`
and `.env.*` (except `.env.example`, `.env.sample`, `.env.template`, `.env.dist`), and key files (`*.pem`, `*.key`,
`*.p12`, `*.pfx`, `id_rsa`, `id_dsa`, `id_ecdsa`, `id_ed25519`). Users may add patterns; patterns without `/`
match any path segment, patterns with `/` match the relative path or a parent. Negation is not allowed.

## Limits

| Limit | Default |
|---|---|
| Archive size | 1 GiB |
| Files | 50,000 |
| File size | 512 MiB |
| Expanded total | 4 GiB |
| Expansion ratio | 100× the compressed size (minimum budget 100 MiB) |
| Chunk size | 8 MiB (request body limit for chunks) |
| Open uploads per user | 5 |
| JSON request bodies | 1 MiB |
| API requests per caller | 20/s sustained, burst 100 |

## Preflight warnings

Best effort only, never claimed exhaustive: private keys, AWS access key IDs, GitHub/GitLab/Slack/npm tokens
(first 1 MiB of each file), compiled executables (ELF, PE, Mach-O), the EICAR test signature, and excluded credential
files. Warnings carry a code, a message and the path, never the matched content.

## Snapshots

Each accepted file is stored in the content-addressed blob store (`<dataDir>/blobs/sha256/…`). The snapshot
manifest (sorted files with size, digest and executable flag, plus directories) is stored the same way, and its
digest is the snapshot digest. Every read re-hashes the content and fails on mismatch (TM-17). Snapshot rows
cannot be updated. Re-uploading identical content creates a new snapshot with the same digest. Garbage collection
of unreferenced blobs arrives with artifact storage (WP18).

**Deleting an upload** ([ADR 0040](../adr/0040-uploader-deletes-uploads.md)): `DELETE /snapshots/{id}`, for the
person who uploaded it only, refused while a build is running from it. A row in `source_snapshot_deletions` marks the
snapshot deleted; the snapshot row itself never changes. Its manifest and files are removed at once unless another
snapshot or artifact uses the same blob, and garbage collection no longer counts its manifest as a reference.
Reading a deleted snapshot's files fails with `ErrDeleted` (`410 source_deleted`), which stops discovery, admission,
builds and federation from using it. SEC-UPL-07.
