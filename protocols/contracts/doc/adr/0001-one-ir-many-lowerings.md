# ADR 0001 — One authored contract IR, many generated lowerings

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/contracts` (`protocols/contracts`)

## Context

A project's contract vocabulary (enums, tagged unions, DTOs, config fields,
scopes, capabilities, grants, claims, principal kinds) would otherwise be
written once per output: Go and TypeScript types by hand, JSON Schema, OpenAPI
components from reflection, docs from memory. Each generator re-derives shapes
from language reflection, so the same contract disagrees with itself and nobody
can tell which artifact is wrong. Closed value sets and discriminated unions
have no reusable form at all.

## Decision

1. **The IR is authored once and committed; generators only lower it.**
   `<project>/schema/contracts.json` (promoted from `.gen/schema/`) is the
   single reviewable statement. Go types, TypeScript types, JSON Schema, OpenAPI
   components, and Markdown tables are lowerings of it. A generator that reads
   language reflection instead of the IR is a defect.
2. **Type names share one namespace.** `enums`, `unions`, and `structs` resolve
   from one namespace; a duplicate name across kinds is
   `contracts.duplicate_node`, not last-one-wins.
3. **Union variants are tagged.** A variant carries either a `struct` reference
   or inline `fields`, never both.
4. **JSON Schema is the only cross-language byte-parity artifact.** Go and
   TypeScript type files are pinned by per-language goldens. The JSON Schema is
   byte-compared between `MarshalJSONSchema` and TypeScript
   `serializeJSONSchema`.
5. **Every closed vocabulary is frozen behind the protocol version.** The node
   set, field-type vocabulary, and shared config field types change only with a
   `ProtocolVersion` bump.
6. **Every diagnostic code ships a baked remediation**, because IRs are often
   hand-authored.

## Rejected alternatives

- **Generate the IR from Go reflection.** Makes Go the authority for a contract
  TypeScript also implements, and cannot express a contract before code exists.
- **Untagged or shape-discriminated unions.** Ambiguous once two variants share
  fields, and not lowerable to a strict JSON Schema `oneOf`.
- **Byte-compare Go and TypeScript type files.** Identical bytes across
  languages means one is not idiomatic.
- **Per-generator vocabulary extensions.** The other lowerings silently drop
  the extra fact.

## Consequences

- A new node kind or field type is a versioned change with schema, fixture,
  emitter, and golden updates.
- Hand edits to generated outputs drift; `putnami contracts check` fails on
  drift.
- A project that needs one artifact still authors the whole IR.
- No TypeScript producer stamps the IR yet, so the producer scan in
  `version_test.go` is a deliberately empty guard.
