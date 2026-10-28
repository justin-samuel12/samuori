export function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n / 1024;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[u]}`;
}

export function shortDigest(d: string): string {
  return d.replace(/^sha256:/, "").slice(0, 12);
}

interface NameRules {
  /** Characters allowed besides lowercase letters, digits and dashes, such as "._" or "/._". */
  extra?: string;
  /** Typing is finished: separators at the end are dropped and the length is capped. */
  done?: boolean;
  max?: number;
}

/**
 * Corrects free text into an identifier while it is typed: lowercase, accents dropped, and any run of other
 * characters becomes one dash. A trailing separator stays until done, so a word can still be typed after it.
 */
export function correctName(text: string, rules: NameRules = {}): string {
  const extra = (rules.extra ?? "").replace(/[\]\\^-]/g, "\\$&");
  const sep = new RegExp(`^[-${extra}]+`);
  let out = text
    .toLowerCase()
    .normalize("NFKD")
    .replace(/[\u0300-\u036f]/g, "")
    .replace(new RegExp(`[^a-z0-9${extra}-]+`, "g"), "-")
    .replace(/-{2,}/g, "-")
    .replace(sep, "");
  if (rules.done) {
    const tail = new RegExp(`[-${extra}]+$`);
    out = out
      .replace(tail, "")
      .slice(0, rules.max ?? 63)
      .replace(tail, "");
  }
  return out;
}

/** An identifier of lowercase letters, digits and dashes, at most 63 characters. */
export function slug(text: string): string {
  return correctName(text, { done: true });
}
