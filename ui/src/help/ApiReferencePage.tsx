import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Input, Link, SearchField } from "react-aria-components";
import { Search } from "lucide-react";
import { api } from "../api/client";
import {
  Alert,
  Badge,
  Loading,
  PageHeader,
  Section,
} from "../components/Feedback";
import {
  curlExample,
  groupByTag,
  operations,
  rolesLabel,
  schemaText,
  search,
  type Json,
  type Operation,
} from "./apiref";

export function ApiReferencePage() {
  const [query, setQuery] = useState("");
  const contract = useQuery({
    queryKey: ["api-contract"],
    staleTime: 5 * 60_000,
    queryFn: async () => {
      const { data } = await api.GET("/openapi.json");
      return (data as Json | undefined) ?? null;
    },
  });
  const spec = contract.data;
  const all = useMemo(() => (spec ? operations(spec) : []), [spec]);
  const shown = search(all, query);
  const info = (spec?.info ?? {}) as { version?: string };

  return (
    <>
      <PageHeader
        title="API reference"
        crumbs={[{ label: "Help", href: "/help" }, { label: "API reference" }]}
        description={
          <>
            Every operation of the Samuori API, read from the contract this
            server runs. Download it as{" "}
            <a className="samuori-link" href="/api/v1/openapi.yml">
              openapi.yml
            </a>{" "}
            or{" "}
            <a className="samuori-link" href="/api/v1/openapi.json">
              openapi.json
            </a>{" "}
            to generate a client.
          </>
        }
      />
      <Section title="Using the API">
        <ul className="m-0 space-y-2 pl-5 text-sm">
          <li>
            Scripts sign in with an API token:{" "}
            <Link href="/account" className="samuori-link">
              create one on your Account page
            </Link>{" "}
            and send it as <code>Authorization: Bearer &lt;token&gt;</code>.
            Administrators issue tokens for automation to service identities.
          </li>
          <li>
            The browser session uses a cookie instead. With a session, every
            request other than GET also needs the <code>X-CSRF-Token</code>{" "}
            header with the value of the <code>samuori_csrf</code> cookie.
            Tokens never need it.
          </li>
          <li>
            Errors are JSON with a <code>code</code> to match on, a readable{" "}
            <code>message</code>, optional <code>details</code> and the{" "}
            <code>requestId</code> to quote to an operator. Lists take{" "}
            <code>limit</code> and <code>cursor</code> and return{" "}
            <code>nextCursor</code> while more remain. Too many requests answer
            429 with <code>Retry-After</code>.
          </li>
          <li>
            The API is <code>v1alpha1</code>
            {info.version ? ` (contract ${info.version})` : ""}: it can still
            change. Deprecated operations answer with <code>Deprecation</code>{" "}
            and, once a removal date is set, <code>Sunset</code> headers.
          </li>
        </ul>
        {spec && (
          <details className="samuori-details-toggle mt-3">
            <summary>Error format</summary>
            <pre className="samuori-code">
              {schemaText(spec, { $ref: "#/components/schemas/Problem" })}
            </pre>
          </details>
        )}
      </Section>
      {contract.isPending ? (
        <Loading />
      ) : !spec ? (
        <Alert>The API contract could not be loaded.</Alert>
      ) : (
        <>
          <div className="mb-4 flex items-center gap-3">
            <SearchField
              aria-label="Search operations"
              value={query}
              onChange={setQuery}
              className="samuori-table-search"
            >
              <Search size={15} aria-hidden="true" />
              <Input placeholder="Search path, operation or role…" />
            </SearchField>
            <span className="text-xs text-muted" aria-live="polite">
              {query
                ? `${shown.length} of ${all.length} operations`
                : `${all.length} operations`}
            </span>
          </div>
          {groupByTag(spec, shown).map(([tag, ops]) => (
            <Section key={tag} title={tag}>
              <ul className="m-0 list-none space-y-2 p-0">
                {ops.map((op) => (
                  <li key={op.id}>
                    <OperationItem spec={spec} op={op} />
                  </li>
                ))}
              </ul>
            </Section>
          ))}
        </>
      )}
    </>
  );
}

function OperationItem({ spec, op }: { spec: Json; op: Operation }) {
  const [open, setOpen] = useState(false);
  return (
    <details
      className="samuori-card p-0"
      onToggle={(e) => setOpen(e.currentTarget.open)}
    >
      <summary className="flex cursor-pointer flex-wrap items-center gap-2 px-4 py-2.5">
        <code className="w-14 text-xs font-semibold">{op.method}</code>
        <code className="text-sm break-all">{op.path}</code>
        {op.deprecated && <Badge tone="warn">Deprecated</Badge>}
        <span className="samuori-muted ml-auto text-xs">{op.id}</span>
      </summary>
      {open && (
        <div className="space-y-3 border-t border-line px-4 py-3 text-sm">
          {op.description && <p className="m-0">{op.description}</p>}
          <p className="m-0">
            <strong>Who may call it:</strong> {rolesLabel(op)}.
            {!op.isPublic &&
              " The server also checks the organization, team or project the call names."}
          </p>
          {op.parameters.length > 0 && (
            <div>
              <strong>Parameters</strong>
              <ul className="m-0 mt-1 space-y-1 pl-5">
                {op.parameters.map((p) => (
                  <li key={`${p.in}:${p.name}`}>
                    <code>{p.name}</code> ({p.in}
                    {p.required ? ", required" : ""}):{" "}
                    <code>{schemaText(spec, p.schema)}</code>
                    {p.description ? ` ${p.description}` : ""}
                  </li>
                ))}
              </ul>
            </div>
          )}
          {op.body && (
            <details className="samuori-details-toggle" open>
              <summary>Request body ({op.bodyType})</summary>
              <pre className="samuori-code">{schemaText(spec, op.body)}</pre>
            </details>
          )}
          <div>
            <strong>Responses</strong>
            <ul className="m-0 mt-1 list-none space-y-1 p-0">
              {op.responses.map((r) => (
                <li key={r.status}>
                  {r.schema ? (
                    <details className="samuori-details-toggle">
                      <summary>
                        <code>{r.status}</code> {r.description}
                      </summary>
                      <pre className="samuori-code">
                        {schemaText(spec, r.schema)}
                      </pre>
                    </details>
                  ) : (
                    <span>
                      <code>{r.status}</code> {r.description}
                    </span>
                  )}
                </li>
              ))}
            </ul>
          </div>
          <div>
            <strong>Example</strong>
            <pre className="samuori-code mt-1">
              {curlExample(op, window.location.origin)}
            </pre>
          </div>
        </div>
      )}
    </details>
  );
}
