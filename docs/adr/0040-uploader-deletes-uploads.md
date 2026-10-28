# 0040. The uploader may delete an upload

Status: Accepted (2026-10-21, owner decision). Amends D17 and [ADR 0004](0004-source-snapshot-is-buildrun-input.md).

## Context
D17 made source snapshots immutable with admin retention: nothing removed an upload, so a person who uploaded the
wrong files, or files that should never have left their machine, could not take them back.

## Decision
- **Who:** only the person who uploaded a snapshot may delete it, and only while they still have access to its
  project. Teammates, other teams and administrators cannot delete someone else's upload.
- **Always allowed,** whether or not builds used it, except while a build is running from it (cancel it first).
- **Effect:** the snapshot's record stays, marked deleted, so builds, attempts and audit keep their history and
  digests. Its manifest and files are removed from storage at once, except blobs another snapshot, artifact,
  publication or hold still uses (blobs are content-addressed and shared). Deleted snapshots are left out of lists,
  cannot be analysed or built (`410 source_deleted`), and Build again, revalidation and federation reads that need
  the files fail with that error. Images already built and published are not affected.
- **Visibility is unchanged:** uploads stay visible to the project's team, so teammates can review and build from
  them; images stay team-wide.
- Every deletion is audited (`snapshot.delete`).

## Consequences
A build's source can disappear: such builds cannot be rebuilt from the same files or re-examined. Reproducibility
becomes the uploader's choice rather than a guarantee.
