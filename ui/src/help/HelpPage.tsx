import { useQuery } from "@tanstack/react-query";
import { Link } from "react-aria-components";
import { BookOpen } from "lucide-react";
import { api } from "../api/client";
import { Alert, Loading, PageHeader } from "../components/Feedback";
import { Markdown } from "./Markdown";
import { useHelpTopics, type HelpSummary } from "./topics";

const roleLabel: Record<string, string> = {
  everyone: "Everyone",
  developer: "Developers",
  maintainer: "Maintainers",
  publisher: "Publishers",
  operator: "Operators",
  "security-reviewer": "Security reviewers",
  administrator: "Administrators",
};

export function HelpPage({ topic }: { topic?: string }) {
  const topics = useHelpTopics();
  const list = topics.data ?? [];
  const current = topic ?? list[0]?.id;
  const guide = useQuery({
    queryKey: ["help", "topic", current],
    enabled: !!current,
    queryFn: async () => {
      const { data } = await api.GET("/help/topics/{topicId}", {
        params: { path: { topicId: current ?? "" } },
      });
      return data ?? null;
    },
  });
  const visible = new Set(list.map((t) => t.id));
  const sections = new Map<string, HelpSummary[]>();
  for (const t of list)
    sections.set(t.section, [...(sections.get(t.section) ?? []), t]);

  return (
    <>
      <PageHeader
        title="Help"
        description="Guides for the parts of Samuori your roles let you use."
      />
      {topics.isPending ? (
        <Loading />
      ) : (
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-[260px_minmax(0,1fr)]">
          <nav
            aria-label="Guides"
            className="lg:sticky lg:top-24 lg:self-start"
          >
            {[...sections].map(([section, items]) => (
              <div key={section} className="mb-5">
                <h2 className="mb-1.5 text-[0.6875rem] font-semibold tracking-[0.08em] text-muted uppercase">
                  {section}
                </h2>
                <ul className="m-0 list-none space-y-0.5 p-0">
                  {items.map((t) => (
                    <li key={t.id}>
                      <Link
                        href={`/help/${t.id}`}
                        aria-current={t.id === current ? "page" : undefined}
                        className={`block rounded-lg px-3 py-1.5 text-sm no-underline outline-none data-[focus-visible]:outline-2 data-[focus-visible]:outline-[var(--samuori-focus)] ${
                          t.id === current
                            ? "bg-accent-soft font-semibold text-fg"
                            : "text-muted hover:bg-raised hover:text-fg"
                        }`}
                      >
                        {t.title}
                      </Link>
                    </li>
                  ))}
                </ul>
              </div>
            ))}
            <Link
              href="/help/api-reference"
              className="block rounded-lg px-3 py-1.5 text-sm text-muted no-underline outline-none hover:bg-raised hover:text-fg data-[focus-visible]:outline-2 data-[focus-visible]:outline-[var(--samuori-focus)]"
            >
              API reference
            </Link>
          </nav>
          <article className="samuori-card min-w-0">
            {guide.isPending && current ? (
              <Loading />
            ) : guide.data ? (
              <>
                <div className="mb-4 flex items-start gap-3 border-b border-line pb-4">
                  <span
                    className="grid size-9 flex-none place-items-center rounded-lg bg-accent-soft text-info"
                    aria-hidden="true"
                  >
                    <BookOpen size={18} />
                  </span>
                  <div className="min-w-0">
                    <h2 className="m-0 text-xl">{guide.data.title}</h2>
                    <p className="samuori-muted m-0 mt-1 text-sm">
                      For{" "}
                      {guide.data.roles
                        .map((r) => roleLabel[r] ?? r)
                        .join(", ")}
                    </p>
                  </div>
                </div>
                <Markdown
                  source={guide.data.body}
                  canOpen={(id) => visible.has(id)}
                />
              </>
            ) : (
              <Alert tone="info">
                This guide does not exist, or your roles do not include it.
              </Alert>
            )}
          </article>
        </div>
      )}
    </>
  );
}
