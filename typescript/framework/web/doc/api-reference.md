# API Reference

Complete API documentation for `@putnami/web`.

## ReactApplication

Main class for configuring React SSR routes manually.

### Constructor

```typescript
new ReactApplication(
  config?: PutnamiReactConfig,
  httpPlugin?: HttpPlugin,
  routeHelper?: RouteTreeHelper<RouteObject>
)
```

**Parameters:**
- `config` - Optional configuration object
- `httpPlugin` - Optional HttpPlugin instance
- `routeHelper` - Optional RouteTreeHelper instance

**Example:**
```typescript
const reactApp = new ReactApplication();
```

### Methods

#### `reactPage(route, options)`

Adds a page component to the specified route.

```typescript
reactPage(
  route: string,
  options: {
    page: ReactNode | PageModule | LazyPageModule;
    loader?: LoaderModule | LazyLoaderModule;
    action?: ActionModule | LazyActionModule;
    statusCode?: number;
    notFoundMiddleware?: HttpMiddleware[];
  }
): ReactApplication
```

**Parameters:**
- `route` - The route path (e.g., '/users/[id]')
- `options.page` - The page component, page module, or lazy page module (a `page.tsx` default export carries its own config via `.render(...)`)
- `options.loader` - Optional loader module (eager or lazy)
- `options.action` - Optional action module (eager or lazy)
- `options.statusCode` - Optional HTTP status code (default: 200)
- `options.notFoundMiddleware` - Optional middleware applied when the route falls through to not-found handling

**Returns:** `ReactApplication` instance for chaining

**Example:**
```typescript
reactApp.reactPage('/users', {
  page: <UsersPage />,
  loader: usersLoader,
  action: usersAction,
  statusCode: 200,
});
```

#### `reactLayout(route, options)`

Adds a layout component to the specified route.

```typescript
reactLayout(
  route: string,
  options: {
    layout: LayoutModule;
    loader?: LoaderModule;
  }
): ReactApplication
```

**Parameters:**
- `route` - The route path (e.g., '/dashboard')
- `options.layout` - The layout module (`{ default: React.ComponentType | LayoutDefinition }`)
- `options.loader` - Optional loader module for the layout

**Returns:** `ReactApplication` instance for chaining

**Example:**
```typescript
reactApp.reactLayout('/dashboard', {
  layout: dashboardLayoutModule,
  loader: dashboardLoader,
});
```

#### `reactError(route, options)`

Adds an error boundary component to the specified route.

```typescript
reactError(
  route: string,
  options: {
    error: ErrorModule;
  }
): void
```

**Parameters:**
- `route` - The route path (e.g., '/')
- `options.error` - The error module (`{ default: React.ComponentType | ErrorDefinition }`)

**Example:**
```typescript
reactApp.reactError('/', { error: errorModule });
```

#### `reactScript(route, path, isEntry?)`

Registers a static script file to be served.

```typescript
reactScript(
  route: string,
  path: string,
  isEntry?: boolean
): ReactApplication
```

**Parameters:**
- `route` - The URL route for the script (e.g., '/hydrate.main.js')
- `path` - The file path relative to the public folder
- `isEntry` - Whether this is the entry script (default: true)

**Returns:** `ReactApplication` instance for chaining

**Example:**
```typescript
reactApp.reactScript('/hydrate.main.js', 'hydrate.main.js');
```

#### `setBasename(basename: string): void`

Set the basename (URL prefix) for this ReactApplication. Used by `ReactPlugin` to apply module-level path at warmup time. Updates the React Router root path for correct SSR URL matching and injects `window.__basename` for client-side hydration.

**Parameters:**
- `basename` - The URL prefix (e.g., `/tasks`)

**Example:**
```typescript
reactApp.setBasename('/tasks');
// React Router will match routes under /tasks/*
```

#### `getBasename(): string | undefined`

Get the basename, if set.

**Returns:** The basename string, or `undefined` if not set

#### `getHttpPlugin()`

Gets the internal HttpPlugin for merging routes.

```typescript
getHttpPlugin(): HttpPlugin
```

**Returns:** `HttpPlugin` instance

