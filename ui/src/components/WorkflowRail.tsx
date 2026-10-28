import { Link } from "react-aria-components";
import {
  FileSearch,
  Hammer,
  Send,
  ShieldCheck,
  Ship,
  SlidersHorizontal,
  Upload,
  type LucideIcon,
} from "lucide-react";

export type StageId =
  | "upload"
  | "discover"
  | "configure"
  | "build"
  | "validate"
  | "publish"
  | "kubernetes";

export const stages: {
  id: StageId;
  label: string;
  icon: LucideIcon;
  text: string;
  optional?: boolean;
}[] = [
  {
    id: "upload",
    label: "Upload source",
    icon: Upload,
    text: "A folder or archive from your computer",
  },
  {
    id: "discover",
    label: "Discover",
    icon: FileSearch,
    text: "Static analysis, nothing runs",
  },
  {
    id: "configure",
    label: "Configure",
    icon: SlidersHorizontal,
    text: "BuildSpec or your Dockerfile",
  },
  {
    id: "build",
    label: "Build",
    icon: Hammer,
    text: "Isolated, one build per daemon",
  },
  {
    id: "validate",
    label: "Validate",
    icon: ShieldCheck,
    text: "Scans, policy and runtime test",
  },
  {
    id: "publish",
    label: "Publish",
    icon: Send,
    text: "To approved registries",
  },
  {
    id: "kubernetes",
    label: "Kubernetes export",
    icon: Ship,
    text: "Only if you deploy to Kubernetes",
    optional: true,
  },
];

export interface StageInfo {
  href?: string;
  /** A fact about this stage, such as a count; never a claim the stage passed. */
  meta?: string;
  active?: boolean;
}

/** The path from source to image as one row of stages; each links to where that stage is done. */
export function WorkflowRail({
  label,
  info = {},
}: {
  label: string;
  info?: Partial<Record<StageId, StageInfo>>;
}) {
  return (
    <ol className="samuori-rail" aria-label={label}>
      {stages.map((s, n) => {
        const i = info[s.id] ?? {};
        const Icon = s.icon;
        const body = (
          <>
            <span className="samuori-rail-icon" aria-hidden="true">
              <Icon size={17} />
            </span>
            <span className="samuori-rail-step">
              {s.optional ? "Optional" : `Step ${n + 1}`}
            </span>
            <span className="samuori-rail-label">{s.label}</span>
            <span className="samuori-rail-text">{i.meta ?? s.text}</span>
          </>
        );
        return (
          <li
            key={s.id}
            className="samuori-rail-stage"
            data-active={i.active || undefined}
            data-optional={s.optional || undefined}
          >
            {i.href ? (
              <Link href={i.href} className="samuori-rail-card">
                {body}
              </Link>
            ) : (
              <div className="samuori-rail-card">{body}</div>
            )}
          </li>
        );
      })}
    </ol>
  );
}
