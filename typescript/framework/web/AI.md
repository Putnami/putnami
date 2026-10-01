# @putnami/web

React SSR with file-based routing, server-side data loading, and progressive enhancement.

## Browser / server boundary

The package publishes two entries and they never share code:

| Export condition | Entry | Contains |
|------------------|-------|----------|
| `browser` (and any browser-like loader) | `src/index.browser.ts` → `src/client/**` | Client components, hooks, router, DOM document helper |
| `default` (bun/node, and the `bin/*` executables) | `src/index.ts` → `src/ssr/**` | SSR renderer, plugin, loaders/actions, static generation |

The boundary is enforced by the packaging tooling, not by convention: the
TypeScript extension partitions entrypoints by export condition and runs one
`bun build` per graph, so `--splitting` can only emit shared chunks *inside* a
graph. Server-only code therefore cannot land in a chunk the published browser
entry imports, even though `src/ssr/index.ts` legitimately re-exports client
components. The browser graph is built with bun's `browser` target, so the
`browser` export condition and the package.json `browser` field both apply.

Consequences when working in this package:

- Anything importing a server-only `@putnami/runtime` API (`useContext`,
  `runInContext`, `useLogger`, ...) must stay reachable only from
  `src/index.ts`. Adding such an import under `src/client/**` breaks the
  browser build loudly instead of shipping a broken bundle.
- The package.json `browser` map (`document.helper.ts` →
  `document.helper.browser.ts`) still matters in source/dev mode, where
  workspace consumers resolve `@putnami/web` to `src/*.ts` and run their own
  client build. It is a per-module redirect, not the containment mechanism.
- `test/entrypoints.test.ts` mirrors the published shape and fails if an SSR
  symbol or a server-only runtime import becomes reachable from the browser
  entry closure.

## Setup

```ts
import { application, http } from '@putnami/application';
import { react } from '@putnami/web';

export const app = () =>
  application()
    .use(http())
    .use(react());  // Enables React SSR with file-based routing
```

## File-Based Routing

Pages live in `src/app/` — the directory structure defines routes:

```
src/app/
  layout.tsx              -> Root layout (wraps all pages)
  page.tsx                -> /
  about/page.tsx          -> /about
  posts/
    page.tsx              -> /posts
    loader.ts             -> Server data for /posts
    [id]/
      page.tsx            -> /posts/:id
      loader.ts           -> Server data for /posts/:id
      action.ts           -> POST handler for /posts/:id
  error.tsx               -> Error boundary
  not-found.tsx           -> 404 page
```

### File Conventions

| File | Purpose |
|------|---------|
| `page.tsx` | Route component |
| `layout.tsx` | Layout wrapper (uses `<Outlet />` for children) |
| `loader.ts` | Server-side data loading (runs before render) |
| `action.ts` | Form POST handler |
| `error.tsx` | Error boundary for this route segment |
| `not-found.tsx` | 404 component |
| `[param]/` | Dynamic route segment |

## Page Builder

```tsx
import { page } from '@putnami/web';

export default page().render(function MyPage() {
  return <div>Hello</div>;
});
```

With security:
```tsx
export default page()
  .secure({ roles: ['admin'] })
  .render(function AdminPage() { ... });
```

## Rendering Modes — SSG / ISR / Islands

Declare how each route renders. The mode is explicit and never inferred.

```tsx
// SSG — pre-rendered at build, zero client JS
export default page().static().render(About);

// ISR — revalidate every 60s, or on a cache tag
export default page().static({ revalidate: 60 }).render(News);
export default page().static({ revalidate: { tags: ['posts'] } }).render(Post);

// SSG for dynamic routes — enumerate params at build (pure fn)
export default page()
  .static({ paths: async () => (await listPosts()).map((p) => ({ slug: p.slug })) })
  .render(Post);

// Loader runs at build and is baked into the static HTML
export default loader().static().handle(async () => ({ posts: await listPosts() }));
```

Islands — interactive components in `*.island.tsx` that hydrate independently
(strategies: `load` | `idle` | `visible` | `media`). A static page ships only
the islands it uses:

```tsx
// src/app/Counter.island.tsx
import { island } from '@putnami/web';
export default island().load('visible').render(Counter);
```

`revalidateTag('posts')` re-renders affected ISR pages. `Slot` preserves static
children inside an island. Every build emits a diffable
`.gen/putnami-web-manifest.json` (route · mode · hydration · client-JS budget);
`putnami-web-manifest-diff <a> <b>` gates rendering changes in CI. A `.static()`
route that touches request data is a hard build error (`StaticRenderViolation`).

See `doc/rendering-modes.md` for the full guide.

## Layout Builder

```tsx
import { layout, Outlet } from '@putnami/web';

export default layout().render(function RootLayout() {
  return (
    <html lang="en">
      <head><title>My App</title></head>
      <body>
        <nav>...</nav>
        <Outlet />
      </body>
    </html>
  );
});
```

## Loader (Server Data)

Loaders run on the server before rendering. Data is serialized and available to components:

```ts
import { loader } from '@putnami/web';

// Simple
export default loader(async (ctx) => {
  const posts = await fetchPosts();
  return { posts };
});

// With DI injection
export default loader()
  .inject({ db: PostRepository })
  .handle(async ({ db }, ctx) => {
    return { posts: await db.find({}) };
  });

// With query params
export default loader()
  .query({ page: Optional(Int) })
  .inject({ posts: PostService })
  .handle(async ({ posts }, ctx) => {
    return { items: await posts.list(ctx.queryParams().page ?? 1) };
  });
```

