# React Routing

`@putnami/web` uses file-based routing built on React Router. Routes are automatically discovered from your file structure, with support for dynamic segments, nested layouts, and error boundaries.

## File conventions

### Route files

- **page.tsx** - React component for the route, optionally with middleware via `page()` builder
- **loader.ts** - Server-side data fetching
- **action.ts** - Form submission handler
- **layout.tsx** - Shared layout wrapper, optionally with middleware via `layout()` builder
- **layout.loader.ts** - Layout data fetching
- **error.tsx** - Error boundary component, optionally with middleware via `error()` builder
- **not-found.tsx** - Not-found (404) page component, optionally with middleware via `notFound()` builder

### Folder structure

```text
src/app/
  page.tsx              # /
  loader.ts             # Data for /
  layout.tsx            # Root layout
  error.tsx             # Global error boundary
  about/
    page.tsx            # /about
  users/
    page.tsx            # /users
    loader.ts           # Data for /users
    layout.tsx          # Layout for /users/*
    [id]/
      page.tsx          # /users/:id
      loader.ts         # Data for /users/:id
  blog/
    page.tsx            # /blog
    [...slug]/
      page.tsx          # /blog/* (catch-all)
      loader.ts         # Data for catch-all
```

## Dynamic routes

### Single parameter

Use brackets for dynamic segments:

```text
src/app/users/[id]/page.tsx → /users/:id
```

```tsx
// src/app/users/[id]/page.tsx
import { useParams } from '@putnami/web';

export default function UserPage() {
  const { id } = useParams();
  return <h1>User {id}</h1>;
}
```

```ts
// src/app/users/[id]/loader.ts
import { loader } from '@putnami/web';
import { Uuid } from '@putnami/application';

export default loader()
  .params({ id: Uuid })
  .handle(async (ctx) => {
    // ctx.params.id is validated as a UUID
    const user = await fetchUser(ctx.params.id);
    return { user };
  });
```

### Multiple parameters

```text
src/app/posts/[year]/[month]/[slug]/page.tsx → /posts/:year/:month/:slug
```

```ts
// loader.ts
import { loader } from '@putnami/web';

export default loader()
  .params({ year: String, month: String, slug: String })
  .handle(async (ctx) => {
    const { year, month, slug } = ctx.params;
    const post = await fetchPost(year, month, slug);
    return { post };
  });
```

### Catch-all routes

Use `[...param]` to match any remaining path segments:

```text
src/app/docs/[...path]/page.tsx → /docs/*
```

```ts
// src/app/docs/[...path]/loader.ts
import { loader } from '@putnami/web';

export default loader(async (ctx) => {
  const { path } = ctx.params;
  // path = 'getting-started/installation' for /docs/getting-started/installation
  const doc = await fetchDoc(path);
  return { doc };
});
```

### Optional catch-all

Catch-all routes also match the parent path:

```text
/docs           → path = undefined
/docs/intro     → path = 'intro'
/docs/api/users → path = 'api/users'
```

## Layouts

Layouts wrap child routes and persist across navigation.

### Root layout

```tsx
// src/app/layout.tsx
import { layout, Outlet } from '@putnami/web';

export default layout().render(function RootLayout() {
  return (
    <html lang="en">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <title>My App</title>
      </head>
      <body>
        <Outlet />
      </body>
    </html>
  );
});
```

### Nested layouts

```tsx
// src/app/dashboard/layout.tsx
import { layout, Outlet } from '@putnami/web';

export default layout().render(function DashboardLayout() {
  return (
    <div className="dashboard">
      <nav>
        <a href="/dashboard">Overview</a>
        <a href="/dashboard/settings">Settings</a>
      </nav>
      <main>
        <Outlet />
      </main>
    </div>
  );
});
```

### Layout with data

