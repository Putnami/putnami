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
