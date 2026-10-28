---
title: Glossary
section: Reference
summary: Short definitions of the words Samuori uses.
roles: [everyone]
order: 95
---
| Term | Meaning |
|---|---|
| **Project** | One application and the images built from it. Belongs to a team |
| **Source upload (snapshot)** | An immutable copy of the files you uploaded. Builds always use a snapshot, never your machine |
| **Discovery** | Samuori reading your files to work out the language, framework and how to build. It never runs your code |
| **Build specification (spec)** | How to build and check the image. Each change is a new revision |
| **Build run** | One build of a spec revision from a snapshot. It may use several attempts |
| **Worker** | A dedicated machine that runs builds in isolation. Workers belong to pools |
| **Isolation profile** | How a pool isolates builds. Uploaded source builds only on pools with an approved and verified profile |
| **Validation** | The checks run on a finished image: tests, sensitive content, SBOM and policies |
| **Destination** | A registry location that images are published to |
| **Publication** | Pushing a validated image to a destination. Ready does not mean published |
| **Promotion** | Copying a published image to another destination by digest, without rebuilding |
| **Secret key** | A secret an administrator granted to a project. Builds use it through a name in the spec, never its value |
| **Output exception** | An approved, time-limited permission for an image to contain specific sensitive files |
| **Quarantine** | Images held back because the worker that built them was revoked |
