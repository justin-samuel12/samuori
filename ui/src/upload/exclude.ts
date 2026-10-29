export interface UploadPolicy {
  maxArchiveBytes: number;
  maxFiles: number;
  maxFileBytes: number;
  chunkBytes: number;
  defaultExcludes: string[];
  keptFiles: string[];
  secretFilePatterns: string[];
}

function globToRegExp(glob: string): RegExp {
  let re = "";
  for (let i = 0; i < glob.length; i++) {
    const c = glob[i] as string;
    if (c === "*") re += "[^/]*";
    else if (c === "?") re += "[^/]";
    else if (c === "[") {
      const end = glob.indexOf("]", i + 1);
      if (end < 0) return /$^/;
      re +=
        "[" +
        glob
          .slice(i + 1, end)
          .replace(/^\^/, "^")
          .replace(/\\/g, "\\\\") +
        "]";
      i = end;
    } else re += c.replace(/[.+^${}()|\\]/g, "\\$&");
  }
  return new RegExp(`^${re}$`);
}

export interface Exclusion {
  pattern: string;
  secret: boolean;
}

/** Mirrors the server's rules: patterns without "/" match any path segment, others match the path or a parent. */
export function makeExcluder(
  policy: UploadPolicy,
  extra: string[],
): (path: string) => Exclusion | null {
  const patterns = [
    ...policy.defaultExcludes,
    ...extra.map((p) => p.replace(/\/$/, "")),
  ].map((p) => ({
    p,
    re: globToRegExp(p),
    anchored: p.includes("/"),
  }));
  const kept = new Set(policy.keptFiles);
  const secret = new Set(policy.secretFilePatterns);
  return (path) => {
    const segs = path.split("/");
    for (const { p, re, anchored } of patterns) {
      if (anchored) {
        for (let i = segs.length; i > 0; i--)
          if (re.test(segs.slice(0, i).join("/")))
            return { pattern: p, secret: secret.has(p) };
        continue;
      }
      for (const s of segs) {
        if (re.test(s) && !(p === ".env.*" && kept.has(s)))
          return { pattern: p, secret: secret.has(p) };
      }
    }
    return null;
  };
}

/** The archive kinds the server extracts: ZIP, TAR and gzip-compressed TAR. */
export function isArchiveName(name: string): boolean {
  return /\.(zip|tar|tgz|tar\.gz)$/i.test(name);
}

/** Rejects patterns the server refuses, so the user sees the problem before uploading. */
export function invalidPattern(p: string): string | null {
  if (!p) return "Empty pattern";
  if (p.startsWith("!")) return "Negated patterns are not supported";
  if (p.startsWith("/")) return "Patterns are relative to the folder";
  if (p.includes("\\")) return "Use / as the separator";
  if ((p.match(/\[/g) ?? []).length !== (p.match(/\]/g) ?? []).length)
    return "Unbalanced [ ]";
  return null;
}
