# putnami.dev

The official Putnami website: the public documentation, the CLI install script,
the published JSON Schemas, and the generated public support catalog.

## Development

```bash
# Serve locally with hot reload
putnami serve putnami.dev
```

Open the local URL `putnami serve` prints.

## Deployment

```bash
putnami deploy putnami.dev
```

The committed `infra/runtime.json` declares the Putnami Cloud runtime and
public custom domain access used by `putnami deploy` planning. During build,
TypeScript/Bun runtime compatibility adds `runtime.protocols.http2: false` to
the aggregated deploy manifest because Bun does not serve h2c today.
The project package options set the Docker container port to 8080, which the
runtime receives as `PORT`.

The committed `infra/requirements.json` asks for one managed `postgres`
database named `analytics` — the four tables `@putnami/analytics` contributes
as its migration source. The platform applies the migration bundle before the
new revision takes traffic, which is why `sql()` runs on its default
`autoApply: false`.

One value must exist in the control plane before the first deploy with
collection on: **`analytics.secret`**, any string of 32 characters or more. It
appears as a sensitive key of the published `schema/config.json`, so it is set
where secrets are set, never in `conf/`. The plugin refuses to start without
it — a visitor hash with no rotating key anonymises nothing — so a revision
that cannot read it never takes traffic.

## Where the published content comes from

Nothing under `public/docs` is committed. Every documentation page reaching the
build has exactly one source of truth, and four different mechanisms put it
there:

| Published under | Source of truth | Mechanism |
|---|---|---|
| `/docs/00-…06-` | `sites/putnami.dev/doc/` | `putnami.json` `generate.assets` copy |
| `/docs/07-tooling-&-workspace` | `tooling/doc/` | `putnami.json` `generate.assets` copy |
| `/docs/08-frameworks/0{1,2,3}-*` | `{typescript,go,python}/doc/framework/` | `putnami.json` `generate.assets` copy |
| `/docs/08-support` | `putnami.support.json` (workspace root) | `src/plugins/support-catalog.plugin.ts` |
| `/docs/platform` | `cloud/doc-contents-platform` bundle (published by the Cloud workspace), pinned by `content.lock.json` | `src/plugins/content-bundle.plugin.ts` |
| `/schemas/…` | `protocols/*/schemas/*.json` | `src/plugins/schemas.plugin.ts` |
| `/schemas/agent-readiness/…` | `hosted-schemas/agent-readiness/*.json`, copies of the `@putnami/agent-readiness` schemas | `src/plugins/schemas.plugin.ts` |
| `/install.sh`, `/install-commands.txt`, `/install.ps1`, `/LICENSE.md` | `tooling/cli/scripts/install.sh`, `tooling/cli/scripts/install-commands.txt`, `tooling/cli/scripts/install.ps1`, `LICENSE.md` | `putnami.json` `generate.assets` copy |

