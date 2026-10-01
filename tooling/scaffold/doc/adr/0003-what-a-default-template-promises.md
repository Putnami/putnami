# ADR 0003 — What a default template promises

- **Status**: accepted
- **Scope**: `@putnami/scaffold` (`tooling/scaffold`)

## Context

`putnami projects create` offers a fixed set of default templates. Each is
somebody's first contact with Putnami and a support promise: the workspace
support catalog classifies them, so users may build on what they produce.

Without a stated promise, a template either grows into a sample nobody wants to
delete from, or rots against APIs that moved. A template could also demonstrate
the framework or be the minimum that works; choosing per template gives an
inconsistent set.

## Decision

A default template is the **smallest project that already works**, not a
demonstration. Each template promises exactly four things:

1. It renders into a project whose manifests parse and whose declared extension
   activates.
2. It contains a working example of its own kind (an exported function for a
   library, a served route for a server, a rendered page for a web
   application) and a test beside it.
3. It declares every dependency its own files use, and no more.
4. It is a starting point the user edits. Nothing in it is load-bearing for
   Putnami itself.

Samples, not templates, demonstrate the framework. When a template and a sample
would show the same thing, the template shows less.

Each template declares its own feature, with `tooling/project-scaffolding` as
parent: a Go library and a React web application make different promises.

## Invariants

- A template ships no file the scaffolded project does not need.
- A template's dependency declaration matches what its sources import.
- A template that renders an application is servable by `putnami serve`.
- A template's example has a test beside it.
- Every template is classified in the workspace support catalog, and the
  catalog entry, not this document, is the support promise.

## Rejected alternatives

- **Rich templates that demonstrate the framework.** The first experience
  becomes "delete what you do not need", and every framework change touches
  every template.
- **One template per language, configured by flags.** The flag matrix becomes
  the thing to test; a library and a web application share almost no files.
- **No templates; document a manual setup.** Moves the drift into prose nothing
  checks.
- **One shared feature for all templates.** A feature links intent to one
  project root, so the other templates would state no outcome.
- **Leave templates unclassified in the support catalog.** An unstated support
  status is a promise made by silence.

## Consequences

- A new framework capability goes into a sample, not a template.
- Structure and packaging are checked by [ADR 0002](0002-templates-are-checked-by-their-packager.md).
  Promises 2 and 3 are proven for TypeScript and Go templates by
  [ADR 0006](0006-templates-run-against-the-workspace-framework.md); Python
  templates have no such proof.
- One feature and one spec per template is more declaration than a shared
  feature, and it makes each promise reviewable.