**Example:**
```typescript
const httpPlugin = reactApp.getHttpPlugin();
app.use(httpPlugin);
```

## React Plugin

### `react(config?)`

Creates a React plugin instance for file-based routing.

```typescript
react(config?: Partial<PutnamiReactConfig>): ReactPlugin
```

**Parameters:**
- `config` - Optional partial configuration object

**Returns:** `ReactPlugin` instance

**Example:**
```typescript
import { react } from '@putnami/web';

export const app = () => application().use(
  react({
    scanRoots: [
      { path: 'src/app' },
      { path: 'src/projects/web', routePrefix: '/projects' },
      { path: 'src/tasks/web', routePrefix: '/tasks' },
    ],
  })
);
```

When used inside a module with `.path()`, the React plugin automatically inherits the module path as its basename. All React routes and SSR rendering will be prefixed accordingly:

```typescript
import { module } from '@putnami/application';
import { react } from '@putnami/web';

const dashboard = module('dashboard')
  .path('/dashboard')
  .use(react());  // Pages served under /dashboard/*
```

## Hooks

### Data Hooks

#### `useLoaderData<T>()`

Returns typed loader data for the current route.

```typescript
useLoaderData<T = unknown>(): T
```

**Type Parameters:**
- `T` - The expected return type of the loader data

**Returns:** The loader data cast to type T

**Example:**
```typescript
interface UserData {
  name: string;
  email: string;
}

function Profile() {
  const data = useLoaderData<UserData>();
  return <div>{data.name}</div>;
}
```

#### `useActionData<T>()`

Returns action data with `ok` and `status` fields, or `undefined` if no action has been performed.

```typescript
useActionData<T = unknown>(): (T & { ok: boolean; status: number }) | undefined
```

**Type Parameters:**
- `T` - The expected return type of the action data

**Returns:** The action data with `ok` and `status` fields, or `undefined`

**Example:**
```typescript
interface FormResult {
  message: string;
}

function Form() {
  const result = useActionData<FormResult>();
  if (result?.ok) {
    return <div>{result.message}</div>;
  }
  return <form>...</form>;
}
```

#### `useRouteLoaderData<T>(routeId)`

Returns loader data for a specific route by ID.

```typescript
useRouteLoaderData<T = unknown>(routeId: string): T
```

**Type Parameters:**
- `T` - The expected return type of the loader data

**Parameters:**
- `routeId` - The route ID (e.g., 'root', 'dashboard-layout')

**Returns:** The loader data for the specified route

**Example:**
```typescript
function ChildComponent() {
  const rootData = useRouteLoaderData<{ user: User }>('root');
  return <div>{rootData.user.name}</div>;
}
```

#### `useRouteError<E>()`

Returns the error caught by the nearest error boundary. A route can throw any
value, so the default return type is `unknown`.

```typescript
useRouteError<E = unknown>(): E
```

**Type Parameters:**
- `E` - The expected type of the thrown value. This is an unchecked assertion,
  not a check: a route that throws a 4xx/5xx `Response` reaches the boundary as
  an `ErrorResponse`, so `useRouteError<Error>()` would read `undefined` from
  `.message`. Name a type only when the boundary cannot receive anything else;
  otherwise narrow the default `unknown`.

**Returns:** The thrown value cast to type E

**Example:**
```typescript
// src/app/posts/error.tsx
import { error, isRouteErrorResponse, useRouteError } from '@putnami/web';

export default error().render(function PostsError() {
  const err = useRouteError();
  if (isRouteErrorResponse(err)) {
    return <div>{err.status} — {err.statusText}</div>;
  }
  return <div>Error: {err instanceof Error ? err.message : 'Unknown error'}</div>;
});
```

#### `isRouteErrorResponse(value)`

Narrows an error-boundary value to a thrown `Response` — a 4xx/5xx raised by a
loader, an action, or the router itself — exposing `status`, `statusText`, and
`data`. Re-exported so a boundary never imports `react-router` directly.

```typescript
isRouteErrorResponse(value: unknown): value is ErrorResponse
```

**Returns:** `true` when the value is a thrown route response

### Navigation Hooks

#### `useNavigate()`

Returns a function to programmatically navigate.

