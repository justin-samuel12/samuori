# 0054. Default image tags: the build date and a daily counter

Status: **Accepted** (2026-10-27, owner decision; provisional, may change during acceptance testing; OD-30). Amends
product decision D02 and
[`registry-and-artifacts.md#tags-d02`](../architecture/registry-and-artifacts.md#tags-d02).

## Context
D02 gives every image an immutable reference tag, `samuori-<first 12 hex of the image digest>`, and leaves version,
custom and moving tags to the person publishing. In practice a publisher without a tagging scheme has only that
digest tag, which says nothing about when the image was built or which build came first. The owner asked for a
readable default: the build's UTC date with a per-day counter, such as `2026-10-27.1`, `2026-10-27.2`. The owner also
asked to warn, not block, when a custom tag someone chooses already exists. D02 already refuses collisions with
protected or immutable tags at publish time, against the live registry.

The wizard's step 6, **Destination & tags**, today holds only image labels and annotations, and says publishing is
not available, which is no longer true.

## Options
- **A. Date and daily counter, assigned when a build succeeds (chosen).** Each successful build gets the next
  number for that UTC day: the first is `2026-10-27.1`. The build page, the Images page and notifications show it, and publishing uses it as the default
  tag unless the publisher gives other tags.
- **B. The same, assigned at publish time per destination repository.** The number follows what is in the
  registry, but one image can get different numbers at different destinations, and it is unknown until published.
- **C. Keep D02 as is and only suggest a date in the publish dialog.** No stored version; nothing to show before
  publishing.

## Decision (option A, owner answers 2026-10-27)
- **Version.** When a build succeeds, Samuori gives it `<UTC date it succeeded>.<n>`, where `n` counts the
  **project's** successful builds that day, across all its apps, starting at 1, in the order they succeed (owner:
  per project; the date is the day it succeeds so numbers follow that order). Failed, cancelled and deleted builds do not
  use a number; a deleted build keeps its number, which is not reused. The version is stored with the build
  (`build_runs.version`, migration 00066) and is unique per project; builds that existed before are not numbered.
- **Tags at publish time.** The immutable `samuori-<digest>` tag stays as it is. If the publisher gives no tags, Samuori
  applies the build's version as a tag. Publishers may still give their own tags, which replace the default.
  Protected tags, immutable tags and collisions keep their checks at publish time (D02), so a version tag that
  already points to another image is refused like any other tag.
- **Custom tags in the spec.** `outputs.image.tags` lists the tags a publication uses by default: literals such as
  `1.4.2` or `stable`, and `{version}` for the build's version. It defaults to `["{version}"]`. When a spec is
  created or revised, Samuori **warns** (never blocks) if the project already published a literal tag, naming the
  destination and version it points to, from Samuori's publication records rather than a registry call. The publish-time
  check against the registry still decides. `latest` is applied only when the spec lists it.
- **Wizard.** Step 6 becomes **Image details**: tags with the default shown, labels and annotations. The stale
  "Publishing is not available in this release yet" notice goes.

## Owner answers
Counter per project; only successful builds are numbered; `latest` only when the spec lists it; existing builds stay
unnumbered.

## Consequences
- Every successful build has a readable, sortable name before it is published; notifications and the Images page
  can show it.
- Two controllers finishing builds of one project at once must not hand out the same number: a counter row per
  project and day (`build_version_counters`) is incremented in the transaction that marks the build succeeded, and a
  unique index on project and version backs it.
- Changes D02's "optional custom tags" into "a default version tag plus optional custom tags"; the immutable digest
  tag and publish-time enforcement are unchanged.
- Reversible: dropping the default leaves the stored versions unused.