```ts
// src/app/dashboard/layout.loader.ts
import { loader } from '@putnami/web';
import { useUser, HttpResponse } from '@putnami/application';

export default loader(async (ctx) => {
  const user = await useUser();

  if (!user) {
    return HttpResponse.redirect('/login');
  }

  return { user };
});
```

```tsx
// src/app/dashboard/layout.tsx
import { layout, Outlet, useLoaderData } from '@putnami/web';

export default layout().render(function DashboardLayout() {
  const { user } = useLoaderData<{ user: User }>();

  return (
    <div>
      <header>Welcome, {user.name}</header>
      <Outlet />
    </div>
  );
});
```

### Layout middleware

Use the `layout()` builder to declare middleware that applies to all pages under a layout:

```tsx
// src/app/admin/layout.tsx
import { layout, Outlet } from '@putnami/web';

export default layout()
  .secure({ roles: ['admin'] })
  .render(function AdminLayout() {
    return <Outlet />;
  });
```

All pages under `/admin/*` now require OAuth authentication with the `admin` role.

### Page middleware

Declare middleware for a specific page directly in `page.tsx` using the `page()` builder:

```tsx
// src/app/admin/settings/page.tsx
import { page } from '@putnami/web';

export default page()
  .secure()
  .rateLimit({ max: 50 })
  .render(function SettingsPage() {
    return <h1>Settings</h1>;
  });
```

Layout and page middleware compose: layout middleware runs first (outermost), then page middleware, then the handler. Nested layouts stack in order from root to leaf.

### Sharing context

Pass data to child routes via outlet context:

```tsx
// src/app/dashboard/layout.tsx
import { layout, Outlet, useLoaderData } from '@putnami/web';

export default layout().render(function DashboardLayout() {
  const { user, permissions } = useLoaderData();

  return (
    <Outlet context={{ user, permissions }} />
  );
});
```

```tsx
// src/app/dashboard/settings/page.tsx
import { useOutletContext } from '@putnami/web';

export default function SettingsPage() {
  const { user, permissions } = useOutletContext<DashboardContext>();

  return (
    <div>
      <h1>Settings for {user.name}</h1>
      {permissions.canEdit && <EditForm />}
    </div>
  );
}
```

## Error boundaries

### Route error boundary

```tsx
// src/app/users/error.tsx
import { error, useRouteError, isRouteErrorResponse, Link } from '@putnami/web';

export default error().render(function UsersError() {
  const err = useRouteError();

  if (isRouteErrorResponse(err)) {
    if (err.status === 404) {
      return (
        <div>
          <h1>User Not Found</h1>
          <p>The user you're looking for doesn't exist.</p>
          <Link to="/users">Back to users</Link>
        </div>
      );
    }

    return (
      <div>
        <h1>{err.status} {err.statusText}</h1>
        <p>{err.data}</p>
      </div>
    );
  }

  return (
    <div>
      <h1>Something went wrong</h1>
      <p>An unexpected error occurred.</p>
    </div>
  );
});
```

### Global error boundary

```tsx
// src/app/error.tsx
import { error } from '@putnami/web';

export default error()
  .status(500)
  .render(function GlobalError() {
    return (
      <html>
        <head>
          <title>Error</title>
        </head>
        <body>
          <h1>Application Error</h1>
          <p>Something went wrong. Please try again later.</p>
          <a href="/">Go home</a>
        </body>
      </html>
    );
  });
```

### Error recovery

```tsx
import { error, useRouteError, useNavigate } from '@putnami/web';

export default error().render(function ErrorBoundary() {
  const err = useRouteError();
  const navigate = useNavigate();

  const retry = () => {
    navigate(0); // Refresh current route
  };

  return (
    <div>
      <h1>Error</h1>
      <p>{err instanceof Error ? err.message : 'Unknown error'}</p>
      <button onClick={retry}>Try again</button>
    </div>
  );
});
```

## Navigation

### Link component

