# Developer Journey and Primary Screens

Status: **Authoritative.** This is the single definition of the developer wizard (ADR 0006) and the primary
navigation (ADR 0007). The graphical journey is the core product: prioritize fully wired functionality over
ornamental screens.

## Primary navigation (all signed-in users, filtered by permission)

| Screen | Purpose |
|---|---|
| Home | Recent runs, drafts to resume, items awaiting the user, useful empty state ("Create your first image") |
| Projects | Projects the user can access; sources, saved specs, settings within role |
| New Image | The wizard below |
| Build Runs | Searchable run history; run details/results page |
| Image Library | Built and published images by digest; tags, lineage, findings, publish/promote actions |
| Approvals | Publication approvals and security exceptions for users holding Publisher / Security Reviewer roles |
| Account | Profile, sessions, personal API tokens, notification preferences |

Administration is a separate surface (see [`administration.md`](administration.md)). Never show unauthorized
navigation entries, and never treat hiding navigation as authorization — the API enforces every permission. Entries,
buttons and sections are shown from the roles the API document declares per operation (`api.md`, principles).

## Wizard principles

- Resumable, responsive, accessible; a stable **server-side draft** is saved at every step transition.
- No local Docker, Podman, Git, CLI, AWS credentials or container knowledge required.
- Normal application terms by default ("start command", "port your app listens on"), container terms only in
  Detailed mode or help text.
- High-confidence fields are auto-filled and shown as "detected — confirm"; questions are asked only when evidence
  is missing or ambiguous. Never infer guaranteed correctness.
- Every field shows where its value came from: detected (with evidence), recipe default, admin policy (locked), or
  user-entered.
- Useful empty states and actionable errors everywhere.

## Editor modes (ADR 0005)

**Form** and **YAML** are two views of one in-memory model (the BuildSpec AST). The owner merged the former Guided
and Detailed modes into one form on 2026-10-26 (ADR 0005 amendment).

- **Form** — every question, in plain language, in the step where it belongs; nothing is collapsed. Install, build
  and test commands are ordered lists of command lines (Add, Remove, Move up/down); one line is saved as a string,
  several as one shell string joined with `&&`, because a list in the BuildSpec is one command's argument vector.
  The start command stays one line: a container runs one main process, so several need a start script.
- **YAML** — the canonical serialization, schema-validated as the user types. It is opened from the Review
  step with **Advanced: edit as YAML** and left with **Back to the form**, so the form stays the default path.

Changes round-trip without semantic loss; fields the form cannot edit (argument-vector commands, `x-*` extensions,
buildpacks `env` and bindings, expected sensitive content) are kept and listed as YAML-only. Overwriting a field last
edited in the other view requires explicit confirmation. Drafts saved in Guided or Detailed mode open in the form.

The steps below keep their former "Detailed mode adds" column as a list of what the form asks beyond the basic
questions; what is built today: step 3 adds a description; step 4 has the runtime version (with organization runtimes
labelled), system packages for recipes, build arguments, engine preference and platforms; step 5 adds the check
timeout, SBOM, extra checks and provenance; step 6 adds image labels and annotations, which builds apply with
automatic OCI values (`architecture/buildspec.md#image-labels-and-annotations`). Extra checks, provenance and the
engine preference are kept in the spec but not yet applied to builds, and the form says so.

## The wizard: one authoritative definition

Steps are numbered for reference; conditional steps are skipped (not hidden-but-required) when their condition is
false, and the stepper shows them as "Not needed for this app".

| # | Step | Shown when | Guided mode | Detailed mode adds |
|---|---|---|---|---|
| 1 | Upload source | Always | Folder picker (or ZIP/TAR); exclusion preview; progress/cancel | Custom include/exclude rules |
| 2 | Discover & choose app | Always; app chooser only if >1 candidate app found | Detected stack summary with evidence and confidence; pick subproject | Full evidence table, detector versions, raw findings |
| 3 | Describe your app | Always; individual questions conditional (see below) | App type; install/build/start/test commands prefilled; port if HTTP; runtime version from approved list; test-secret references | Working directory, user, environment defaults, system packages, certificates |
| 4 | Choose how to build | Always shown; choice offered when a Dockerfile is found or more than one approved recipe fits | Recommended option with a plain explanation: an approved recipe ("Samuori builds it for you") or "Use my Dockerfile" (detected Dockerfiles listed). Secrets panel when secrets are needed | Recipe alternatives, base-image digest, stages, reusable steps, platform matrix; for Dockerfiles: context, target stage, named contexts, build args |
| 5 | Checks | Always | Required checks (locked, with reason) + optional checks suited to the app type | Custom test commands, health-check details, timeouts |
| 6 | Destination & tags | Only if the user has ≥1 permitted destination; otherwise shows "Build only — an admin must grant a destination to publish" | Permitted destinations only; auto unique tag always present; optional extra tags; publish manually/automatically within policy | Tag templates, multiple tags, approval requirements preview |
| 7 | Review & build | Always | Plain-English summary, warnings, requirements; Save draft or Build | Spec diff vs previous revision, YAML preview, exact transformations applied by validation |

After step 7 the user lands on the **Run details** page (not a wizard step): it streams progress and later shows
results. Users can close the browser and return.

### Conditional questions in step 3

