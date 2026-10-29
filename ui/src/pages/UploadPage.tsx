import { useCallback, useMemo, useRef, useState } from "react";
import {
  DropZone,
  FileTrigger,
  ProgressBar,
  Label,
  isDirectoryDropItem,
  isFileDropItem,
  type DirectoryDropItem,
  type DropItem,
} from "react-aria-components";
import { api, type Schemas } from "../api/client";
import { Button } from "../components/Button";
import { TextField } from "../components/Field";
import { Alert, Loading, PageHeader } from "../components/Feedback";
import { bytes } from "../format";
import { useLoader } from "../hooks";
import { navigate } from "../router";
import { buildTar, markExecutables } from "../upload/tar";
import {
  invalidPattern,
  isArchiveName,
  makeExcluder,
  type UploadPolicy,
} from "../upload/exclude";
import { plural } from "./display";
import {
  uploadArchive,
  UploadError,
  waitFor,
  type Progress,
} from "../upload/uploader";

interface Picked {
  path: string;
  file: File;
}

type Selection =
  | { kind: "folder"; name: string; files: Picked[] }
  | { kind: "archive"; file: File };

const archiveTypes = [
  ".zip",
  ".tar",
  ".tgz",
  ".tar.gz",
  "application/zip",
  "application/x-tar",
  "application/gzip",
];

function fromFolder(list: FileList): Selection | null {
  const files: Picked[] = [];
  let name = "";
  for (const f of Array.from(list)) {
    const rel =
      (f as File & { webkitRelativePath?: string }).webkitRelativePath ||
      f.name;
    const [root, ...rest] = rel.split("/");
    name = root ?? "";
    files.push({ path: rest.length ? rest.join("/") : rel, file: f });
  }
  return files.length ? { kind: "folder", name, files } : null;
}

async function fromDrop(items: DropItem[]): Promise<Selection | string> {
  const dir = items.find(isDirectoryDropItem);
  if (dir) {
    const files: Picked[] = [];
    const walk = async (d: DirectoryDropItem, prefix: string) => {
      for await (const e of d.getEntries()) {
        if (e.kind === "directory") await walk(e, `${prefix}${e.name}/`);
        else files.push({ path: prefix + e.name, file: await e.getFile() });
      }
    };
    await walk(dir, "");
    return files.length
      ? { kind: "folder", name: dir.name, files }
      : "The dropped folder is empty.";
  }
  const file = items.find(isFileDropItem);
  if (file && isArchiveName(file.name))
    return { kind: "archive", file: await file.getFile() };
  return "Drop one project folder, or a ZIP, TAR or TAR.GZ archive.";
}

