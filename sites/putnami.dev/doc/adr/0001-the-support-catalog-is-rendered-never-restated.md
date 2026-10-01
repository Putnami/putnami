# ADR 0001 — The support catalog is rendered, never restated

- **Status**: accepted
- **Scope**: `putnami.dev` (`sites/putnami.dev`)

## Context

A support status is a public promise: it tells a reader whether they can depend
on a package today. The workspace root holds one reviewed authority for those
promises, `putnami.support.json`, and `protocols/support` requires a generated
view to point back at it instead of becoming a second inventory. A hand-written
status on the site drifts silently: publishing `preview` for something the
catalog calls `stable` understates a commitment, and the reverse invents one.

## Decision

The reviewed catalog is the site's only source of support statuses.

`/docs/support` is generated. `src/plugins/support-catalog.plugin.ts` reads
`putnami.support.json` and renders every classified package and protocol, with
the independent `default` and `parity` claims where present. The page is never
committed and is a pure function of the catalog bytes.

The reader is strict. Only the three reviewed statuses parse. A token like
`beta` or `evolving`, a `protocolVersion` other than the integer `1`, a repeated
`(kind, id)`, or an `experimental` subject that claims to be default-on fails
the build.

`src/lib/tools.ts` names each documented surface's catalog subject
(`supportSubject`) and never its value. Statuses are resolved server-side in
`src/app/docs/loader.ts`, which keeps `tools.ts` client-safe for the islands. A
surface the catalog does not classify carries `supportSubject: null` and renders
no badge. The managed platform and the two `sites/` projects are such surfaces:
the support protocol classifies things a user depends on, not services Putnami
operates.

The page is written before static paths are enumerated, from the callback that
materializes content bundles, so cold and warm builds publish the same page set
and route digest.

## Rejected alternatives

- **Keep a hand-written list and test it against the catalog.** Every
  classification would still need two edits.
- **Import the catalog from the client-safe module.** Reaches outside the
  project root and ships the whole catalog to the browser.
- **Commit the generated page.** It would drift the moment a classification
  changed without a rebuild.
- **Also write the page into the source `public/docs` mirror.** The
  static-files and public-surface plugins would both declare the route and fail
  with `http_routes.duplicate_route`.
- **Invent a `site` support kind.** A wire change to a closed vocabulary that
  turns a promise about artifacts into one about hosted services.
- **Publish what could be read from a malformed catalog.** A missing row reads
  as "not supported".

## Consequences

- A catalog edit changes a published page with no site edit, so a careless
  edit is immediately public.
- The catalog is part of this project's generate cache key, so a classification
  change invalidates the site build.
- A malformed catalog breaks the site build as well as the support protocol's
  gate.
- A surface without a badge gets one by being classified, never by a site edit.
- The status vocabulary here is closed at three values; a fourth needs a
  support-protocol version bump first.