| Detected / chosen app type | Questions asked |
|---|---|
| Web service / API (`http-service`) | Start command, port (required; flagged if ambiguous), health path (optional) |
| Static site (`static-site`) | Build command, output directory; no port question (recipe serves it) |
| Background worker (`worker`) | Start command; no port; how to tell it started (optional log line / exit expectations) |
| Command-line task (`cli-task`) | Entrypoint command and default arguments; smoke-test arguments |
| All | Install dependencies? (prefilled from lockfile), build command (if any), runtime version (approved options only) |

PowerShell projects show Windows-specific warnings when evidence indicates Windows-only commands/modules. Windows
targets are offered only when a verified Windows execution capability exists (see ADR 0013).

### Step details

**1. Upload source.** Browser directory selection (secure, with progress, cancellation, resumable bounded chunks
where justified) or ZIP/TAR; Git repository and templates are future sources. Preview the file manifest and
exclusion rules *before* transfer. Normalize paths; never follow arbitrary symlinks; reject traversal paths, device
files and special files; enforce max upload size and file count; detect zip bombs; run malware and secret preflight
checks and warn on likely secrets, malware and suspicious executables (detection is best-effort, never claimed
exhaustive). Ignored and sensitive files are
excluded by default. Committing creates an **immutable content-addressed source snapshot**; re-upload creates a new
snapshot.

**2. Discover & choose app.** Inspect files *without executing* source, dependencies or lifecycle scripts. Show
primary/secondary languages, framework, package manager, application kind, likely commands, ports, OS/runtime/
architecture constraints, evidence source and per-finding confidence (rules in
[`../architecture/discovery.md`](../architecture/discovery.md)). Recognize monorepos, polyglot trees and multiple
candidate apps and require explicit selection. Flag ambiguous ports or start commands instead of guessing.

**3. Describe your app.** Ask only what is required (table above). Secrets are referenced, never typed as literals
into the spec. Runtime secrets are never available at build time.

**4. Choose how to build.** Recommend an approved, version-pinned recipe with approved digest-pinned base images, a
non-technical explanation, expected output, and compatibility/admin-policy notes. The default is an optimized
multistage build. When the source contains Dockerfiles, offer "Use my Dockerfile" as a first-class choice (ADR 0028).
The choice (ADR 0047) is *Automatic (recommended)*, *Railpack*, *Paketo Buildpacks*, *Samuori
recipe* and *Custom Dockerfile*; methods the policy or provider scope does not allow are disabled with the reason
from `GET /projects/{id}/build-methods`, which reveals no provider pins. *Automatic* recommends from the static
detectors and the policy: the organization's default when it applies, else a found Dockerfile, a fitting approved
recipe, then Railpack. It recommends *Paketo Buildpacks* only when that is the organization's default. Choosing
buildpacks offers the folder to build; buildpack settings (`env`) and secret bindings are edited in the BuildSpec.
Its stages, args and secret mounts come from static analysis; nothing is executed. Ambiguous findings are shown for
confirmation.

**Secrets panel** (steps 3 and 4, shown whenever the build needs secrets):

| Column | Content |
|---|---|
| Secret | Logical ID and the admin-provided description |
| Required | Required or optional |
| Type | Text or binary, where known |
| Mounted at | Target path when the Dockerfile declares one; otherwise `/run/secrets/<id>` |
| Status | **Ready** (authorized binding exists), **Missing** (with "Request access" to the project's admins), **Not permitted** (policy forbids it here, with the reason) |

The panel never shows values, provider paths or credentials. A missing required secret blocks submission with a
plain explanation. Developers never retrieve passwords, certificates or keys themselves.

**5. Checks.** An application-aware validation profile (e.g. `http-startup`, `process-start`, `cli-smoke`,
`static-serve`), admin-mandated gates (locked), and optional extra checks. The UI distinguishes required, optional,
skipped and blocked explicitly.

**6. Destination & tags.** Show only destinations the user is authorized for (registries/repositories, including
ECR repos admins have registered). Users may *request* a new repository (audited approval flow). An automatically
generated unique immutable tag is always present; user-added tags must pass central tag rules. Publication mode
(manual/automatic) is per project and admin policy overrides it. Required approvals are explained up front. Users
never choose internal artifact storage or see raw credentials.

**7. Review & build.** Compile to the BuildSpec; JSON Schema + semantic validation + policy evaluation; show exact
transformations. Save draft without building is always possible. On "Build", the server persists source snapshot,
spec revision and user intent *before* queueing and returns a durable run ID.
If the project already has a spec with the same image name, the wizard offers to save the work as that spec's next
revision instead of failing; cancelling leaves both untouched so the user can pick another name.

## Run details page

Shows the three lifecycles of ADR 0003 as one timeline: **Build** (queued → building), **Checks** (tests, scans,
SBOM, signing/provenance) and **Publication** (approval → publishing → published), each with its own outcome.
Contents:

- Live progress and stage-level logs with secrets filtered; stage diagnostics and plain-language failure explanations.
- Image digest, size, tags, registry; SBOM / scan / provenance / signature status; sensitive-content findings
  (rule, path and size only, never contents) and any exception or `restricted` classification (ADR 0031).
- Links to source snapshot, spec revision and recipe version; executing worker and location; timestamps; cache use.
- Next actions: publish/promote (if authorized), re-run, edit spec, compare with previous builds,
  how-to-run help, download / OCI copy instructions where allowed.

## Quality requirements

- Real API integration tests for every screen; browser E2E for the full journey; visual regression for critical states.
- WCAG 2.2 AA practical target; keyboard operable; responsive layouts.
- Mapping from the superseded 10-step and 8-step drafts is recorded in ADR 0006.
