# Web

Full React server-side rendering with data flow.

## Features

- Pages & layouts (`page.tsx`, `layout.tsx`, `not-found.tsx`, `error.tsx`)
- Loaders (`loader.ts`) for server-side data fetching
- Actions (`action.ts`) for form submissions/mutations
- Dynamic route segments with `[id]`
- `useLoaderData()` hook for accessing loader data
- Navigation between pages
- **Static rendering (SSG)** — the home page (`page().static()`) is pre-rendered to zero-JS HTML
- **Islands** — `Counter.island.tsx` hydrates on its own (`visible` strategy) on the static home page
- Manifest-driven packaged SSR — the late `@putnami/web` pre-build hook's
  `react-loader` is reconciled into `capabilities.json` and statically bundled,
  so runtime does not need `.gen/src`
- Secure-by-default actions — each form renders `<CsrfInput />`, so the same
  double-submit token protects enhanced and no-JavaScript POSTs
- **Analytics without a database** — `analytics()` composed with no `sql()`

## Pages

| Path | Description |
|------|-------------|
| [http://localhost:3903/](http://localhost:3903/) | Welcome page (SSG + a `visible` island) |
| [http://localhost:3903/tasks](http://localhost:3903/tasks) | Task list with loader |
| [http://localhost:3903/tasks/[id]](http://localhost:3903/tasks/[id]) | Task detail with toggle action |
| [http://localhost:3903/tasks/new](http://localhost:3903/tasks/new) | Create task form with action |

## Run

```bash
putnami serve .
```

`putnami build . --compile` consumes the reconciled capability manifest when
generating the bundled entrypoint. The sample test bundles that exact
entrypoint, runs it from an isolated directory with no `.gen/src`, and verifies
that `/tasks` still renders through SSR.

## Try It

Once the server is running, open your browser:

**1. Visit the welcome page:**

Open [http://localhost:3903/](http://localhost:3903/) in your browser.

Expected result — a welcome page with links to the features list and a "View Tasks" link.

**2. Browse the task list:**

Click "View Tasks" or navigate to [http://localhost:3903/tasks](http://localhost:3903/tasks).

Expected result — a list of tasks fetched server-side by the loader. Each task links to its detail page.

**3. Create a new task:**

Navigate to [http://localhost:3903/tasks/new](http://localhost:3903/tasks/new).

Expected result — a form with a title field. Submit the form and you are redirected to the task list with the new task included.

**4. View task detail and toggle status:**

Click on any task in the list.

Expected result — the task detail page showing title, status, and a toggle button. Click the toggle to change the task status via a server action.

**5. Test the 404 page:**

Navigate to [http://localhost:3903/nonexistent](http://localhost:3903/nonexistent).

Expected result — the custom `not-found.tsx` page renders.

## Analytics

This sample composes `@putnami/analytics` with **no database**, which is the
plug-and-play shape: the plugin logs

```
analytics: no sql() plugin — events are counted in metrics only and not stored
```

once at warmup, counts what it accepts in metrics, and stores nothing.
Everything else is real — the bootstrap, the tracker bundle, and the ingest
route.

- **Composition** (`src/main.ts`): `.use(analytics({ events, serverPageViews: false }))`.
- **Declared action** (`src/main.ts`): `counter_click` with a `value` property.
  It is typed `String` because `data-track-*` attribute values are always
  strings; a `Number`-typed property would be dropped on arrival.
- **Tracking** (`src/app/Counter.island.tsx`): `data-track="counter_click"
  data-track-value="1"` on the increment button. No handler, no import — one
  capturing listener on the document reads the attributes.
- **Bootstrap**: every server-rendered page carries
  `window.__putnamiBootstrap.analytics` with the page-view id, the route, the
  ingest endpoint, and the declared names — and nothing about the visitor. The
  static home page carries none: it is served as a file, so no middleware runs.
- **Ingest**: `POST /_putnami/analytics/events` answers `202` with an empty body
  for everything semantic, without a CSRF token, and tells a bot exactly the
  same thing it tells a visitor.
- **Cookies**: none. The default mode is cookieless.

`conf/.env.yaml` carries `analytics.secret` for every environment: the visitor
hash needs a server-side key, and the plugin fails closed without one — under
`putnami qualify --target local` too, which runs the sample production-mode.
The value is a sample-only placeholder; a real workload keeps its key out of
the tree.

Where the data would go, if there were a database:
[`13-fullstack-app`](../13-fullstack-app/README.md#analytics).

## Test

```bash
putnami test .
```

## What this sample proves

The home route is generated as zero-full-page-JavaScript static HTML and emits
only the `visible` counter island. The task routes are live SSR: loaders read the
server store before rendering and actions mutate it only after input validation
and default CSRF middleware. The test starts the real React plugin, proves an
unprotected POST is rejected with 403, submits the rendered `_csrf` field with
its cookie, and observes the new task through a later server-rendered list.

Contract: [`@putnami/web`](../../framework/web/README.md) —
[web-application-delivery specification](../../framework/web/specs/web-application-delivery.json).
