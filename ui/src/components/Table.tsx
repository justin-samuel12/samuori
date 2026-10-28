import {
  Cell,
  Column,
  Row,
  Table as AriaTable,
  TableBody,
  TableHeader,
  Input,
  SearchField,
  type SortDescriptor,
} from "react-aria-components";
import {
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type SortingState,
} from "@tanstack/react-table";
import { ArrowDown, ArrowUp, ArrowUpDown, Search } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";

export interface ColumnDef<T> {
  id: string;
  name: string;
  render: (row: T) => ReactNode;
  isRowHeader?: boolean;
  /** Makes the column sortable by this value. */
  sortValue?: (row: T) => string | number;
  /** "end" right-aligns numbers; a column named "Actions" always is. */
  align?: "start" | "end";
}

function columnClass<T>(c: ColumnDef<T>): string | undefined {
  if (c.name === "Actions") return "samuori-col-actions";
  return c.align === "end" ? "samuori-col-end" : undefined;
}

interface Props<T extends { id: string }> {
  label: string;
  columns: ColumnDef<T>[];
  rows: T[];
  empty?: ReactNode;
  /** State the cell renderers read besides the row itself. */
  dependencies?: unknown[];
  /** Shows a filter over the rows' text values; on by default above ten rows. */
  searchable?: boolean;
}

const searchAbove = 10;

function rowText(v: unknown, depth = 0): string {
  if (typeof v === "string" || typeof v === "number") return String(v);
  if (!v || typeof v !== "object" || depth > 2) return "";
  return Object.values(v)
    .map((x) => rowText(x, depth + 1))
    .join(" ");
}

export function Table<T extends { id: string }>({
  label,
  columns,
  rows,
  empty,
  dependencies,
  searchable,
}: Props<T>) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [query, setQuery] = useState("");
  const showSearch = searchable ?? rows.length > searchAbove;
  const needle = showSearch ? query.trim().toLowerCase() : "";
  const shown = useMemo(
    () =>
      needle
        ? rows.filter((r) => rowText(r).toLowerCase().includes(needle))
        : rows,
    [rows, needle],
  );
  const defs = useMemo(
    () =>
      columns.map((c) => ({
        id: c.id,
        accessorFn: c.sortValue ?? (() => 0),
        enableSorting: c.sortValue !== undefined,
      })),
    [columns],
  );
  // TanStack Table owns the sort order; React Aria renders an accessible grid.
  // eslint-disable-next-line react-hooks/incompatible-library
  const table = useReactTable({
    data: shown,
    columns: defs,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getRowId: (r) => r.id,
  });
  const sorted = table.getRowModel().rows.map((r) => r.original);
  const first = sorting[0];
  const descriptor: SortDescriptor | undefined = first
    ? { column: first.id, direction: first.desc ? "descending" : "ascending" }
    : undefined;

  return (
    <div className="samuori-table-wrap">
      {showSearch && (
        <div className="samuori-table-bar">
          <SearchField
            aria-label={`Filter ${label.toLowerCase()}`}
            value={query}
            onChange={setQuery}
            className="samuori-table-search"
          >
            <Search size={15} aria-hidden="true" />
            <Input placeholder="Filter…" />
          </SearchField>
          <span className="text-xs text-muted" aria-live="polite">
            {needle ? `${shown.length} of ${rows.length}` : `${rows.length}`}
          </span>
        </div>
      )}
      <AriaTable
        aria-label={label}
        className="samuori-table"
        sortDescriptor={descriptor}
        onSortChange={(d) =>
          setSorting([
            { id: String(d.column), desc: d.direction === "descending" },
          ])
        }
      >
        <TableHeader columns={columns}>
          {(c) => (
            <Column
              id={c.id}
              className={columnClass(c)}
              isRowHeader={c.isRowHeader}
              allowsSorting={c.sortValue !== undefined}
            >
              {({ allowsSorting, sortDirection }) => (
                <span className="inline-flex items-center gap-1">
                  {c.name}
                  {allowsSorting && <SortIcon direction={sortDirection} />}
                </span>
              )}
            </Column>
          )}
        </TableHeader>
        <TableBody
          items={sorted}
          dependencies={[...(dependencies ?? []), sorting, needle]}
          renderEmptyState={() => (
            <div className="samuori-table-empty px-4 py-6 text-center text-muted">
              {needle
                ? "Nothing matches the filter."
                : (empty ?? "Nothing here yet.")}
            </div>
          )}
        >
          {(row) => (
            <Row columns={columns} dependencies={dependencies}>
              {(c) => <Cell className={columnClass(c)}>{c.render(row)}</Cell>}
            </Row>
          )}
        </TableBody>
      </AriaTable>
    </div>
  );
}

function SortIcon({ direction }: { direction?: "ascending" | "descending" }) {
  const Icon =
    direction === "ascending"
      ? ArrowUp
      : direction === "descending"
        ? ArrowDown
        : ArrowUpDown;
  return (
    <Icon
      size={13}
      aria-hidden="true"
      className={direction ? "text-fg" : "opacity-50"}
    />
  );
}
