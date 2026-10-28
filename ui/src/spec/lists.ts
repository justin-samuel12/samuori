// A BuildSpec command is a shell string or one command's argument vector (docs/architecture/buildspec.md), so
// several command lines are saved as one shell string joined with " && ": they run in order and stop at the first
// failure.
const joiner = " && ";

/** The lines a command field shows, or null for a value the form cannot edit (an argument vector, or several lines of shell). */
export function commandLines(value: unknown): string[] | null {
  if (value === undefined || value === null || value === "") return [];
  if (typeof value !== "string" || value.includes("\n")) return null;
  return value.split(joiner);
}

export function commandValue(lines: string[]): string | undefined {
  const kept = cleanLines(lines);
  return kept.length ? kept.join(joiner) : undefined;
}

/** The entries a list field shows; values that are not lists of strings are not editable in the form. */
export function listLines(value: unknown): string[] | null {
  if (value === undefined || value === null) return [];
  if (!Array.isArray(value) || value.some((v) => typeof v !== "string"))
    return null;
  return value as string[];
}

export function listValue(lines: string[]): string[] | undefined {
  const kept = cleanLines(lines);
  return kept.length ? kept : undefined;
}

function cleanLines(lines: string[]): string[] {
  return lines.map((l) => l.trim()).filter(Boolean);
}

export function move<T>(items: T[], index: number, by: number): T[] {
  const to = index + by;
  if (index < 0 || index >= items.length || to < 0 || to >= items.length)
    return items;
  const next = [...items];
  const [item] = next.splice(index, 1);
  next.splice(to, 0, item as T);
  return next;
}
