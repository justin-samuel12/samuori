# 0005. Editor modes: Guided / Detailed / YAML over one model

Status: Accepted (2026-10-19) — owner decision

## Context
The original used both "Guided/Detailed/YAML" and "Simple/Detailed/YAML".

## Decision
- The modes are **Guided**, **Detailed**, **YAML** everywhere (UI, API enum values `guided | detailed | yaml`, docs).
- All three edit one BuildSpec AST held client-side and validated server-side with the same JSON Schema and semantic
  rules.
- Unknown-but-allowed fields are preserved through every mode.
- Constructs not representable in Guided mode render read-only there with an explanation and a switch-to-Detailed
  action.
- Overwriting a field last edited in another mode requires explicit confirmation.
- A user's preferred mode is their experience-level setting.

## Consequences
Round-trip tests (Guided → YAML → Detailed → Guided) are required unit and E2E tests. The canonical serializer
defines field order and formatting so YAML diffs are stable.

## Amendment (2026-10-26, owner decision)

Guided and Detailed merge into one **Form**: every question the form can edit is shown in the step where it belongs,
with nothing collapsed. The views are **Form** and **YAML**. Fields the form cannot edit are kept and listed as
YAML-only; overwriting a field last changed in the other view still asks first. Drafts saved in Guided or Detailed
open in the form. The API's `editorMode` enum is unchanged; the form records `detailed`. Round trips are now
Form → YAML → Form (`ui/src/spec/model.test.ts`, the acceptance specs).
