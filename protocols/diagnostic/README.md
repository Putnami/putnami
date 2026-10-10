# Diagnostic Protocol

## Why

The diagnostic protocol exists so protocol packages can report parse and validation problems in one reusable structure.

That avoids a mix of free-form errors and makes failures easier for tools, tests, and users to understand.

## What

This package provides:

- shared severity levels,
- a structured `Diagnostic` record with code, message, and field metadata,
- helper constructors for errors and warnings, and
- utilities for filtering or checking whether diagnostics contain errors.

## How

Other protocol packages compose this package instead of inventing their own result types:

- strict parsers convert decode failures into diagnostics,
- validators append field-level findings,
- callers use `HasErrors()` or `Errors()` to decide whether to proceed.

Use this package whenever a protocol operation needs to surface machine-readable findings instead of ad hoc strings.

## This module's "wire" is a Go API, not a JSON document

There is no schema directory here and no fixture corpus, and that is not an
omission to be backfilled. Nothing sends a `Diagnostic` document on its own:
`Diagnostic` is the shared **return type** of every protocol package's parse and
validate entry points. It reaches a wire only when an owning contract embeds it
— a CI document validation report, a structured CLI result — and that owning
contract is where the bytes are specified, schematized and fixtured.

So the compatibility surface of this module is the Go type and the Go
constructor set:

- `Severity` and its three values `error`, `warning`, `info`;
- `Diagnostic{Severity, Code, Message, Field}` and its JSON member names
  (`severity`, `code`, `message`, `field`), which do appear on the wire whenever
  an owning contract embeds the struct;
- `Errorf` / `Warningf` / `HasErrors` / `Errors` / `ErrorText` and `Diagnostic.String()`.

**Codes are not owned here.** Each protocol package declares and freezes its own
dotted code namespace (`doctor.unknown_field`, `job.invalid_key`,
`ci.public_literal`, …). This module deliberately defines no code vocabulary: a
central registry would make every new code a change to a package that has
nothing to do with it, and would tempt packages to reuse a neighbour's code for
a different meaning.

## Producers and consumers

| Role | Who |
| --- | --- |
| Producers | every protocol package's strict parsers and validators |
| Consumers | the CLI and any caller that branches on `HasErrors()`/`Errors()`, plus whatever renders findings to a human or a machine |
| Owner of this contract | this project — for the record shape and the severity vocabulary only. Codes and messages belong to the emitting package |

## Versioning and compatibility

This module declares **no `ProtocolVersion` token**, because a bare
`Diagnostic` is never a standalone document that a reader must version-check.
Compatibility is therefore governed by the embedding contract: when a
`Diagnostic` travels inside a versioned document, that document's version rules
apply to it.

Within this module:

- adding a helper, or a new field with `omitempty`, is additive;
- adding, removing or repurposing a `Severity` value is breaking for every
  consumer that switches on severity, and for every embedding contract's closed
  vocabulary — it is a coordinated change across the packages that embed the
  struct, not a local one;
- renaming a JSON member, or removing `omitempty` from `field`, is breaking for
  every embedding wire at once.

Because every protocol package in this repository composes this type, the blast
radius of a change here is the whole `protocols/` tree. That is the reason to
keep the type small rather than a reason to freeze it prematurely.

## Support status

- **Subject**: `go.putnami.dev/protocol/diagnostic`, kind `protocol`.
- **Status**: `preview`, recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json) — the only reviewed
  authority for support status (contract:
  [`protocols/support/README.md`](../support/README.md)).
- **Owner**: this project (`protocols/diagnostic`).
- **Evidence for `preview` rather than `stable`**: the package is exercised by
  `diagnostic_test.go` (constructors, `String()` rendering with and without a
  field, `HasErrors`, `Errors`, `ErrorText`), and its four wire fields are documented on the
  Go type, which `protocols/doccov` guards at or below the undocumented-field bar
  and pins in `documentedPackages`. What is missing for `stable` is a frozen
  vocabulary: **no test pins the three `Severity` values**, there is no
  `ProtocolVersion` token, no schema and no fixture corpus, so nothing would fail
  if a fourth severity were added or one renamed. A test that pins the severity
  set — the same drift guard sibling modules apply to their enums — is the
  concrete work that would earn `stable`.
- **`default` and `parity`**: no claim is recorded on either axis. Omission is
  not a denial — see the support vocabulary.

## Specs and durable decisions

There is deliberately **no user-facing feature or spec for this module**. It is
a shared record type between protocol packages; a user never chooses it, and the
outcome a user cares about — "the tool told me exactly which field is wrong and
why" — is delivered by whichever command renders the findings
(`putnami doctor`, `putnami contracts check`, …). A product feature per shared Go
type would be a promise with no user behind it.

This module records **no durable decisions of its own**. The one rule worth
stating — codes belong to the emitting package, not to a central registry — is
documented above and is enforced by the absence of any code vocabulary here.
When a decision here becomes contested it gets a record under `doc/adr/`
following
[`protocols/features/doc/adr/TEMPLATE.md`](../features/doc/adr/TEMPLATE.md).
