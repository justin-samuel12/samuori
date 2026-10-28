# Spike: Browser Folder Upload

Date: 2026-10-19. Status: **Complete; feasible, no contradictions.** Code: `tools/spikes/browser-upload/`. Raw output is kept in the
maintainer's private records.

## Setup
- Playwright 1.64.1 with Chromium 156, Firefox and WebKit on macOS. The page uses `<input type=file webkitdirectory>`.
- The client builds a manifest (relative path, size, NFC check, exclusion match). Chunked upload uses 1 MiB
  `Blob.slice` chunks over `fetch`, with resume (the server lists received chunks) and cancellation
  (`AbortController`). The server reassembles the file and returns a SHA-256 on commit.
- Synthetic project contents:
  - nested sources;
  - `node_modules/`;
  - a hidden `.env`;
  - an NFD-encoded `café.txt`;
  - a 3 MiB binary;
  - an empty directory;
  - a symlink to a file outside the selected folder.

## Results

| Check | Chromium | Firefox | WebKit |
|---|---|---|---|
| Nested relative paths (`webkitRelativePath`) | ✔ | ✔ | ✔ |
| Hidden files (`.env`) | included | included | **omitted** |
| Symlink to outside file | omitted | omitted | omitted |
| Empty directories | absent | absent | absent |
| NFD file name delivered unnormalized | yes | yes | yes |
| Client exclusion rules (`node_modules`, `.git`, `.env*`) | applied | applied | applied |
| Cancel after 2 chunks, then resume with only missing chunks | ✔ | ✔ | ✔ |
| Reassembled SHA-256 matches the source | ✔ | ✔ | ✔ |

## Conclusions for M2
- Directory selection plus chunked, resumable upload works in all three engines without plugins or local tools.
- Browsers differ on hidden files. Exclusions run in the client, for the preview and for bandwidth, **and are
  enforced again on the server**, which is authoritative (SEC-UPL tests).
- Filenames arrive unnormalized. The server normalizes to NFC and rejects case- or normalization-collisions (TM-12).
- Empty directories cannot be carried by browser selection. Snapshots contain files only, which matches Dockerfile
  `COPY` semantics. The UI says so where relevant.
- Symlinks were omitted here, but the server still rejects link entries from ZIP/TAR uploads (TM-11).

## Limitations
- Playwright sets the folder programmatically. A real user's picker (including how each OS dialog treats symlinks
  and hidden files) is not exercised and needs a manual check during M2.
- Not tested: drag-and-drop folders (`DataTransferItem.webkitGetAsEntry`), the Chromium-only File System Access API
  (`showDirectoryPicker`, which needs a user gesture), and very large file counts. These are covered by M2 E2E and
  performance tests.
