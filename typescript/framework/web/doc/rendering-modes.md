# Rendering Modes — SSG, ISR & Islands

`@putnami/web` lets you declare **how** and **how much** each route renders, explicitly and per route. Two orthogonal axes plus a determinism contract:

- **WHEN it renders** — SSR (default), SSG (static), or ISR (revalidated static).
- **WHAT hydrates** — full-page hydration (SSR pages), component **islands**, or nothing (zero JS).
- **Determinism** — the mode is exactly what the builder declares and is **never inferred**. A static route that touches request data is a hard build error, never a silent downgrade.

> Design principle: the rendering mode is declared explicitly and is reviewable in a diffable manifest — the opposite of "calling `cookies()` silently makes the route dynamic".

## Axis 1 — WHEN it renders

| Builder | Mode | Behaviour |
|---|---|---|
| `page().render()` | SSR | Rendered per request (default). |
| `page().cache({ ttl }).render()` | SSR + server cache | Per-request render, cached server-side. |
| `page().static().render()` | **SSG** | Rendered once at build, emitted as static HTML. |
| `page().static({ revalidate: 60 })` | **ISR** | Static, re-rendered when stale (every 60s). |
| `page().static({ revalidate: { tags: ['posts'] } })` | **ISR** | Static, re-rendered when the `posts` tag is revalidated. |
| `page().static({ paths })` | **SSG/ISR** | Dynamic routes enumerate concrete params at build. |

### Static Site Generation (SSG)

```tsx
// src/app/about/page.tsx
import { page } from '@putnami/web';

function About() {
  return <main><h1>About us</h1></main>;
}

// Pre-rendered at build time. Ships zero client JavaScript.
export default page().static().render(About);
```

At build, the page (composed with its layouts) is rendered to a complete HTML
document and written to `.gen/<public>/static/about.html`. At runtime the route
serves that file directly — no per-request React render.

### Build-time loaders

Pair `loader().static()` with a static page to run data fetching **at build**
and bake the result into the HTML:

```ts
// src/app/blog/loader.ts
import { loader } from '@putnami/web';

export default loader()
  .static()
  .handle(async () => ({ posts: await listPosts() }));
```

```tsx
// src/app/blog/page.tsx
import { page } from '@putnami/web';
export default page().static().render(BlogIndex);
```

The build version is the one exception to baking. When a pre-rendered page
shows the `getBuildInfo()?.version` that loaders read, the build writes it to
`.gen/<public>/static/.build-version.json`. The server replaces that version with
its own when it serves a pre-rendered page at its route. A page restored from
the build cache, or served by an image deployed under another release id, shows
the running version. A bare release tag such as `1.0.0` is not replaced, because
page content can contain the same text.

### Dynamic routes — `paths()`

For routes with `[param]` segments, enumerate the concrete params to pre-render.
`paths()` is a **pure** build-time function:

```tsx
// src/app/blog/[slug]/page.tsx
import { page } from '@putnami/web';

export default page()
  .static({ paths: async () => (await listPosts()).map((p) => ({ slug: p.slug })) })
  .render(PostPage);
```

Each enumerated path is emitted as its own HTML file (`blog/hello.html`, …).
Requests for a path that was **not** enumerated fall back to a live static render.

### Incremental Static Regeneration (ISR)

ISR keeps the speed of static while staying fresh. It reuses the framework's
existing TTL + tag cache.

```tsx
// Revalidate at most every 60 seconds
export default page().static({ revalidate: 60 }).render(NewsPage);

// Revalidate on demand when a content tag changes
export default page().static({ revalidate: { tags: ['posts'] } }).render(PostPage);
```

Revalidate a tag from an action (e.g. after a publish):

```ts
import { action, revalidateTag } from '@putnami/web';

export default action().handle(async (ctx) => {
  await publishPost(await ctx.body());
  await revalidateTag('posts'); // affected ISR pages re-render on next request
  return { ok: true };
});
```

The build output seeds the cache; once the TTL lapses or a tag is revalidated,
the next request re-renders fresh static HTML.

## Axis 2 — WHAT hydrates (Islands)

A static page ships **zero JavaScript by default**. Interactivity comes from
**islands** — components declared in `*.island.tsx` files that hydrate
independently.

```tsx
// src/app/Counter.island.tsx
import { useState } from 'react';
import { island } from '@putnami/web';

function Counter({ start = 0 }: { start?: number }) {
  const [n, setN] = useState(start);
  return <button type="button" onClick={() => setN((v) => v + 1)}>Clicked {n} times</button>;
}

// Hydrate when scrolled into view.
export default island().load('visible').render(Counter);
```

Use it like any component:

```tsx
// src/app/page.tsx
import { page } from '@putnami/web';
import Counter from './Counter.island';

function Home() {
  return <div><h1>Welcome</h1><Counter start={0} /></div>;
}

export default page().static().render(Home);
```

The page renders to static HTML; only the `Counter` island ships and hydrates
its JavaScript. Every other element is inert HTML.

### Hydration strategies

| Strategy | Hydrates when | API |
|---|---|---|
| `load` | immediately on load | `island().load('load')` (default) |
| `idle` | the browser is idle (`requestIdleCallback`) | `island().load('idle')` |
| `visible` | scrolled into view (`IntersectionObserver`) | `island().load('visible')` |
| `media` | a media query matches (`matchMedia`) | `island().media('(min-width: 768px)')` |

