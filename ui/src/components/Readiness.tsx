import type { Schemas } from "../api/client";
import { EgressAccess, type AccessContext } from "./EgressAccess";
import { Badge } from "./Feedback";

type Item = Schemas["ReadinessItem"];

const severity: Record<
  Item["severity"],
  { label: string; tone: "bad" | "warn" | "info" }
> = {
  "will-fail": { label: "Will fail", tone: "bad" },
  "may-fail": { label: "May fail", tone: "warn" },
  advice: { label: "Advice", tone: "info" },
};

const fixer: Record<Item["fixer"], string> = {
  developer: "You can fix this",
  administrator: "An administrator can fix this",
};

const confidence: Record<Item["confidence"], string> = {
  certain: "certain",
  likely: "likely",
  possible: "possible",
};

// Build readiness is a static prediction (ADR 0050); it never decides whether a build may run.
// access, when set, offers a request for each item's requestable egress, with the item as evidence (ADR 0052).
export function ReadinessList({
  items,
  heading = "Build readiness",
  access,
}: {
  items?: Item[];
  heading?: string;
  access?: AccessContext;
}) {
  if (!items?.length) return null;
  return (
    <section
      className="samuori-readiness"
      aria-label={heading}
      data-testid="readiness"
    >
      <h4>{heading}</h4>
      <p className="samuori-muted">
        Predicted from your files without running them. Fix what you can before
        building; a build can still meet problems Samuori did not predict.
      </p>
      <ul>
        {items.map((it, i) => (
          <li
            key={`${it.code}-${i}`}
            className={`samuori-readiness__item samuori-readiness__item--${it.severity}`}
          >
            <div className="samuori-readiness__head">
              <Badge tone={severity[it.severity].tone}>
                {severity[it.severity].label}
              </Badge>
              <span className="samuori-muted">
                {confidence[it.confidence]}
                {it.path && (
                  <>
                    {" · "}
                    <code>
                      {it.path}
                      {it.line ? `:${it.line}` : ""}
                    </code>
                  </>
                )}
              </span>
            </div>
            <p>{it.message}</p>
            <p>
              <strong>{fixer[it.fixer]}:</strong> {it.fix}
            </p>
            {it.alternative && (
              <p className="samuori-muted">{it.alternative}</p>
            )}
            {access && it.requestable && (
              <EgressAccess
                context={{
                  ...access,
                  evidence: {
                    ...access.evidence,
                    code: it.code,
                    path: it.path ?? "",
                    line: it.line ?? 0,
                  },
                }}
                presets={it.requestable.presets}
                destinations={it.requestable.destinations}
                internalOnly={it.requestable.internal ?? []}
              />
            )}
            {it.blocking && (
              <p>
                <strong>
                  Admission refuses the build until this is fixed.
                </strong>
              </p>
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}
