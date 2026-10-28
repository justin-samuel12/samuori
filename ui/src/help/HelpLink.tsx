import { Link } from "react-aria-components";
import { CircleHelp } from "lucide-react";
import { useHelpTopics } from "./topics";

/** A link to a guide, shown only when the server lists that guide for this user. */
export function HelpLink({ topic }: { topic: string }) {
  const { data } = useHelpTopics();
  const t = data?.find((x) => x.id === topic);
  if (!t) return null;
  return (
    <Link
      href={`/help/${t.id}`}
      aria-label={`Help: ${t.title}`}
      className="inline-grid size-7 place-items-center rounded-full text-muted no-underline outline-none hover:bg-raised hover:text-fg data-[focus-visible]:outline-2 data-[focus-visible]:outline-[var(--samuori-focus)]"
    >
      <CircleHelp size={18} aria-hidden="true" />
    </Link>
  );
}