```typescript
useNavigate(): NavigateFunction
```

**Returns:** Navigate function

**Example:**
```typescript
function Page() {
  const navigate = useNavigate();
  return <button onClick={() => navigate('/dashboard')}>Go</button>;
}
```

#### `useNavigation()`

Returns the current navigation state.

```typescript
useNavigation(): Navigation
```

**Returns:** Navigation object with `state` property ('idle' | 'loading' | 'submitting')

**Example:**
```typescript
function Page() {
  const navigation = useNavigation();
  return navigation.state === 'loading' ? <Spinner /> : <Content />;
}
```

#### `useLocation()`

Returns the current location object.

```typescript
useLocation(): Location
```

**Returns:** Location object with `pathname`, `search`, `hash`, and `state`

**Example:**
```typescript
function Page() {
  const location = useLocation();
  return <div>Current path: {location.pathname}</div>;
}
```

#### `useParams()`

Returns an object of key/value pairs of URL parameters.

```typescript
useParams<Params extends Record<string, string> = Record<string, string>>(): Params
```

**Type Parameters:**
- `Params` - The expected params type

**Returns:** Object of route parameters

**Example:**
```typescript
// Route: /posts/[id]
function PostPage() {
  const { id } = useParams<{ id: string }>();
  return <div>Post ID: {id}</div>;
}
```

#### `useSearchParams()`

Returns a tuple of the current search params and a function to update them.

```typescript
useSearchParams(): [URLSearchParams, SetURLSearchParams]
```

**Returns:** Tuple of search params and setter function

**Example:**
```typescript
function SearchPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const query = searchParams.get('q') || '';

  return (
    <input
      value={query}
      onChange={(e) => setSearchParams({ q: e.target.value })}
    />
  );
}
```

### Form Hooks

#### `useSubmit()`

Returns a function to submit forms programmatically.

```typescript
useSubmit(): SubmitFunction
```

**Returns:** Submit function

**Example:**
```typescript
function Page() {
  const submit = useSubmit();
  const handleSubmit = () => {
    const formData = new FormData();
    formData.set('name', 'John');
    submit(formData, { method: 'post' });
  };
  return <button onClick={handleSubmit}>Submit</button>;
}
```

#### `useFetcher()`

Returns a fetcher object for data fetching without navigation.

```typescript
useFetcher<TData = unknown>(): FetcherWithComponents<TData>
```

**Type Parameters:**
- `TData` - The expected data type

**Returns:** Fetcher object with `Form`, `load`, `submit`, `data`, and `state`

**Example:**
```typescript
function LikeButton({ postId }: { postId: string }) {
  const fetcher = useFetcher();
  return (
    <fetcher.Form method="post" action={`/posts/${postId}/like`}>
      <button type="submit">Like</button>
    </fetcher.Form>
  );
}
```

#### `useRevalidator()`

Returns a revalidator object to manually revalidate data.

```typescript
useRevalidator(): Revalidator
```

**Returns:** Revalidator object with `revalidate` method

**Example:**
```typescript
function Page() {
  const revalidator = useRevalidator();
  return <button onClick={() => revalidator.revalidate()}>Refresh</button>;
}
```

### Other Hooks

All standard React Router hooks are re-exported:
- `useOutlet()` - Render child route content
- `useOutletContext<T>()` - Access context from parent layout
- `useMatch(pattern)` - Check if route matches pattern
- `useResolvedPath(to)` - Resolve a path relative to current location
- `useNavigationType()` - Get navigation type (POP, PUSH, REPLACE)
- `useBlocker(blocker)` - Block navigation
- `useBeforeUnload(handler)` - Handle before unload
- `useLinkClickHandler(to, options?)` - Handle link clicks
- `useFormAction(action?, method?)` - Get form action URL

## Components

### `Link`

Client-side navigation link with prefetching support.

```typescript
interface LinkProps extends RouterLinkProps {
  prefetch?: 'intent' | 'render' | 'none';
}
```

**Props:**
- All standard anchor/React Router Link props
- `prefetch` - Prefetch behavior ('intent' | 'render' | 'none', default: 'none')