A `generate.assets` source that no longer exists is not silently dropped. The
five sources another domain owns are declared imports of the `public-docs`
domain (`putnami.architecture.json`), and the build enforces them as DARC
snapshots — see [Documentation sources are contracts](#documentation-sources-are-contracts)
below. The copies the site itself owns (`sites/putnami.dev/doc/`) and the four
non-documentation copies (`install.sh`, `install-commands.txt`, `install.ps1`,
`LICENSE.md`) still only produce the extension's `generate asset source not
found, skipping` warning. Treat that warning as a broken page; `test/docs-sources.test.ts` fails
on it.

`hosted-schemas/` holds schemas that a published Putnami product owns and
this site serves at their `$id`. Today that is the `putnami agent-readiness`
payload and report. Their public copies live in
`intelligence/agent-readiness/schema/`. Each hosted file must stay byte-identical to
that source. `test/agent-readiness.test.ts` checks parity and each file's `$id`.
When the service changes its contract, update the embedded and hosted schemas
together.

The support page is written from the reviewed catalog on every build, so a
package or protocol status can only change by changing `putnami.support.json`.
The site keeps no second list; `src/lib/tools.ts` names the catalog subject for
each documented surface and never restates its status.

Generated output lands only in `.gen/public/…`. Mirroring a docs section into
the source `public/docs` makes the static-files plugin and the public-surface plugin
declare the same route and fails the build with `http_routes.duplicate_route`.

### Documentation sources are contracts

Five of the copies above publish documentation another domain owns. Each one is
a declared import of the `public-docs` domain in `putnami.architecture.json`, and
`src/lib/darc/` turns that declaration into the build's configuration:

| Import | Source root | Section |
|---|---|---|
| `public-docs.workspace-documentation.v1` | `/tooling/doc` | `08-tooling-&-workspace` |
| `public-docs.method-documentation.v1` | `/tooling/sdd-extension/doc` | `07-spec-driven-development` |
| `public-docs.typescript-framework-documentation.v1` | `/typescript/doc/framework` | `09-frameworks/01-typescript` |
| `public-docs.go-framework-documentation.v1` | `/go/doc/framework` | `09-frameworks/02-go` |
| `public-docs.python-surface-documentation.v1` | `/python/doc/framework` | `09-frameworks/03-python` |

Each is a `snapshot`: `src/plugins/docs-prerender.plugin.ts` observes the source
tree into the section's `Snapshot`, then publishes the file inventory
`Snapshot.latest()` hands back. Because the import declares
`onMissing: fail-closed`, a source that is gone was never attached and `latest()`
throws `DarcError('missing')` — the build fails instead of the section quietly
disappearing.

A version is the tree's identity: a SHA-256 over the carrier name
(`putnami.documentation-tree.v1`) followed by the sorted `(relative path,
SHA-256 of contents)` pairs of its markdown files. Same tree in, same version
out, cold build or warm. Re-attaching a version with different content is
refused (`DarcError('immutable')`). `maxStaleness: 720h` with
`onStale: use-stale` is the deployment half: a container serving a month-old
copy keeps serving it and stamps the record `stale` rather than refusing.

The sixth contract, `public-docs.application-runtime.v1`, is a `reference` on the
TypeScript framework runtime the site is built on. Nothing is copied, so the only
promise is minimization: the import names one fact, and asking for another throws
`DarcError('fact-not-imported')`.

`src/main.ts` registers all six through `DarcPlugin`, which contributes one
`domainAccess` row per contract to the capability manifest.
`putnami architecture validate` joins those rows to the declarations. A row is
evidence, never authority — emitting one cannot create a cross-domain permission.

## Endpoints

| Path | Description |
|------|-------------|
| `/` | Landing page |
| `/docs` | Documentation hub |
| `/docs/support` | Generated public support catalog |
| `/install.sh` | Install script (`curl -fsSL https://putnami.dev/install.sh \| bash`) |
| `/agent-readiness/method` | Redirect (302) to the agent-readiness method page under `/docs/platform/intelligence` |
| `/install.sh?run=<command>` | The same script with `<command>` set in its one `RUN_COMMAND_DEFAULT=""` line (`src/plugins/installer-run.plugin.ts`); a `run` value that is not one `^[a-z][a-z0-9-]{0,63}$` is a 400 |
| `/install-commands.txt` | Command map the installers read for `?run=`: which extension provides each command. Unsigned, trusted as far as `install.sh` or `install.ps1` from the same origin |
| `/install.ps1` | Windows install script (`irm https://putnami.dev/install.ps1 \| iex`, typed in a PowerShell window) |
| `/install.ps1?run=<command>` | The same script with `<command>` set in its one `$RunCommandDefault = ''` line (`src/plugins/installer-run.plugin.ts`); a `run` value that is not one `^[a-z][a-z0-9-]{0,63}$` is a 400 |
| `/dl/:artifact` | Binary resolver redirect (302) using query params (`version`, `target`, `platform`) |
| `/llms.txt`, `/doc-markdown` | Agent-facing documentation index and raw markdown |
| `/release.json` | The Putnami version the CLI's `latest` channel points at, shown in the footer |
| `/privacy` | What the site measures, how visitors are counted, and how to object |
| `/analytics/analytics.<hash>.js` | Browser tracker bundle, a build output of `@putnami/analytics` |
| `/_putnami/analytics/events` | Tracker ingest (`POST`, CSRF-exempt, rate-limited) |

## Public route surface

The build emits a canonical `putnami.http-routes.v1` inventory of everything
this workload serves at `.gen/schema/http-routes.json` (build output — never
committed). Putnami Cloud consumes it to generate a default-deny edge
allowlist, so it must stay a *complete* description of the served surface.

This workload enables that policy explicitly with
`@putnami/cloud.deploy.enforceHttpRoutes: true` in `putnami.json`. The provider
compacts routes into method-aware prefixes, so exact documentation routes that
the public `/docs/` prefix already admits are not repeated in the provider URL
map.

```bash
# Regenerate the artifact
putnami build --projects putnami.dev

# Inspect it
jq '.routes[] | "\(.match) \(.path) \(.methods|join(",")) public=\(.publicEdge)"' \
  .gen/schema/http-routes.json
```

Canonicalization makes the bytes and `digest` a pure function of the route
facts — fragment and route order cannot change them. The docs static-path
callback materializes lock-pinned content bundles before it enumerates pages,
so the first build from a cold `.gen` emits the same per-page `exact` routes
and digest as every later build. The application generate hook repeats that
idempotent materialization later to register the generated files as assets.

Route facts are merged from per-emitter fragments under
`.gen/http-routes.d/` and canonicalized by the TypeScript extension:

| Fragment | Emitter | Contributes |
|----------|---------|-------------|
| `web.json` | `@putnami/web` | file-routes from `src/app/` (`/`, `/docs`, and the enumerated `/docs/[...page]` expansion) plus the hashed client-bundle prefixes `/react/` and `/react-islands/` |
| `application.json` | `@putnami/application` | typed API routes from `src/api/`, and the static mounts/files found in the source `public/` folder (`/favicon.ico`, `/robots.txt`, `/assets/`, `/fonts/`, `/schemas/`). The search index is written after that scan, only under `.gen/public/search/`, so it is not one of them |
| `application.json` | `src/plugins/public-surface.plugin.ts` | everything no framework emitter can see: the `putnami.json` `generate.assets` copies (`/install.sh`, `/install-commands.txt`, `/install.ps1`, `/LICENSE.md`, `/docs/`), the SSG mirror `/static/`, and the routes registered at warmup (`/search/index.json`, `/api/content/refresh`, `/_/livez`, `/_/healthz`, `/_/readyz`, `/_/version`) |

Two entries are `publicEdge: false` — represented so a validator can prove no
public prefix admits them, but denied at the edge:

- `/_/livez`, `/_/healthz`, `/_/readyz`, and `/_/version` — operational platform endpoints, not site content.
- `/static/` — the `@putnami/web` SSG/ISR prerender output. The SSR reads it
  from disk; every byte is a duplicate of a page already served at its
  canonical URL, so it must not be a second public URL space.

### Fallback behavior is documented, not routed

The app renders a styled 404 page for any unmatched path, and
`/docs/[...page]` renders one for any unmatched path under `/docs/`. That
fallback is a **rendering** decision and is deliberately *not* encoded as a
route: there is no root `/` prefix and no `{rest...}` catch-all in the
inventory, because either would hand the edge an allow-everything matcher.
The inventory uses bounded prefixes (`/assets/`, `/docs/`, `/fonts/`,
`/react/`, `/react-islands/`, `/schemas/`, `/static/`) and
enumerated exact files, so unknown paths are rejected at the edge and never
reach the 404 renderer.

### Real-client probe matrix

Expected disposition of a request arriving at the public edge once the
inventory is enforced. `test/http-routes.test.ts` asserts this table and the
emitted artifact agree, so it cannot drift.

| Path | Method | Disposition | Admitted by |
|------|--------|-------------|-------------|
| `/` | GET | allow | `exact /` |
| `/` | HEAD | allow | `exact /` |
| `/` | POST | reject | — |
| `/docs` | GET | allow | `exact /docs` |
| `/docs` | DELETE | reject | — |
| `/docs/getting-started` | GET | allow | `prefix /docs/` |
| `/docs/frameworks/go/caching` | GET | allow | `prefix /docs/` |
| `/docs/01-getting-started/index.md` | GET | allow | `prefix /docs/` |
| `/docs2` | GET | reject | — |
| `/install.sh` | GET | allow | `exact /install.sh` |
| `/install.sh` | PUT | reject | — |
| `/install-commands.txt` | GET | allow | `exact /install-commands.txt` |
| `/install-commands.txt` | POST | reject | — |
| `/install.ps1` | GET | allow | `exact /install.ps1` |
| `/install.ps1` | PUT | reject | — |
| `/LICENSE.md` | GET | allow | `exact /LICENSE.md` |
| `/llms.txt` | GET | allow | `exact /llms.txt` |
| `/llms.txt` | PATCH | reject | — |
| `/sitemap.xml` | GET | allow | `exact /sitemap.xml` |
| `/doc-markdown` | GET | allow | `exact /doc-markdown` |
| `/release.json` | GET | allow | `exact /release.json` |
| `/release.json` | POST | reject | — |
| `/dl/putnami` | GET | allow | `template /dl/{artifact}` |
| `/dl` | GET | reject | — |
| `/robots.txt` | GET | allow | `exact /robots.txt` |
| `/favicon.ico` | GET | allow | `exact /favicon.ico` |
| `/assets/putnami-logo.svg` | GET | allow | `prefix /assets/` |
| `/assets` | GET | reject | — |
| `/fonts/inter-latin.woff2` | GET | allow | `prefix /fonts/` |
| `/schemas/putnami-project.json` | GET | allow | `prefix /schemas/` |
| `/schemas/agent-readiness/payload.v1.json` | GET | allow | `prefix /schemas/` |
| `/agent-readiness/method` | GET | allow | `exact /agent-readiness/method` |
| `/agent-readiness/method` | POST | reject | — |
| `/search/index.json` | GET | allow | `exact /search/index.json` |
| `/react/chunk.abc12345.js` | GET | allow | `prefix /react/` |
| `/react-islands/islands.abc12345.js` | GET | allow | `prefix /react-islands/` |
| `/api/content/refresh` | POST | allow | `exact /api/content/refresh` |
| `/api/content/refresh` | GET | reject | — |
| `/_/livez` | GET | reject | — |
| `/_/healthz` | GET | reject | — |
| `/_/readyz` | GET | reject | — |
| `/_/version` | GET | reject | — |
| `/static/index.html` | GET | reject | — |
| `/.env` | GET | reject | — |
| `/.git/config` | GET | reject | — |
| `/.aws/credentials` | GET | reject | — |
| `/wp-admin` | GET | reject | — |
| `/wp-login.php` | GET | reject | — |
| `/admin` | GET | reject | — |
| `/phpmyadmin` | GET | reject | — |
| `/index.php` | GET | reject | — |
| `/actuator/health` | GET | reject | — |
| `/server-status` | GET | reject | — |
| `/%2E%2E%2Fetc%2Fpasswd` | GET | reject | — |

`allow` means the edge forwards the request; the app still applies its own
status codes and the `POST /api/content/refresh` OIDC guard, which fails
closed with a 401.

## Audience measurement

The site measures its own audience with `@putnami/analytics`: cookieless by
default, stored in the site's own database, with no third party and no egress.
`conf/.env.yaml` keeps it **off** everywhere and `conf/env.prod.yaml` (the `prod`
environment the platform deploys to)
turns it on, so `putnami serve` needs neither a Postgres nor a key.

Every page here is `page().static()`, which is the case the package had listed
as a non-goal: pre-rendered HTML is produced at build time, with no request, so
the renderer writes no tracker into it. The page-view middleware now injects it
into the response instead, carrying that request's own page-view id — see
`typescript/framework/analytics/doc/adr/0005-pre-rendered-pages-get-the-tracker-at-serve-time.md`.

`src/analytics.ts` is the whole action vocabulary. `/privacy` names every entry
of it in plain language, and `test/privacy.test.ts` fails if the two drift or
if the site tracks a name it never declared.

## Structure

```
src/
├── main.ts      # App definition (http + react + static + search index + analytics)
├── analytics.ts # The declared action vocabulary, and all of it
├── app/         # Website routes (React pages)
├── api/         # API routes (e.g. sitemap)
├── components/  # Shared UI components
└── lib/
    ├── darc/    # The public-docs domain's declared imports, as components
    └── …        # Markdown/docs/search utilities
public/
├── docs/        # Built documentation content
├── install.sh   # CLI install script
├── install-commands.txt # Command map for install.sh?run=<command> and install.ps1?run=<command>
├── install.ps1  # CLI install script for Windows
└── search-index.json
```
