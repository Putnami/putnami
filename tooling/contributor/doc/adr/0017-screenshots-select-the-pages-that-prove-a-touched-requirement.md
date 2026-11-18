# ADR 0017 — Screenshots select the pages that prove a touched requirement

- **Status**: accepted
- **Date**: 2026-11-17
- **Scope**: `@putnami/contributor` (`change screenshots`, `change finalize`)
  and `go.putnami.dev/protocol/screenshots`
- **Amends**: [ADR 0014](0014-screenshots-are-the-proof-of-a-web-change.md),
  decisions 1 and 4
- **Follows**: the `proves` export of a `@putnami/web` page, which the build
  writes into the page's routes of `schema/http-routes.json`

## Context

Under ADR 0014, any changed file under a web project's `src/` that is no
route's evidence touches every page. A change to a shared component of an
application with 45 pages captures 360 files in about five and a half
minutes, and an animated element makes the before and the after of an
unchanged page differ. The reader gets many pictures and no reason to look at
any one of them.

A page can now name the feature requirements it proves, and the route
inventory carries them with a concrete path, `at`, that renders the page with
fixture data. The requirements a change touches are visible in its feature
declarations.

## Decision

1. **A page is captured when it proves a touched requirement, when its own
   page file changed, or when the run names its route.** A touched
   requirement is one the change adds, or whose declaration it changes, in a
   `putnami.features.json` or a `features/<feature>.json` of the workspace,
   between the merge base and the head. A page route is selected when its
   `proves`, at the head, names one. A changed shared file selects no page.
2. **A touched page is visited at its proof.** The `--fixtures` values of the
   route come first, then the `at` of a touched requirement the page proves,
   then the `at` of its first proof.
3. **Animations do not move between two captures.** Before each capture, the
   command stops every animation of the page at its end, so an entrance
   shows the content it brings in, and an endless one, such as a spinner, at
   its start; transitions jump to their end value.
4. **The manifest names what each page proves.** Manifest version 2 adds
   `pages[].proves`, the requirements the page proves at the head. Readers
   accept versions 1 and 2. `change finalize` requires a version 2 manifest
   and, for each touched requirement a page proves, that page with its four
   after captures and the requirement in its `proves`.

## Consequences

- A change to a shared component captures only the pages its declared
  requirements name. Its effect on another page is not pictured unless the
  run names that page with `--route`: a requirement the change does not
  touch is not the change's proof.
- A web change shows a requirement on screen only if a page declares that it
  proves it. A page with no `proves` is captured only when its own file
  changes.
- A change to the prose of a requirement in `specs/` alone touches no
  requirement: the declaration in the feature file is what changes the
  requirement's check.
- A manifest written before version 2 still decodes, and a stored one still
  reads; finalize asks for a new one.

## Alternatives rejected

- **Keep every page for a shared file, and add the proving pages.** It keeps
  the cost and the noise the change exists to remove.
- **Follow the imports of each page to the changed file.** It needs a module
  graph per side and still captures pages whose look did not change.
- **Record only the touched requirements in the manifest.** A consumer then
  cannot show a capture as the proof of the other requirements the page
  proves.