**Example:**
```typescript
<Link to="/dashboard" prefetch="intent">Dashboard</Link>
```

### `Form`

Form component that submits to route actions.

```typescript
interface FormProps extends React.FormHTMLAttributes<HTMLFormElement> {
  method?: 'get' | 'post' | 'put' | 'patch' | 'delete';
  action?: string;
  encType?: string;
}
```

**Example:**
```typescript
<Form method="post">
  <input name="email" />
  <button type="submit">Submit</button>
</Form>
```

### `Outlet`

Renders child route content in layouts.

```typescript
interface OutletProps {
  context?: unknown;
}
```

**Example:**
```typescript
export default function Layout() {
  return (
    <div>
      <nav>Navigation</nav>
      <main>
        <Outlet />
      </main>
    </div>
  );
}
```

## Document Helpers

### `documentHelper()`

Returns a document helper instance for managing HTML document metadata.

```typescript
documentHelper(): DocumentHelper
```

**Returns:** DocumentHelper instance

**Example:**
```typescript
const doc = documentHelper();
doc.title = 'My Page';
doc.addMeta({ name: 'description', content: 'Page description' });
```

### Document Helper Interface

```typescript
interface DocumentHelper {
  title: string;
  lang: string;
  addScript(scriptAttributes: ScriptHTMLAttributes<HTMLScriptElement>): void;
  addLink(linkAttributes: LinkHTMLAttributes<HTMLLinkElement>): void;
  addMeta(metaAttributes: MetaHTMLAttributes<HTMLMetaElement>): void;
  addStyle(styleAttributes: StyleHTMLAttributes<HTMLStyleElement>): void;
  readonly headHtml?: string;
}
```

### Document Components

#### `Title`

Sets the page title.

```typescript
<Title>Page Title</Title>
```

#### `Meta`

Adds a meta tag.

```typescript
<Meta name="description" content="Page description" />
<Meta property="og:title" content="Page Title" />
```

#### `Script`

Adds a script tag.

```typescript
<Script src="/script.js" />
<Script>{`console.log('inline');`}</Script>
```

#### `Style`

Adds a style tag or stylesheet link.

```typescript
<Style href="/styles.css" />
<Style>{`body { margin: 0; }`}</Style>
```

#### `Lang`

Sets the HTML lang attribute.

```typescript
<Lang>en</Lang>
```

#### `Favicon`

Sets the favicon.

```typescript
<Favicon href="/favicon.ico" />
```

## Type Definitions

### `HttpRequestContext`

Context object passed to loaders and actions.

```typescript
interface HttpRequestContext {
  req: Request;
  params: Record<string, string>;
  // ... other context properties
}
```

### `LoaderModule`

Module containing a loader function or definition.

```typescript
interface LoaderModule {
  loader?: ((ctx: HttpRequestContext) => unknown) | LoaderDefinition;
  default?: ((ctx: HttpRequestContext) => unknown) | LoaderDefinition;
}
```

### `ActionModule`

Module containing an action function or definition.

```typescript
interface ActionModule {
  action?: RouteHandler | ActionDefinition;
  default?: RouteHandler | ActionDefinition;
}
```

### `PageModule`

Module containing a page component.

```typescript
interface PageModule {
  default: React.ComponentType;
}
```

## Page & Layout Declarations

### `page()`

Creates a page configuration builder for declaring middleware, directives, and the page component. The `page()` builder is used directly in `page.tsx` files, combining configuration and component rendering in a single export.

```typescript
page(): PageBuilder
```

**Returns:** `PageBuilder` instance

**Example:**
```typescript
// src/app/admin/page.tsx
import { page } from '@putnami/web';

export default page()
  .secure({ roles: ['admin'] })
  .render(() => <AdminDashboard />);
```

### `PageBuilder`

Fluent builder returned by `page()`.

| Method | Description |
|--------|-------------|
| `.secure(options?)` | Require authentication (scopes, roles) |
| `.rateLimit(options?)` | Apply rate limiting |
| `.status(code)` | Set HTTP status code |
| `.use(middleware)` | Add custom middleware |
| `.render(Component)` | Set the page component and finalize the `PageDefinition` |

### `layout()`