export function UploadPage({ projectId }: { projectId: string }) {
  const [policy, setPolicy] = useState<UploadPolicy | null>();
  const [project, setProject] = useState<Schemas["Project"]>();
  const [selection, setSelection] = useState<Selection>();
  const [extra, setExtra] = useState("");
  const [progress, setProgress] = useState<Progress>();
  const [phase, setPhase] = useState<"choose" | "uploading" | "processing">(
    "choose",
  );
  const [error, setError] = useState<string>();
  const abort = useRef<AbortController | null>(null);

  const load = useCallback(async () => {
    const [p, pr] = await Promise.all([
      api.GET("/upload-policy"),
      api.GET("/projects/{projectId}", { params: { path: { projectId } } }),
    ]);
    setPolicy(p.data ?? null);
    setProject(pr.data);
  }, [projectId]);
  useLoader(load);

  const extraPatterns = useMemo(
    () => extra.split(/[\s,]+/).filter(Boolean),
    [extra],
  );
  const patternError =
    extraPatterns.map(invalidPattern).find(Boolean) ?? undefined;

  const preview = useMemo(() => {
    if (!policy || selection?.kind !== "folder") return null;
    const exclude = makeExcluder(policy, patternError ? [] : extraPatterns);
    const included: Picked[] = [];
    const excluded = new Map<string, number>();
    const secrets: string[] = [];
    const tooBig: string[] = [];
    for (const f of selection.files) {
      const x = exclude(f.path);
      if (x) {
        excluded.set(x.pattern, (excluded.get(x.pattern) ?? 0) + 1);
        if (x.secret) secrets.push(f.path);
        continue;
      }
      if (f.file.size > policy.maxFileBytes) tooBig.push(f.path);
      included.push(f);
    }
    const size = included.reduce((n, f) => n + f.file.size, 0);
    const problems: string[] = [];
    if (included.length === 0)
      problems.push("Nothing is left to upload after exclusions.");
    if (included.length > policy.maxFiles)
      problems.push(
        `${included.length} files is more than the ${policy.maxFiles} file limit.`,
      );
    if (size > policy.maxArchiveBytes)
      problems.push(
        `${bytes(size)} is more than the ${bytes(policy.maxArchiveBytes)} upload limit.`,
      );
    if (tooBig.length)
      problems.push(
        `${plural(tooBig.length, "file")} ${tooBig.length === 1 ? "exceeds" : "exceed"} the ${bytes(policy.maxFileBytes)} per-file limit, for example ${tooBig[0]}.`,
      );
    return { included, excluded, secrets, size, problems };
  }, [policy, selection, extraPatterns, patternError]);

  async function start() {
    if (!selection || !policy) return;
    setError(undefined);
    setPhase("uploading");
    abort.current = new AbortController();
    try {
      const archive =
        selection.kind === "folder"
          ? buildTar(
              await markExecutables(
                (preview?.included ?? []).map((f) => ({
                  path: f.path,
                  data: f.file,
                })),
              ),
            )
          : selection.file;
      const count =
        selection.kind === "folder" ? (preview?.included.length ?? 1) : 1;
      const op = await uploadArchive(
        projectId,
        archive,
        count,
        patternError ? [] : extraPatterns,
        setProgress,
        abort.current.signal,
      );
      if (op.status === "failed" || !op.resourceId) {
        setPhase("choose");
        setError(
          op.error?.message
            ? `The upload was refused: ${op.error.message}.`
            : "The upload could not be processed.",
        );
        return;
      }
      setPhase("processing");
      const started = await api.POST("/snapshots/{snapshotId}/discoveries", {
        params: { path: { snapshotId: op.resourceId } },
      });
      if (started.data) await waitFor(started.data.id);
      navigate(`/projects/${projectId}/snapshots/${op.resourceId}`);
    } catch (e) {
      setPhase("choose");
      setProgress(undefined);
      if (abort.current?.signal.aborted)
        setError("The upload was cancelled. Nothing was saved.");
      else
        setError(
          e instanceof UploadError
            ? e.message
            : "The upload failed. Try again.",
        );
    }
  }

  if (policy === undefined) return <Loading />;
  if (policy === null)
    return (
      <Alert>
        You do not have permission to upload source to this project.
      </Alert>
    );

  const busy = phase !== "choose";
  const blocked =
    !selection || !!patternError || (preview?.problems.length ?? 0) > 0;
  return (
    <>
      <PageHeader
        help="upload-source"
        title="Upload source"
        crumbs={[
          { label: "Projects", href: "/projects" },
          { label: project?.name ?? "Project", href: `/projects/${projectId}` },
          { label: "Upload" },
        ]}
        description="Choose your project folder, or a ZIP or TAR archive. Nothing is run while we look at your files."
      />
      {error && <Alert>{error}</Alert>}

      <section className="samuori-section" aria-labelledby="choose-h">
        <h2 id="choose-h">1. Choose files</h2>
        <DropZone
          className="samuori-dropzone"
          aria-label="Drop a project folder or archive"
          isDisabled={busy}
          onDrop={(e) => {
            void fromDrop(e.items).then((r) => {
              if (typeof r === "string") setError(r);
              else {
                setError(undefined);
                setSelection(r);
              }
            });
          }}
        >
          <p className="m-0 font-medium">
            Drop your project folder or an archive here, or choose one.
          </p>
          <p className="samuori-muted m-0 text-sm">
            ZIP, TAR or TAR.GZ. Up to {bytes(policy.maxArchiveBytes)} and{" "}
            {plural(policy.maxFiles, "file")} in all, each file at most{" "}
            {bytes(policy.maxFileBytes)}.
          </p>
          <div className="samuori-actions samuori-actions--start">
            <FileTrigger
              acceptDirectory
              onSelect={(l) => l && setSelection(fromFolder(l) ?? undefined)}
            >
              <Button isDisabled={busy}>Choose folder</Button>
            </FileTrigger>
            <FileTrigger
              acceptedFileTypes={archiveTypes}
              onSelect={(l) =>
                l?.[0] && setSelection({ kind: "archive", file: l[0] })
              }
            >
              <Button variant="secondary" isDisabled={busy}>
                Choose ZIP or TAR
              </Button>
            </FileTrigger>
          </div>
        </DropZone>
        {selection?.kind === "archive" && (
          <p>
            Archive <strong>{selection.file.name}</strong> (
            {bytes(selection.file.size)}). Exclusions and safety checks are
            applied on the server.
          </p>
        )}
      </section>

      {selection && (
        <section className="samuori-section" aria-labelledby="review-h">
          <h2 id="review-h">2. Review what will be uploaded</h2>
          {preview && (
            <>
              <p>
                From{" "}
                <strong>
                  {selection.kind === "folder" ? selection.name : ""}
                </strong>
                : <strong>{preview.included.length}</strong> files (
                {bytes(preview.size)}) will be uploaded.
              </p>
              {preview.excluded.size > 0 && (
                <details className="samuori-details-toggle">
                  <summary>
                    {[...preview.excluded.values()].reduce((a, b) => a + b, 0)}{" "}
                    files are left out
                  </summary>
                  <ul>
                    {[...preview.excluded.entries()].map(([p, n]) => (
                      <li key={p}>
                        <code>{p}</code>: {n}
                      </li>
                    ))}
                  </ul>
                </details>
              )}
              {preview.secrets.length > 0 && (
                <Alert tone="warning">
                  {plural(preview.secrets.length, "file")} that usually hold
                  credentials {preview.secrets.length === 1 ? "is" : "are"} left
                  out, for example <code>{preview.secrets[0]}</code>. Use secret
                  references for anything the build needs.
                </Alert>
              )}
              {preview.problems.map((p) => (
                <Alert key={p}>{p}</Alert>
              ))}
            </>
          )}
          <TextField
            label="Also leave out (optional)"
            value={extra}
            onChange={setExtra}
            placeholder="dist, *.log, docs/drafts"
            description="Space- or comma-separated patterns. A name matches anywhere; a path with / matches from the folder root."
            isInvalid={!!patternError}
            errorMessage={patternError}
            isDisabled={busy}
          />
        </section>
      )}

      {selection && (
        <section className="samuori-section" aria-labelledby="upload-h">
          <h2 id="upload-h">3. Upload</h2>
          {phase === "uploading" && progress && (
            <ProgressBar
              className="samuori-progress"
              value={progress.sentBytes}
              maxValue={progress.totalBytes}
              valueLabel={`${bytes(progress.sentBytes)} of ${bytes(progress.totalBytes)}`}
            >
              {({ percentage, valueText }) => (
                <>
                  <Label>Uploading</Label>
                  <span className="samuori-muted">{valueText}</span>
                  <div className="samuori-progress-track">
                    <div
                      className="samuori-progress-fill"
                      style={{ width: `${percentage}%` }}
                    />
                  </div>
                </>
              )}
            </ProgressBar>
          )}
          {phase === "processing" && (
            <Loading label="Checking files and looking for your application…" />
          )}
          <div className="samuori-actions samuori-actions--start">
            <Button onPress={() => void start()} isDisabled={busy || blocked}>
              Upload and analyse
            </Button>
            {phase === "uploading" && (
              <Button
                variant="secondary"
                onPress={() => abort.current?.abort()}
              >
                Cancel
              </Button>
            )}
          </div>
        </section>
      )}
    </>
  );
}