Access in components:
```tsx
import { useLoaderData } from '@putnami/web';

export default function PostsPage() {
  const { posts } = useLoaderData<{ posts: Post[] }>();
  return <ul>{posts.map(p => <li key={p.id}>{p.title}</li>)}</ul>;
}
```

## Action (Form Handling)

Actions handle form POST submissions with progressive enhancement:

```ts
import { action } from '@putnami/web';

// Simple
export default action(async (ctx) => {
  const form = await ctx.req.formData();
  await saveContact(form.get('name'), form.get('email'));
  return { ok: true };
});

// With body validation
export default action()
  .body({ name: String, email: Email })
  .handle(async (ctx) => {
    const data = await ctx.body();
    return { ok: true };
  });

// With DI
export default action()
  .body({ name: String, message: String })
  .inject({ contacts: ContactService })
  .handle(async ({ contacts }, ctx) => {
    const data = await ctx.body();
    await contacts.save(data);
    return { ok: true };
  });
```

Form component:
```tsx
import { Form, useActionData } from '@putnami/web';

export default function ContactPage() {
  const result = useActionData<{ ok: boolean }>();
  return (
    <Form method="post">
      <input name="name" required />
      <input name="email" type="email" required />
      <button type="submit">Send</button>
      {result?.ok && <p>Sent!</p>}
    </Form>
  );
}
```

## Error Boundary

```tsx
import { error, isRouteErrorResponse, useRouteError } from '@putnami/web';

export default error().render(function ErrorPage() {
  const err = useRouteError();
  if (isRouteErrorResponse(err)) {
    return <div>{err.status} {err.statusText}</div>;
  }
  return <div>Error: {err instanceof Error ? err.message : 'Unknown error'}</div>;
});
```

## Client-Side Hooks

```tsx
import {
  useLoaderData,    // Access loader data
  useActionData,    // Access action result
  useNavigation,    // Navigation state (loading/idle)
  useParams,        // Route parameters
  useNavigate,      // Programmatic navigation
  useRouteError,    // Error in error boundaries (unknown by default)
  isRouteErrorResponse, // Narrow it to a thrown 4xx/5xx Response
} from '@putnami/web';
```

## Plugin seams

Generic, plugin-facing surfaces. Nothing in this package knows what a plugin
does with them.

| Export | Entry | Use |
|--------|-------|-----|
| `mergeClientBootstrap(ctx, entry)` | server | Shallow-merge per-request data serialized as `window.__putnamiBootstrap` in the SSR hydration script |
| `pushClientScript(ctx, url)` | server | Emit one nonce-stamped `<script type="module">` after the hydrate script, deduplicated by URL |
| `onNavigation(listener)` | browser + server | Subscribe to the `putnami:navigation` DOM event the browser router dispatches on each client route change |

`ACTION_OUTCOME_CONTEXT_KEY` carries how the last action settled
(`ok` / `validation_error` / `error`) plus its route, written by the action
handler and read after `next()` resolves. Static pages consume neither slot:
they are served as files, with no request context.

## Builder Pattern Summary

All builders support the same progressive chain:

| Builder | `.query()` | `.body()` | `.inject()` | `.secure()` | `.static()` | Finalizer |
|---------|-----------|----------|------------|------------|------------|-----------|
| `endpoint()` | Yes | Yes | Yes | Yes | — | `.handle()` |
| `loader()` | Yes | — | Yes | Yes | Yes | `.handle()` |
| `action()` | — | Yes | Yes | Yes | — | `.handle()` |
| `page()` | — | — | — | Yes | Yes | `.render()` |
| `layout()` | — | — | — | Yes | — | `.render()` |
| `error()` | — | — | — | — | — | `.render()` |
| `island()` | — | — | — | — | — | `.render()` (with `.load()` / `.media()`) |

## Detailed Documentation

See `doc/` folder:
- `getting-started.md` — project setup, first page
- `file-based-routing.md` — routing conventions
- `rendering-modes.md` — SSG, ISR, islands, the determinism manifest
- `data-loading.md` — loaders and data fetching
- `forms-and-actions.md` — form handling, progressive enhancement
- `client-side-features.md` — hooks, navigation
- `document-management.md` — head, meta, scripts
- `advanced-patterns.md` — middleware, streaming, auth
- `configuration.md` — React plugin options

## Security and failure boundaries

- Loaders and actions execute only in the server graph. Validate their declared
  inputs and resolve application services with `.inject()`.
- A layout/page `.secure()` declaration protects its SSR page and generated
  loader/action endpoints. Browser route metadata is only a UI hint; it never
  grants access, and function guards default-deny there because they cannot be
  serialized.
- React routes enable CSP/security headers and action CSRF checks by default.
  Use `<CsrfInput />` for no-JavaScript forms; enhanced clients echo the same
  cookie token in the request header.
- SSR serializes loader/action data through script-context escaping and includes
  only bounded role/scope state, never tokens or general user claims.
- Default page-cache keys include user identity and query parameters. A custom
  key must preserve every input that can change the HTML.
- Hydration failure keeps or restores the server HTML. Do not replace that
  readable fallback with a blank client root.

## Support and feature ownership

`@putnami/web` is classified `stable` and owns the modeled
[`typescript/web-application-delivery`](putnami.features.json) feature. Its
[specification](specs/web-application-delivery.json),
[render-boundary decision](doc/adr/0001-rendering-is-explicit-across-separate-runtime-graphs.md),
and [server-authority decision](doc/adr/0002-data-and-request-security-remain-server-authoritative.md)
are canonical. OAuth identity, WebSocket/SSE endpoint transport, UI styling, and
persistence are owned by other packages; do not invent parallel web feature
claims for them. No default-framework or cross-language parity status is
implied.