Creates a layout configuration builder for declaring middleware that propagates to all child pages, and rendering the layout component.

```typescript
layout(): LayoutBuilder
```

**Returns:** `LayoutBuilder` instance

**Example:**
```typescript
// src/app/dashboard/layout.tsx
import { layout, Outlet } from '@putnami/web';

export default layout()
  .secure({ roles: ['user'] })
  .render(function DashboardLayout() {
    return <div><Outlet /></div>;
  });
```

### `LayoutBuilder`

Fluent builder returned by `layout()`.

| Method | Description |
|--------|-------------|
| `.secure(options?)` | Require authentication for all child pages |
| `.rateLimit(options?)` | Apply rate limiting to all child pages |
| `.use(middleware)` | Add custom middleware to all child pages |
| `.render(Component)` | Set the layout component and finalize the `LayoutDefinition` |

## Loader & Action Declarations

### `loader()`

Creates a loader definition with optional schema validation.

**Simple mode** — pass a handler directly:

```typescript
loader(handler: (ctx: HttpRequestContext) => unknown): LoaderDefinition
```

**Builder mode** — chain validation schemas:

```typescript
loader(): LoaderBuilder
```

**Examples:**

```typescript
// Simple mode
import { loader } from '@putnami/web';

export default loader((ctx) => {
  return fetchUsers();
});

// Builder mode
import { loader } from '@putnami/web';
import { Uuid, Optional, String } from '@putnami/application';

export default loader()
  .params({ id: Uuid })
  .query({ include: Optional(String) })
  .handle(async (ctx) => {
    return getUser(ctx.params.id);
  });
```

### `LoaderBuilder`

Fluent builder returned by `loader()`.

| Method | Description |
|--------|-------------|
| `.params(schema)` | Declare and validate path parameters |
| `.query(schema)` | Declare and validate query string parameters |
| `.inject(tokens)` | Declare DI dependencies to resolve from the current scope |
| `.cache(options)` | Set server-side and/or HTTP caching options |
| `.secure(options?)` | Require authentication and enforce access rules |
| `.handle(handler)` | Provide the handler function and finalize the definition |

### `action()`

Creates an action definition with optional schema validation.

**Simple mode** — pass a handler directly:

```typescript
action(handler: (ctx: HttpRequestContext) => RouteHandlerResult): ActionDefinition
```

**Builder mode** — chain validation schemas:

```typescript
action(): ActionBuilder
```

**Examples:**

```typescript
// Simple mode
import { action } from '@putnami/web';

export default action(async (ctx) => {
  const data = await ctx.body();
  return { success: true };
});

// Builder mode
import { action } from '@putnami/web';
import { Uuid, String, Email } from '@putnami/application';

export default action()
  .params({ id: Uuid })
  .body({ name: String, email: Email })
  .handle(async (ctx) => {
    const body = await ctx.body();
    return updateUser(ctx.params.id, body);
  });
```

### `ActionBuilder`

Fluent builder returned by `action()`.

| Method | Description |
|--------|-------------|
| `.params(schema)` | Declare and validate path parameters |
| `.query(schema)` | Declare and validate query string parameters |
| `.body(schema)` | Declare and validate request body |
| `.evict(...patterns)` | Evict cache patterns after successful action execution |
| `.inject(tokens)` | Declare DI dependencies to resolve from the current scope |
| `.secure(options?)` | Require authentication and enforce access rules |
| `.handle(handler)` | Provide the handler function and finalize the definition |

### `isLoaderDefinition(value)`

Type guard for `LoaderDefinition` objects.

```typescript
isLoaderDefinition(value: unknown): value is LoaderDefinition
```

### `isActionDefinition(value)`

Type guard for `ActionDefinition` objects.

```typescript
isActionDefinition(value: unknown): value is ActionDefinition
```

## Error & Not-Found Declarations

### `error()`

Creates an error boundary definition with middleware support and renders the error component.

```typescript
error(): ErrorBuilder
```

**Example:**

