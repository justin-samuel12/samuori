import { useId, type ReactNode } from "react";
import { Link } from "react-aria-components";
import {
  ChevronRight,
  CircleAlert,
  CircleCheck,
  Inbox,
  Info,
  TriangleAlert,
  type LucideIcon,
} from "lucide-react";
import { HelpLink } from "../help/HelpLink";
import { Spinner } from "./Spinner";

type Tone = "error" | "info" | "success" | "warning";

const alertIcon: Record<Tone, LucideIcon> = {
  error: CircleAlert,
  info: Info,
  success: CircleCheck,
  warning: TriangleAlert,
};

export function Alert({
  tone = "error",
  children,
}: {
  tone?: Tone;
  children: ReactNode;
}) {
  const Icon = alertIcon[tone];
  return (
    <div
      className={`samuori-alert samuori-alert--${tone}`}
      role={tone === "error" ? "alert" : "status"}
    >
      <Icon size={18} aria-hidden="true" />
      <div className="samuori-alert-body">{children}</div>
    </div>
  );
}

export function Loading({ label = "Loading…" }: { label?: string }) {
  return (
    <p className="samuori-muted flex items-center gap-2" role="status">
      <Spinner decorative />
      {label}
    </p>
  );
}

/** The full-page wait while the portal starts; it matches the mark index.html shows before scripts run. */
export function PageSpinner({
  label,
  showLabel = false,
}: {
  label: string;
  showLabel?: boolean;
}) {
  return (
    <div className="samuori-page-spinner" role="status" aria-live="polite">
      <Spinner variant="assemble" size={64} decorative />
      <span className={showLabel ? "text-sm text-muted" : "sr-only"}>
        {label}
      </span>
    </div>
  );
}

export function EmptyState({
  title,
  icon: Icon = Inbox,
  children,
}: {
  title: string;
  icon?: LucideIcon;
  children?: ReactNode;
}) {
  return (
    <div className="samuori-empty">
      <Icon size={32} aria-hidden="true" />
      <h3>{title}</h3>
      {children}
    </div>
  );
}

export interface Crumb {
  label: string;
  href?: string;
}

export function PageHeader({
  title,
  description,
  actions,
  crumbs,
  help,
}: {
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
  crumbs?: Crumb[];
  /** The guide for this page; its link appears only if the user may read it. */
  help?: string;
}) {
  return (
    <div className="mb-6">
      {crumbs && crumbs.length > 0 && (
        <nav
          aria-label="Breadcrumb"
          className="mb-2 flex flex-wrap items-center gap-1 text-[0.8125rem] text-muted"
        >
          {crumbs.map((c, i) => (
            <span key={i} className="flex items-center gap-1">
              {i > 0 && (
                <ChevronRight size={13} className="opacity-60" aria-hidden />
              )}
              {c.href ? (
                <Link href={c.href} className="samuori-link">
                  {c.label}
                </Link>
              ) : (
                <span>{c.label}</span>
              )}
            </span>
          ))}
        </nav>
      )}
      <div className="samuori-page-header mb-0">
        <div className="min-w-0">
          <div className="flex items-center gap-1.5">
            <h1>{title}</h1>
            {help && <HelpLink topic={help} />}
          </div>
          {description && (
            <p className="samuori-muted max-w-3xl text-[0.9375rem]">
              {description}
            </p>
          )}
        </div>
        {actions && <div className="samuori-actions">{actions}</div>}
      </div>
    </div>
  );
}

type Accent = "blue" | "green" | "amber" | "red" | "slate";

const accentStyle: Record<Accent, { chip: string; value: string }> = {
  blue: { chip: "bg-accent-soft text-info", value: "text-fg" },
  green: { chip: "bg-ok-soft text-ok", value: "text-fg" },
  amber: { chip: "bg-warn-soft text-warn", value: "text-fg" },
  red: { chip: "bg-bad-soft text-bad", value: "text-bad" },
  slate: { chip: "bg-raised text-muted", value: "text-fg" },
};

export function StatCard({
  label,
  value,
  icon: Icon,
  hint,
  accent = "blue",
  href,
  linkLabel,
}: {
  label: string;
  value: ReactNode;
  icon: LucideIcon;
  hint?: ReactNode;
  accent?: Accent;
  href?: string;
  linkLabel?: string;
}) {
  const a = accentStyle[accent];
  return (
    <div className="samuori-card flex flex-col gap-3">
      <div className="flex items-start justify-between gap-3">
        <p className="m-0 text-xs font-semibold tracking-wider text-muted uppercase">
          {label}
        </p>
        <span
          className={`grid size-9 flex-none place-items-center rounded-lg ${a.chip}`}
          aria-hidden="true"
        >
          <Icon size={18} />
        </span>
      </div>
      <p
        className={`m-0 text-3xl leading-none font-semibold tracking-tight ${a.value}`}
      >
        {value}
      </p>
      {hint && <p className="m-0 text-[0.8125rem] text-muted">{hint}</p>}
      {href && (
        <Link href={href} className="samuori-link mt-auto text-sm font-medium">
          {linkLabel ?? "View"} →
        </Link>
      )}
    </div>
  );
}

type BadgeTone = "ok" | "warn" | "bad" | "info" | "neutral";

const badgeTone: Record<BadgeTone, string> = {
  ok: "border-ok/30 bg-ok-soft text-ok",
  warn: "border-warn/30 bg-warn-soft text-warn",
  bad: "border-bad/30 bg-bad-soft text-bad",
  info: "border-info/30 bg-info-soft text-info",
  neutral: "border-line-strong bg-raised text-muted",
};

const dotTone: Record<BadgeTone, string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  bad: "bg-bad",
  info: "bg-info",
  neutral: "bg-muted",
};

export function Badge({
  tone = "neutral",
  live,
  children,
  testId,
}: {
  tone?: BadgeTone;
  live?: boolean;
  children: ReactNode;
  testId?: string;
}) {
  return (
    <span
      data-testid={testId}
      className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-semibold whitespace-nowrap ${badgeTone[tone]}`}
    >
      <span
        className={`size-1.5 rounded-full ${dotTone[tone]} ${live ? "animate-pulse" : ""}`}
        aria-hidden="true"
      />
      {children}
    </span>
  );
}

export function Section({
  title,
  description,
  actions,
  children,
  className = "",
}: {
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  const id = useId();
  return (
    <section className={`samuori-section ${className}`} aria-labelledby={id}>
      <div className="samuori-section-head">
        <div className="min-w-0">
          <h2 id={id}>{title}</h2>
          {description && <p className="samuori-muted">{description}</p>}
        </div>
        {actions && <div className="samuori-actions">{actions}</div>}
      </div>
      {children}
    </section>
  );
}