```tsx
import { Link } from '@putnami/web';

function Navigation() {
  return (
    <nav>
      <Link to="/">Home</Link>
      <Link to="/about">About</Link>
      <Link to="/users" prefetch="intent">Users</Link>
      <Link to={`/users/${userId}`}>User Profile</Link>
    </nav>
  );
}
```

### Programmatic navigation

```tsx
import { useNavigate } from '@putnami/web';

function LoginButton() {
  const navigate = useNavigate();

  const handleLogin = async () => {
    await login();
    navigate('/dashboard');
  };

  // Navigate with replace (no back button)
  const handleReplace = () => {
    navigate('/new-page', { replace: true });
  };

  // Navigate back
  const goBack = () => {
    navigate(-1);
  };

  return <button onClick={handleLogin}>Login</button>;
}
```

### Active links

```tsx
import { NavLink } from '@putnami/web';

function Navigation() {
  return (
    <nav>
      <NavLink
        to="/dashboard"
        className={({ isActive }) => isActive ? 'active' : ''}
      >
        Dashboard
      </NavLink>
    </nav>
  );
}
```

## Route parameters and search params

### URL parameters

```tsx
import { useParams } from '@putnami/web';

function UserPage() {
  const { id, tab } = useParams();
  // /users/123/settings → { id: '123', tab: 'settings' }

  return <div>User {id}, Tab: {tab}</div>;
}
```

### Search parameters

```tsx
import { useSearchParams } from '@putnami/web';

function SearchPage() {
  const [searchParams, setSearchParams] = useSearchParams();

  const query = searchParams.get('q') || '';
  const page = parseInt(searchParams.get('page') || '1', 10);

  const updateSearch = (newQuery: string) => {
    setSearchParams({ q: newQuery, page: '1' });
  };

  const nextPage = () => {
    setSearchParams({ q: query, page: String(page + 1) });
  };

  return (
    <div>
      <input
        value={query}
        onChange={(e) => updateSearch(e.target.value)}
      />
      <button onClick={nextPage}>Next page</button>
    </div>
  );
}
```

## Route matching

### useMatch

```tsx
import { useMatch } from '@putnami/web';

function Breadcrumb() {
  const isUsers = useMatch('/users/*');
  const isUserDetail = useMatch('/users/:id');

  return (
    <nav>
      <a href="/">Home</a>
      {isUsers && <a href="/users">Users</a>}
      {isUserDetail && <span>User Detail</span>}
    </nav>
  );
}
```

### useLocation

```tsx
import { useLocation } from '@putnami/web';

function Analytics() {
  const location = useLocation();

  useEffect(() => {
    trackPageView(location.pathname);
  }, [location]);

  return null;
}
```

## Preventing navigation

### useBlocker

```tsx
import { useBlocker } from '@putnami/web';

function EditForm() {
  const [isDirty, setIsDirty] = useState(false);

  const blocker = useBlocker(
    ({ currentLocation, nextLocation }) =>
      isDirty && currentLocation.pathname !== nextLocation.pathname
  );

  return (
    <div>
      <form onChange={() => setIsDirty(true)}>
        {/* form fields */}
      </form>

      {blocker.state === 'blocked' && (
        <dialog open>
          <p>You have unsaved changes. Leave anyway?</p>
          <button onClick={() => blocker.proceed()}>Leave</button>
          <button onClick={() => blocker.reset()}>Stay</button>
        </dialog>
      )}
    </div>
  );
}
```

### useBeforeUnload

```tsx
import { useBeforeUnload } from '@putnami/web';

function EditForm() {
  const [isDirty, setIsDirty] = useState(false);

  useBeforeUnload(
    useCallback(
      (event) => {
        if (isDirty) {
          event.preventDefault();
        }
      },
      [isDirty]
    )
  );

  return <form onChange={() => setIsDirty(true)}>{/* ... */}</form>;
}
```

## Related guides

- [Web](/docs/frameworks/typescript/web)
- [Forms and Actions](/docs/frameworks/typescript/forms-and-actions)