### Slots — static children inside an island

An island can wrap static (non-interactive) children with `<Slot />`. The
slotted HTML is preserved from the server without shipping its JavaScript:

```tsx
// src/app/Disclosure.island.tsx
import { useState } from 'react';
import { island, Slot } from '@putnami/web';

function Disclosure() {
  const [open, setOpen] = useState(false);
  return (
    <div>
      <button type="button" onClick={() => setOpen((v) => !v)}>Toggle</button>
      {open && <Slot />}
    </div>
  );
}

export default island().render(Disclosure);
```

```tsx
<Disclosure>
  <p>This static markup is preserved and never ships JS.</p>
</Disclosure>
```

### How islands ship

`putnami build` emits one islands runtime bundle plus a per-island code-split
chunk. A static page that contains island markers loads the runtime, which scans
the page and hydrates only the islands actually present — unused islands are
never downloaded. SSR pages keep classic full-page hydration; islands inside
them render inline and hydrate with the page.

### Boundaries (by design)

- Island props must be JSON-serializable (they cross the server→client boundary).
- Islands don't share React context with each other (same as Astro).
- Islands run on the client — no server DI or request scope inside them.

## Determinism manifest

Every build writes `.gen/putnami-web-manifest.json` describing each route:

```json
{
  "version": 1,
  "routes": [
    { "route": "/", "mode": "ssg", "hydration": "islands", "clientJsBytes": 76787, "diProvenStatic": true },
    { "route": "/tasks", "mode": "ssr", "hydration": "full", "clientJsBytes": 99029 }
  ],
  "islands": [{ "id": "Counter", "strategy": "visible" }],
  "bundles": { "hydrateBytes": 99029, "islandsBytes": 76787 },
  "totals": { "routes": 2, "ssr": 1, "ssg": 1, "isr": 0, "islands": 1, "maxClientJsBytes": 99029 }
}
```

For static (`ssg`/`isr`) routes, `diProvenStatic` records **how** the
static-means-static guarantee is enforced (see below): `true` when it is proven
from the DI dependency graph, `false` when the route is a hybrid that the
runtime guard backstops.

A build-log table summarises the same data:

```
Route    Mode  Hydration  Client JS
-------  ----  ---------  ---------
/        SSG   islands    75.0 KB
/tasks   SSR   full       96.7 KB
```

### Diffing in CI

Commit a baseline manifest and gate pull requests on rendering changes:

```bash
bunx putnami-web-manifest-diff baseline-manifest.json .gen/putnami-web-manifest.json
```

The command prints added / removed / changed routes (mode, hydration, JS budget)
and exits non-zero when anything changed, so an unexpected SSG→SSR downgrade or a
client-JS regression fails the build for review.

### Static-means-static, proven from the DI graph

The runtime guard above is a **tripwire**: it only fires if the offending code
path happens to run during the pre-render pass. Putnami also proves the
guarantee **statically, from the DI dependency graph** — before any rendering,
independent of which branches run.

Request- and session-scoped state reaches a route only through the DI container:
a page loader or matched layout loader pulls it in with `.inject()`, and
providers declare their lifecycle `scope` and dependencies. So for a `.static()`
route the framework can walk those loaders' injected providers and their
transitive `deps` and decide whether any of them is request/session-scoped — a
property of the graph, not of runtime execution:

```ts
// A request-scoped provider…
application().provide(SessionContext, { scope: 'scoped' });

// …injected (directly or transitively) into a static route's loader is a
// build-time StaticRenderViolation naming the route, the provider, and the
// resolution path — e.g.
//   Static route "/dashboard" resolves request/session-scoped provider
//   `SessionContext` through its DI graph (DashboardData → SessionContext).
export default loader().static().inject({ data: DashboardData }).handle(/* … */);
```

Because it is a property of the graph, the proof is **complete**: it has no
false negatives from untaken code paths, which is the gap in a pure runtime
tripwire.

Where the graph is not fully decidable — a factory provider that resolves
dependencies it does not declare (mark a complete factory with
`provide(Token, factory, { deps, depsComplete: true })` to opt back in), or a
loader that injects via a tag/filter selector or cannot be inspected at build
time — the route is treated as a **hybrid**: not proven dynamic, but the runtime
`StaticRenderViolation` guard remains the backstop. The manifest's
`diProvenStatic` flag distinguishes the two (`true` = proven from the graph,
`false` = hybrid), so an unexpected proven→hybrid regression is visible in the
manifest diff.

## The determinism contract

1. A route's mode is **exactly** what its builder declares — nothing inside
   `render`/`handle` can change it. A `.static()` route (or its `.static()`
   loader) that reads request-scoped data (`ctx.user`, `ctx.headers`,
   `ctx.queryParams`, cookies, …) throws a **`StaticRenderViolation`** at build:
   a hard error, never a silent downgrade to SSR.
2. A `.static()` route whose DI graph reaches a request/session-scoped provider
   is the same hard **`StaticRenderViolation`**, proven from the graph before
   rendering — not just caught if a code path happens to touch the request.
3. The only things that hydrate are `*.island.tsx` boundaries.
4. The per-route manifest makes mode, hydration, JS budget and the
   `diProvenStatic` proof status reviewable in CI.