```typescript
// src/app/dashboard/error.tsx
import { error, isRouteErrorResponse, useRouteError } from '@putnami/web';

export default error()
  .secure({ roles: ['user'] })
  .status(500)
  .render(function ErrorPage() {
    const err = useRouteError();
    if (isRouteErrorResponse(err)) {
      return <div>{err.status} {err.statusText}</div>;
    }
    return <div>Error: {err instanceof Error ? err.message : 'Unknown error'}</div>;
  });
```

### `ErrorBuilder`

Fluent builder returned by `error()`.

| Method | Description |
|--------|-------------|
| `.secure(options?)` | Require authentication |
| `.rateLimit(options?)` | Apply rate limiting |
| `.status(code)` | Set HTTP status code |
| `.use(middleware)` | Add custom middleware |
| `.render(Component)` | Set the error component and finalize the `ErrorDefinition` |

### `notFound()`

Creates a not-found page definition with middleware support and renders the not-found component.

```typescript
notFound(): NotFoundBuilder
```

**Example:**

```typescript
// src/app/not-found.tsx
import { notFound } from '@putnami/web';

export default notFound()
  .rateLimit({ max: 50 })
  .render(function NotFoundPage() {
    return <div>404 - Not Found</div>;
  });
```

### `NotFoundBuilder`

Fluent builder returned by `notFound()`.

| Method | Description |
|--------|-------------|
| `.secure(options?)` | Require authentication |
| `.rateLimit(options?)` | Apply rate limiting |
| `.use(middleware)` | Add custom middleware |
| `.render(Component)` | Set the not-found component and finalize the `NotFoundDefinition` |

## Middleware Composition

### `middleware()`

Composes multiple middleware into a single reusable middleware definition.

```typescript
middleware(): MiddlewareBuilder
```

**Example:**

```typescript
import { middleware } from '@putnami/web';

const apiGuard = middleware()
  .secure({ roles: ['user'] })
  .rateLimit({ max: 100 })
  .build();

// Use in page configs:
export default page()
  .use(apiGuard.handler)
  .render(() => <ProtectedPage />);
```

### `MiddlewareBuilder`

Fluent builder returned by `middleware()`.

| Method | Description |
|--------|-------------|
| `.secure(options?)` | Require authentication |
| `.rateLimit(options?)` | Apply rate limiting |
| `.use(middleware)` | Add custom middleware |
| `.build()` | Finalize and return `MiddlewareDefinition` |

### `MiddlewareDefinition`

The composed middleware object. Use `.handler` to get the single `HttpMiddleware` function, or `.stack` to access the individual middleware array.

### Composition Order

Layout middleware composes with page middleware and nested layouts:

```
Root layout middleware → Nested layout middleware → Page middleware → Handler
```

Layout middleware applies to the layout's own loader, plus all descendant page renderers, loaders, and actions.

## Type Guards

### `isPageDefinition(value)`

Type guard for `PageDefinition` objects.

```typescript
isPageDefinition(value: unknown): value is PageDefinition
```

### `isLayoutDefinition(value)`

Type guard for `LayoutDefinition` objects.

```typescript
isLayoutDefinition(value: unknown): value is LayoutDefinition
```

### `isErrorDefinition(value)`

Type guard for `ErrorDefinition` objects.

```typescript
isErrorDefinition(value: unknown): value is ErrorDefinition
```

### `isNotFoundDefinition(value)`

Type guard for `NotFoundDefinition` objects.

```typescript
isNotFoundDefinition(value: unknown): value is NotFoundDefinition
```

### `isMiddlewareDefinition(value)`

Type guard for `MiddlewareDefinition` objects.

```typescript
isMiddlewareDefinition(value: unknown): value is MiddlewareDefinition
```

## Migration from React Router

If you're migrating from React Router, most APIs are compatible:

1. **Hooks** - All React Router hooks are re-exported with improved types
2. **Components** - `Link`, `Form`, `Outlet` work the same way
3. **Loaders/Actions** - Use `HttpRequestContext` instead of React Router's context
4. **Error Boundaries** - Use `useRouteError()` hook (same as React Router)

## Next Steps

- See [Getting Started](getting-started.md) for setup instructions
- Check [Troubleshooting](troubleshooting.md) for common issues
- Explore [Advanced Patterns](advanced-patterns.md) for complex scenarios
