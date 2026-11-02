# Translation catalog and glossary formats

The files a localized `@putnami/web` project keeps under `locales/`:

| File | Schema | Holds |
|---|---|---|
| `locales/<locale>/<namespace>.json` | [`schemas/catalog.json`](schemas/catalog.json) | One namespace's translations in one locale: per message key, its text, the default text it was made from (`source`) and its state |
| `locales/<locale>.glossary.json` | [`schemas/glossary.json`](schemas/glossary.json) | The terms of one locale: per default-locale term, its translation, or `null` to keep it as written, and a note |

An entry's state is `missing` (nobody translated it), `draft` (an agent or a
contributor wrote it), `validated` (a person locked it, with `validatedBy` and
`validatedAt`) or `stale` (its default text changed since). `putnami i18n sync`
writes the catalogs, each naming its schema in `$schema`; putnami.dev publishes
both schemas at their `$id`.

`fixtures/` holds the corpus both sides agree on: every valid file is read by
`@putnami/web`, and every invalid one is refused for the reason its name
states.

## Ownership, support, and evidence

**Owner** — the `protocols` standardization layer. `protocols/i18n`
(`go.putnami.dev/protocol/i18n`) is the single owning project: the two
schemas and the fixture corpus change here first, and `@putnami/web` follows.

**Support status** — `preview` in the workspace-root
[`putnami.support.json`](../../putnami.support.json) catalog, whose vocabulary
[`protocols/support`](../support/README.md) defines. Public and usable, but not
yet carrying a stable cross-implementation commitment.

**Evidence for that status**

- `sites/putnami.dev` keeps its French catalogs in this format, and its build
  reads them through `@putnami/web`.
- The Go test checks every fixture against its schema, and the `@putnami/web`
  test reads the same fixtures: both accept the valid ones and refuse the
  invalid ones.
- What is *missing* for `stable`, against the protocol rows of
  [ADR 0002](../support/doc/adr/0002-promotion-and-demotion-criteria.md):
  a consumer across a boundary this repository does not control (every
  reader today is `@putnami/web`), and a pinned version anchor asserted by a
  test (the schemas carry no version, and `i18n.go` names none), and an N-1
  read window or a migration documented for a format change.
