# Web

Use `@putnami/web` for server-side rendering, file-based routing, server-owned
data loading and mutation, explicit static rendering, bounded islands, and
client hydration. It uses React 19 and streams the response body after the route
graph and lazy document metadata are ready.

## Getting started

### Installation

```bash
putnami deps add @putnami/web
```

### Entry point

```ts
import { application, http, staticFiles } from '@putnami/application';
import { react } from '@putnami/web';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(react())
    .use(staticFiles({ publicFolder: 'public' }));
```

### Configuration options

```ts
react({
  scanFolder: 'app',        // Folder to scan for routes (default: 'app')
  autoScan: true,           // Auto-discover routes (default: true)
  publicFolder: 'public',   // Static assets folder (default: 'public')
  ssrTimeout: 3000,         // SSR render timeout in ms (default: 3000)
  minify: true,             // Minify client bundles (default: true)
  sourcemap: 'external',    // 'none' | 'inline' | 'external' (default: 'external')
  splitting: false,         // Code splitting (default: false)
  isDevelopment: false,     // Development mode (default: NODE_ENV check)
})
```

React routes enable security headers and action CSRF validation by default.
Opting out is explicit; see [Security defaults](#security-defaults).

## Browser and server boundary

The package publishes separate default/server and browser entrypoints, built as
separate dependency graphs. The server graph owns route discovery, SSR,
loaders, actions, static generation, dependency injection, and request
middleware. The browser graph owns hydration, navigation, form enhancement,
document updates, and islands. A browser bundle cannot acquire server authority
by importing a shared route module.

`page()` and `layout()` exist in both graphs so the same component source can be
loaded on either side. In the browser, server middleware methods are inert and
warn once. Declarative `.secure()` metadata can hide unavailable UI, but it is
only a hint: the generated page, loader, and action endpoints always enforce the
real middleware on the server. Function guards cannot be serialized and
default-deny in client UI gating.

### Module path inheritance

When used inside a module with `.path()`, the React plugin automatically inherits the module path as its basename. All routes are served under the module path prefix, and client-side navigation uses it as the router basename:

```ts
const dashboard = module('dashboard')
  .path('/dashboard')
  .use(react());          // Pages served at /dashboard, /dashboard/settings, etc.

const app = application()
  .use(http({ port: 3000 }))
  .use(dashboard);
```

## File-based routing

Routes are automatically discovered from your `src/app/` folder. Each folder becomes a route segment, and special files define the route behavior.

### Route files

- **page.tsx** - React component for the route
- **loader.ts** - Server-side data fetching
- **action.ts** - Form submission handler
- **layout.tsx** - Shared layout wrapper
- **error.tsx** - Error boundary component

### Example structure

```text
src/app/
  page.tsx              # /
  loader.ts             # Data for /
  layout.tsx            # Root layout
  about/
    page.tsx            # /about
  users/
    page.tsx            # /users
    loader.ts           # Data for /users
    [id]/
      page.tsx          # /users/:id
      loader.ts         # Data for /users/:id
  blog/
    page.tsx            # /blog
    [...slug]/
      page.tsx          # /blog/* (catch-all)
```

### Dynamic routes

Use brackets for dynamic segments:

- `[id]/` - Single dynamic segment (`/users/123`)
- `[...slug]/` - Catch-all segment (`/blog/2024/my-post`)

## Pages and loaders

### Basic page

```tsx
// src/app/page.tsx
export default function HomePage() {
  return (
    <div>
      <h1>Welcome to my app</h1>
    </div>
  );
}
```

### Page with loader

```ts
// src/app/users/loader.ts
import { loader } from '@putnami/web';

export default loader(async () => {
  const users = await fetchUsers();
  return { users };
});
```

```tsx
// src/app/users/page.tsx
import { useLoaderData } from '@putnami/web';

interface LoaderData {
  users: { id: string; name: string }[];
}

export default function UsersPage() {
  const { users } = useLoaderData<LoaderData>();

  return (
    <div>
      <h1>Users</h1>
      <ul>
        {users.map(user => (
          <li key={user.id}>{user.name}</li>
        ))}
      </ul>
    </div>
  );
}
```

### Dynamic route loader

```ts
// src/app/users/[id]/loader.ts
import { loader } from '@putnami/web';
import { NotFoundException } from '@putnami/runtime';

export default loader(async (ctx) => {
  const { id } = ctx.params;
  const user = await findUser(id);

  if (!user) {
    throw new NotFoundException('User not found');
  }

  return { user };
});
```

### Loader with query parameters

```ts
// src/app/search/loader.ts
import { loader } from '@putnami/web';
import { Int, Optional } from '@putnami/runtime';

export default loader()
  .query({ q: Optional(String), page: Optional(Int) })
  .handle(async (ctx) => {
    const params = ctx.queryParams();
    const query = params.q ?? '';
    const page = params.page ?? 1;

    const results = await search(query, page);
    return { results, query, page };
  });
```

Loaders always execute on the server. Their return value is script-context
escaped before it enters hydration data; it must still be serializable. Declared
params/query schemas validate before the handler runs, and `.inject()` resolves
services from the active request scope.

## Layouts

Layouts wrap pages and persist across navigation. Use them for shared UI like headers, sidebars, and footers.

### Root layout

```tsx
// src/app/layout.tsx
import { Outlet } from '@putnami/web';

export default function RootLayout() {
  return (
    <html lang="en">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
      </head>
      <body>
        <header>My App</header>
        <main>
          <Outlet />
        </main>
        <footer>Copyright 2025</footer>
      </body>
    </html>
  );
}
```

Security declared by a layout propagates to descendant SSR pages, loader JSON
endpoints, action endpoints, and not-found boundaries, including lazy routes.
It does not affect sibling routes outside that layout.

## Rendering modes

Rendering mode is explicit and is never inferred from component source.

```tsx
import { island, page } from '@putnami/web';

// SSG: complete HTML, no full-page hydration bundle.
export default page().static().render(AboutPage);

// ISR: revalidate by time or cache tag.
export const News = page().static({ revalidate: 60 }).render(NewsPage);
export const Post = page()
  .static({ revalidate: { tags: ['posts'] }, paths: listPostParams })
  .render(PostPage);

// A component declared in Counter.island.tsx hydrates independently.
export const CounterIsland = island().load('visible').render(Counter);
```

A colocated `loader().static()` runs at build time and bakes its data into the
HTML. It receives a request-free context: reading headers, cookies, session, or
other request data raises `StaticRenderViolation`. Dynamic static routes require
a `paths` option that returns finite values, and generated splats cannot escape
or alias the static output root. A dynamic route without finite paths stays live.

A static loader can read the build version with `getBuildInfo()?.version`. When
a pre-rendered page shows that version, the build records it beside the static
HTML, and the server replaces it with its own version when it serves the page at
its route. A page restored from the build cache, or served from an image
deployed under another release id, shows the running version. A bare release
tag such as `1.0.0` is not replaced, because page content can contain the same
text. Other build fields, such as `sha` or `buildTime`, keep their build-time
values.

Island props are escaped for their script embedding. Load strategies are
`load`, `idle`, `visible`, and `media`; if the requested observer is unavailable,
hydration falls back to immediate rather than leaving a dead control.

Every build emits `.gen/putnami-web-manifest.json`, sorted deterministically by
route. It records render mode, hydration, static DI proof, and per-route client
JavaScript bytes. `putnami-web-manifest-diff` makes changes reviewable and
`putnami-web-manifest-budget` enforces the committed byte budget.

### Nested layouts

```tsx
// src/app/dashboard/layout.tsx
import { Outlet } from '@putnami/web';

export default function DashboardLayout() {
  return (
    <div className="dashboard">
      <aside>
        <nav>Dashboard navigation</nav>
      </aside>
      <div className="content">
        <Outlet />
      </div>
    </div>
  );
}
```

### Layout with loader

```ts
// src/app/dashboard/layout.loader.ts
import { useUser, HttpResponse } from '@putnami/application';
import { loader } from '@putnami/web';

export default loader(async () => {
  const user = await useUser();
  if (!user) {
    return HttpResponse.redirect('/login');
  }
  return { user };
});
```

### Sharing data with outlet context

```tsx
// src/app/dashboard/layout.tsx
import { Outlet, useLoaderData } from '@putnami/web';

export default function DashboardLayout() {
  const { user } = useLoaderData<{ user: User }>();

  return (
    <div>
      <header>Welcome, {user.name}</header>
      <Outlet context={{ user }} />
    </div>
  );
}
```

```tsx
// src/app/dashboard/settings/page.tsx
import { useOutletContext } from '@putnami/web';

export default function SettingsPage() {
  const { user } = useOutletContext<{ user: User }>();
  return <div>Settings for {user.email}</div>;
}
```

## Document and SEO

Control the document head with built-in components.

### Setting page title and meta

```tsx
import { Title, Meta, Lang, Favicon } from '@putnami/web';

export default function AboutPage() {
  return (
    <>
      <Title>About Us - My App</Title>
      <Meta name="description" content="Learn about our company" />
      <Meta property="og:title" content="About Us" />
      <Meta property="og:description" content="Learn about our company" />
      <Lang value="en" />
      <Favicon href="/favicon.ico" />

      <h1>About Us</h1>
      <p>Welcome to our company...</p>
    </>
  );
}
```

### Dynamic titles from loader data

```tsx
import { Title, useLoaderData } from '@putnami/web';

export default function UserPage() {
  const { user } = useLoaderData<{ user: { name: string } }>();

  return (
    <>
      <Title>{user.name} - My App</Title>
      <h1>{user.name}</h1>
    </>
  );
}
```

### Adding scripts and styles

```tsx
import { Script, Style } from '@putnami/web';

export default function Page() {
  return (
    <>
      <Script src="/analytics.js" async />
      <Style>{`
        .custom-class {
          color: blue;
        }
      `}</Style>

      <div className="custom-class">Styled content</div>
    </>
  );
}
```

## Client hooks

### Navigation hooks

```tsx
import {
  useNavigate,
  useLocation,
  useParams,
  useSearchParams,
} from '@putnami/web';

function NavigationExample() {
  const navigate = useNavigate();
  const location = useLocation();
  const params = useParams();
  const [searchParams, setSearchParams] = useSearchParams();

  // Programmatic navigation
  const goToUser = (id: string) => {
    navigate(`/users/${id}`);
  };

  // Navigate with state
  const goWithState = () => {
    navigate('/dashboard', { state: { from: 'home' } });
  };

  // Navigate back
  const goBack = () => {
    navigate(-1);
  };

  // Update search params
  const updateFilter = (value: string) => {
    setSearchParams({ filter: value });
  };

  return (
    <div>
      <p>Current path: {location.pathname}</p>
      <p>User ID: {params.id}</p>
      <p>Filter: {searchParams.get('filter')}</p>
    </div>
  );
}
```

### Data hooks

```tsx
import {
  useLoaderData,
  useActionData,
  useNavigation,
  useFetcher,
} from '@putnami/web';

function DataExample() {
  // Get loader data
  const data = useLoaderData<{ items: Item[] }>();

  // Get action result after form submission
  const actionData = useActionData<{ success: boolean; error?: string }>();

  // Check navigation state
  const navigation = useNavigation();
  const isSubmitting = navigation.state === 'submitting';
  const isLoading = navigation.state === 'loading';

  // Fetch data without navigation
  const fetcher = useFetcher<{ count: number }>();

  return (
    <div>
      {isLoading && <p>Loading...</p>}

      {actionData?.error && <p className="error">{actionData.error}</p>}

      <button onClick={() => fetcher.load('/api/count')}>
        Refresh count: {fetcher.data?.count}
      </button>
    </div>
  );
}
```

### Link component

```tsx
import { Link } from '@putnami/web';

function Navigation() {
  return (
    <nav>
      <Link to="/">Home</Link>
      <Link to="/about">About</Link>
      <Link to="/users" prefetch="intent">Users</Link>
      <Link to="/contact" replace>Contact</Link>
    </nav>
  );
}
```

### Blocking navigation

```tsx
import { useBlocker } from '@putnami/web';

function FormWithUnsavedChanges() {
  const [isDirty, setIsDirty] = useState(false);

  const blocker = useBlocker(
    ({ currentLocation, nextLocation }) =>
      isDirty && currentLocation.pathname !== nextLocation.pathname
  );

  return (
    <div>
      <input onChange={() => setIsDirty(true)} />

      {blocker.state === 'blocked' && (
        <div>
          <p>You have unsaved changes. Leave anyway?</p>
          <button onClick={() => blocker.proceed()}>Leave</button>
          <button onClick={() => blocker.reset()}>Stay</button>
        </div>
      )}
    </div>
  );
}
```

## Error boundaries

Handle errors gracefully at the route level.

### Route error boundary

```tsx
// src/app/users/error.tsx
import { useRouteError, isRouteErrorResponse } from '@putnami/web';

export default function UsersError() {
  const error = useRouteError();

  if (isRouteErrorResponse(error)) {
    return (
      <div>
        <h1>{error.status} {error.statusText}</h1>
        <p>{error.data}</p>
      </div>
    );
  }

  return (
    <div>
      <h1>Something went wrong</h1>
      <p>An unexpected error occurred.</p>
    </div>
  );
}
```

### Global error boundary

```tsx
// src/app/error.tsx
export default function GlobalError() {
  return (
    <html>
      <body>
        <h1>Application Error</h1>
        <p>Something went wrong. Please try again later.</p>
        <a href="/">Go home</a>
      </body>
    </html>
  );
}
```

## Streaming and Suspense

React 19 streaming SSR is supported out of the box.

### Using Suspense

```tsx
import { Suspense } from '@putnami/web';

export default function Page() {
  return (
    <div>
      <h1>Dashboard</h1>

      <Suspense fallback={<p>Loading stats...</p>}>
        <AsyncStats />
      </Suspense>

      <Suspense fallback={<p>Loading chart...</p>}>
        <AsyncChart />
      </Suspense>
    </div>
  );
}
```

### Deferred data

Load non-critical data after initial render:

```ts
// loader.ts
import { loader } from '@putnami/web';

export default loader(async () => {
  const criticalData = await fetchCriticalData();

  return {
    critical: criticalData,
    deferred: fetchDeferredData(), // Don't await
  };
});
```

```tsx
// page.tsx
import { Suspense } from '@putnami/web';
import { Await, useLoaderData } from '@putnami/web';

export default function Page() {
  const { critical, deferred } = useLoaderData();

  return (
    <div>
      <h1>{critical.title}</h1>

      <Suspense fallback={<p>Loading more...</p>}>
        <Await resolve={deferred}>
          {(data) => <DeferredContent data={data} />}
        </Await>
      </Suspense>
    </div>
  );
}
```

## Caching

Putnami supports server-side caching and HTTP cache headers for loaders and pages.

### Server-side cache

```ts
// Cache loader results server-side for 1 hour
import { loader } from '@putnami/web';

export default loader()
  .cache({ ttl: 60 * 60 * 1000 })
  .handle(async (ctx) => {
    return { posts: await fetchPosts() };
  });
```

### HTTP cache headers

```ts
// Browser cache for 5 minutes with ETag
export default loader()
  .cache({ maxAge: 300, etag: true })
  .handle(async (ctx) => {
    return { posts: await fetchPosts() };
  });
```

### Combined caching

```ts
// Server cache + HTTP cache for maximum performance
export default loader()
  .cache({
    ttl: 60 * 60 * 1000, // Server: 1 hour
    maxAge: 300,         // Browser: 5 minutes
    etag: true           // Conditional requests
  })
  .handle(async (ctx) => {
    return { data: await fetchData() };
  });
```

### Cache eviction in actions

```ts
import { action } from '@putnami/web';

export default action()
  .evict('loader:/posts/*') // Evict after mutation
  .handle(async (ctx) => {
    await createPost(ctx);
    return { ok: true };
  });
```

See [Caching](/docs/frameworks/typescript/caching) for full documentation.

Default page-cache keys include a stable authenticated-user fingerprint and the
query string; default loader keys include query parameters. Redirect responses
are not cached. A custom key is an escape hatch and must preserve every input
that changes returned data or HTML.

## Security defaults

- Each SSR request receives a CSP nonce. The same nonce is stamped on every
  framework-emitted hydration or module script and added to the default policy.
- React responses also apply `X-Content-Type-Options: nosniff` and a
  `Referrer-Policy` by default without replacing the renderer's content type.
- Action POSTs require the `_csrf` cookie token to be echoed in
  `X-CSRF-Token`. No-JavaScript forms use `<CsrfInput />` to submit the same
  value as a hidden field.
- Hydration receives only `authenticated`, roles, and scopes for UI gating — no
  access token or unrestricted claims object.
- Loader/action/error data is escaped for an inline script context. A client
  chunk or hydration failure keeps or restores server HTML instead of blanking
  the page.

These defaults protect requests even when JavaScript is absent. OAuth identity
establishment and session storage are owned by `@putnami/application`; this
package propagates their server security decisions through the route graph.

## Support and compatibility

`@putnami/web` is a public, documented, maintained package classified `stable`.
Its web-application-delivery specification and accepted ADRs for separate
render graphs and server-authoritative data/security live next to the package
source. That contract covers explicit rendering modes, route generation,
request-isolated document state, hydration containment, server loaders/actions,
security propagation, safe default request headers/CSRF, cache isolation, and
the deterministic render manifest.

OAuth identity, WebSocket/SSE transport, UI styling, and persistence remain
owned by their respective packages. No default-framework or cross-language
parity promise is made. Before v1.0.0, minor `0.x` releases may contain
documented breaking changes.

## Related guides

- [UI system](/docs/frameworks/typescript/ui)
- [Build a web app](/docs/how-to/build-a-web-app)
- [React routing](/docs/frameworks/typescript/react-routing)
- [Forms and actions](/docs/frameworks/typescript/forms-and-actions)
- [Caching](/docs/frameworks/typescript/caching)
