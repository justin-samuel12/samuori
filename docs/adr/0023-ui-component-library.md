# 0023. UI component library: React Aria Components with an original design system

Status: Accepted (2026-10-19, owner decision OD-3). Amended 2026-10-27: the brand palette and logo come from `docs/brand/` (ADR 0057).

## Decision
- Use React Aria Components (Apache-2.0; carry its NOTICE) for accessible, unstyled behavior: focus management,
  keyboard interaction, ARIA semantics and internationalization.
- All visual design is original: Samuori design tokens (color, type, spacing, radius, motion) defined once in
  `ui/src/design/`, light and dark themes, and a small set of Samuori components that wrap React Aria primitives.
  Pages use Samuori components, never raw primitives with ad-hoc styling.
- No copying of styling, components or assets from prior projects or other design systems.
- WCAG 2.2 AA is the target. Automated a11y checks run in E2E; visual regression covers critical states.

## Consequences
More upfront styling work than an opinionated kit like MUI. In return the look is original and accessibility
behavior comes from a well-tested library.

## Amendment (2026-10-21, owner decision)
The owner asked for a denser, card-based layout and chose Tailwind CSS with TanStack libraries.
- **Styling:** Tailwind CSS 4 (MIT) generates utilities from Samuori's own tokens in `ui/src/design/tokens.css`, mapped
  in `ui/src/app.css`. Tailwind's preflight reset is not loaded; Samuori keeps its own base styles. The shared `samuori-*`
  component classes remain for existing pages.
- **Behavior and accessibility:** React Aria Components stay for every interactive primitive (buttons, fields,
  dialogs, tabs, tables), so keyboard and ARIA behavior is unchanged. Tables keep the React Aria grid; TanStack
  Table (MIT) only computes sort order.
- **Data:** TanStack Query (MIT) caches server state on redesigned pages; other pages keep their loaders.
- **Icons:** Lucide (ISC).
- **Layout:** a grouped, icon-labelled sidebar, a top bar with breadcrumbs and live controller health, stat cards,
  status badges and card surfaces. Colors, tokens and components are original.

## Amendment (2026-10-23, layout)
Content fills the window; layout rules live in the shared components and `ui/src/app.css`, not in pages.
- **Width:** tables, stat grids and section headers span the content area. Running text stops at `--samuori-measure`
  (48rem) and in-page forms and fields at `--samuori-form-width` (40rem); both start at the same left edge as headings.
- **Sections:** `Section` renders the heading, an optional description and right-aligned actions on one row. Section
  actions sit in that row, not above or below the table. Filters go in a `samuori-toolbar` above the table.
- **Tables:** a column named "Actions" is right-aligned and shrinks to its buttons; `align: "end"` right-aligns
  numbers. Rows share one minimum height and buttons inside tables are compact. Wide tables scroll inside their frame.
- **Forms:** submit buttons and button rows are right-aligned; in inline forms buttons line up with the inputs.
- **Stat cards:** an auto-fit grid, two columns on phones, equal heights per row.
- **Dialogs:** `Dialog` takes a `size`: `confirm` (440px, a question and its buttons), `form` (540px, the default)
  or `wide` (1040px, editors, tables and long configuration). Every dialog ends in a right-aligned button row,
  Cancel (secondary) left of the primary action, which stays in view while a long dialog scrolls. Tokens, digests,
  URLs and commands sit in a `samuori-secret` box that wraps, or a `samuori-code` block that scrolls inside the dialog; the
  dialog never widens. Select, radio-group and fieldset labels match text-field labels. Long values in table cells
  wrap so the Actions column stays in view.

## Amendment (2026-10-26, workspace shell)
The owner asked for a full-width workspace instead of the two-pane layout. This replaces the sidebar in the
2026-10-21 layout bullet; the width, section, table, form and dialog rules above still apply.
- **Navigation:** a top bar lists the workspaces Overview, Projects, Builds (build runs, images, pipelines,
  approvals, queue), Kubernetes, Registries & Recipes, Workers (including build and conversion providers), Security &
  Secrets and Administration (`ui/src/shell/nav.ts`; regrouped 2026-10-27 at the owner's request). A context bar below it shows
  the current workspace and tabs for its pages, so only the navigation that applies is on screen. Below 1280px a menu
  button opens the same list as a sheet. URLs did not change. Role-based hiding is unchanged and is still not
  authorization; a page that needs several operations is listed only when all are allowed.
- **Search palette:** Ctrl+K or ⌘K (or the search button) opens a palette of every page, project and guide the user
  may open, built with React Aria's Autocomplete and Menu.
- **Account menu:** Account, Notifications, Help, the theme (system, light or dark, kept in the browser) and Sign
  out. A bell shows the unread notification count from `GET /notifications`.
- **Workflow rail:** Overview and each project show the path Upload source → Discover → Configure → Build →
  Validate → Publish, then an optional Kubernetes export (dashed, unnumbered); each stage links to where it is done and shows only counts, never a pass.
- **Dialogs:** `form` dialogs open as a drawer on the right (a sheet from the bottom on phones); `confirm` and
  `wide` dialogs stay centered.
- **Tables:** the shared `Table` adds a text filter above ten rows (`searchable` overrides this).
- **Colors:** neutral backgrounds with one blue accent; dark mode follows the system unless the user picks a theme
  (`data-theme` on the root element).
- **Kubernetes workspace:** `/kubernetes` lists the newest conversions of up to 50 projects, from the existing
  per-project API.
