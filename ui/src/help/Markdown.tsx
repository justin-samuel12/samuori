import { Fragment, type ReactNode } from "react";
import { Link } from "react-aria-components";

// The guides use a small Markdown subset; it is rendered as React elements, never as HTML.
interface Item {
  text: string;
  children: Item[];
}

type Block =
  | { kind: "heading"; level: number; text: string }
  | { kind: "paragraph"; text: string }
  | { kind: "list"; ordered: boolean; items: Item[] }
  | { kind: "table"; head: string[]; rows: string[][] }
  | { kind: "code"; text: string };

const listMarker = /^(\d+\.|-)\s+(.*)$/;
const childMarker = /^\s{2,}-\s+(.*)$/;

function cells(line: string): string[] {
  return line
    .trim()
    .replace(/^\||\|$/g, "")
    .split("|")
    .map((c) => c.trim());
}

export function parse(source: string): Block[] {
  const lines = source.split("\n");
  const blocks: Block[] = [];
  let i = 0;
  const startsBlock = (l: string) =>
    /^(#{1,3}\s|\||```)/.test(l) || listMarker.test(l);
  while (i < lines.length) {
    const line = lines[i] ?? "";
    if (line.trim() === "") {
      i++;
      continue;
    }
    if (line.startsWith("```")) {
      const code: string[] = [];
      i++;
      while (i < lines.length && !(lines[i] ?? "").startsWith("```"))
        code.push(lines[i++] ?? "");
      i++;
      blocks.push({ kind: "code", text: code.join("\n") });
      continue;
    }
    const heading = /^(#{1,3})\s+(.*)$/.exec(line);
    if (heading) {
      blocks.push({
        kind: "heading",
        level: heading[1]?.length ?? 2,
        text: heading[2] ?? "",
      });
      i++;
      continue;
    }
    if (line.startsWith("|")) {
      const rows: string[][] = [];
      while (i < lines.length && (lines[i] ?? "").startsWith("|")) {
        const l = lines[i++] ?? "";
        if (!/^\|[\s:|-]+\|$/.test(l.trim())) rows.push(cells(l));
      }
      blocks.push({ kind: "table", head: rows[0] ?? [], rows: rows.slice(1) });
      continue;
    }
    const first = listMarker.exec(line);
    if (first) {
      const ordered = first[1] !== "-";
      const items: Item[] = [];
      let inChild = false;
      while (i < lines.length) {
        const l = lines[i] ?? "";
        const top = listMarker.exec(l);
        const child = childMarker.exec(l);
        const last = items[items.length - 1];
        if (l.trim() === "") {
          const next = lines[i + 1] ?? "";
          if (listMarker.test(next) || /^\s{2,}\S/.test(next)) {
            i++;
            continue;
          }
          break;
        }
        if (top && (top[1] !== "-") === ordered) {
          items.push({ text: top[2] ?? "", children: [] });
          inChild = false;
        } else if (child && last) {
          last.children.push({ text: child[1] ?? "", children: [] });
          inChild = true;
        } else if (/^\s+\S/.test(l) && last) {
          const target = inChild
            ? last.children[last.children.length - 1]
            : last;
          if (target) target.text += " " + l.trim();
        } else break;
        i++;
      }
      blocks.push({ kind: "list", ordered, items });
      continue;
    }
    const para: string[] = [];
    while (i < lines.length) {
      const l = lines[i] ?? "";
      if (l.trim() === "" || (para.length > 0 && startsBlock(l))) break;
      para.push(l.trim());
      i++;
    }
    blocks.push({ kind: "paragraph", text: para.join(" ") });
  }
  return blocks;
}

const inline = /(`[^`]+`)|(\*\*[^*]+\*\*)|(\[[^\]]+\]\([^)\s]+\))/g;

function Inline({
  text,
  canOpen,
}: {
  text: string;
  canOpen: (topic: string) => boolean;
}) {
  const out: ReactNode[] = [];
  let last = 0;
  for (const m of text.matchAll(inline)) {
    const at = m.index;
    if (at > last) out.push(text.slice(last, at));
    const token = m[0];
    if (token.startsWith("`")) {
      out.push(<code key={at}>{token.slice(1, -1)}</code>);
    } else if (token.startsWith("**")) {
      out.push(
        <strong key={at}>
          <Inline text={token.slice(2, -2)} canOpen={canOpen} />
        </strong>,
      );
    } else {
      const label = token.slice(1, token.indexOf("]("));
      const href = token.slice(token.indexOf("](") + 2, -1);
      const topic = href.startsWith("help:") ? href.slice(5) : undefined;
      if (topic !== undefined && canOpen(topic)) {
        out.push(
          <Link key={at} href={`/help/${topic}`} className="samuori-link">
            {label}
          </Link>,
        );
      } else if (topic === undefined && href.startsWith("/")) {
        out.push(
          <Link key={at} href={href} className="samuori-link">
            {label}
          </Link>,
        );
      } else {
        out.push(<Fragment key={at}>{label}</Fragment>);
      }
    }
    last = at + token.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return <>{out}</>;
}

function List({
  items,
  ordered,
  canOpen,
}: {
  items: Item[];
  ordered: boolean;
  canOpen: (topic: string) => boolean;
}) {
  const body = items.map((it, k) => (
    <li key={k}>
      <Inline text={it.text} canOpen={canOpen} />
      {it.children.length > 0 && (
        <List items={it.children} ordered={false} canOpen={canOpen} />
      )}
    </li>
  ));
  return ordered ? <ol>{body}</ol> : <ul>{body}</ul>;
}

export function Markdown({
  source,
  canOpen,
}: {
  source: string;
  canOpen: (topic: string) => boolean;
}) {
  return (
    <div className="samuori-prose">
      {parse(source).map((b, k) => {
        switch (b.kind) {
          case "heading":
            return b.level <= 2 ? (
              <h2 key={k}>
                <Inline text={b.text} canOpen={canOpen} />
              </h2>
            ) : (
              <h3 key={k}>
                <Inline text={b.text} canOpen={canOpen} />
              </h3>
            );
          case "paragraph":
            return (
              <p key={k}>
                <Inline text={b.text} canOpen={canOpen} />
              </p>
            );
          case "list":
            return (
              <List
                key={k}
                items={b.items}
                ordered={b.ordered}
                canOpen={canOpen}
              />
            );
          case "code":
            return (
              <pre key={k} className="samuori-code">
                {b.text}
              </pre>
            );
          case "table":
            return (
              <div key={k} className="samuori-table-wrap my-4">
                <table className="samuori-table">
                  <thead>
                    <tr>
                      {b.head.map((h, j) => (
                        <th key={j}>
                          <Inline text={h} canOpen={canOpen} />
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {b.rows.map((r, j) => (
                      <tr key={j}>
                        {r.map((c, n) => (
                          <td key={n}>
                            <Inline text={c} canOpen={canOpen} />
                          </td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            );
        }
      })}
    </div>
  );
}
