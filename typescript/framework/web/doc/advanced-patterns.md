# Advanced Patterns

This guide covers advanced patterns, optimization techniques, testing strategies, and deployment considerations for production React applications.

## Authentication & Route Guards

### Declarative Route Protection

The recommended approach is to use the `layout()` builder in `layout.tsx` and the `page()` builder in `page.tsx` to declare authentication and authorization requirements declaratively. This leverages the framework's OAuth middleware.

#### Protecting an entire section

Use `layout()` in `layout.tsx` to apply authentication to all pages under a layout:

```tsx
// src/app/dashboard/layout.tsx
import { layout } from '@putnami/web';
import { Outlet } from '@putnami/web';

export default layout()
  .secure({ roles: ['user'] })
  .render(function DashboardLayout() {
    return <Outlet />;
  });
```

All pages under `/dashboard/*` now require a valid OAuth token with the `user` role. Unauthenticated requests are rejected before the page renders.

#### Protecting a single page

Use the `page()` builder directly in `page.tsx` for page-specific security:

```tsx
// src/app/admin/settings/page.tsx
import { page } from '@putnami/web';

export default page()
  .secure({ roles: ['admin'], scopes: ['settings:write'] })
  .render(() => <AdminSettings />);
```

#### Composing layout and page middleware

Layout and page middleware compose automatically. Layout middleware runs first (outermost), then page middleware:

```
src/app/
├── layout.tsx                # layout().render(...)
└── admin/
    ├── layout.tsx            # layout().secure({ roles: ['admin'] }).render(...)
    └── settings/
        └── page.tsx          # page().secure({...}).render(...) → Gets: Auth → Secure → render
```

### Role-Based Access Control

```tsx
// src/app/admin/layout.tsx
import { layout } from '@putnami/web';
import { Outlet } from '@putnami/web';

export default layout()
  .secure({ roles: ['admin'] })
  .render(function AdminLayout() {
    return <Outlet />;
  });
```

For finer-grained control with scopes:

```tsx
// src/app/admin/users/page.tsx
import { page } from '@putnami/web';

export default page()
  .secure({ roles: ['admin'], scopes: ['users:manage'] })
  .render(() => <UserManagement />);
```

### Custom Authentication Logic

For cases where OAuth middleware isn't sufficient, use loaders for custom logic:

```typescript
// src/app/dashboard/loader.ts
import { loader } from '@putnami/web';
import { redirect } from '@putnami/application';

export default loader(async (ctx) => {
  const user = await getCurrentUser(ctx);

  if (!user) {
    return redirect('/login');
  }

  const dashboardData = await getDashboardData(user.id);
  return { user, dashboardData };
});
```

### Shared Authentication Logic

Create a reusable auth utility for loader-based protection:

```typescript
// src/lib/auth.ts
import type { HttpRequestContext } from '@putnami/application';
import { HttpResponse, redirect } from '@putnami/application';

export async function requireAuth(ctx: HttpRequestContext) {
  const user = await getCurrentUser(ctx);
  if (!user) {
    return redirect('/login');
  }
  return user;
}

export async function requireRole(
  ctx: HttpRequestContext,
  role: string
) {
  const user = await requireAuth(ctx);
  if (user instanceof HttpResponse) {
    return user; // Redirect response
  }

  if (user.role !== role) {
    throw new HttpException(403, 'Forbidden');
  }

  return user;
}
```

Use in loaders:

```typescript
// src/app/dashboard/loader.ts
import { loader } from '@putnami/web';
import { requireAuth } from '../../lib/auth';
import { HttpResponse } from '@putnami/application';

export default loader(async (ctx) => {
  const user = await requireAuth(ctx);
  if (user instanceof HttpResponse) {
    return user; // Redirect
  }

  return { user };
});
```

## Data Prefetching Strategies

### Prefetch on Hover

```tsx
import { Link } from '@putnami/web';

export default function Navigation() {
  return (
    <nav>
      <Link to="/dashboard" prefetch="intent">
        Dashboard
      </Link>
    </nav>
  );
}
```

### Prefetch Critical Routes

```tsx
import { useEffect } from 'react';
import { useFetcher } from '@putnami/web';

export default function HomePage() {
  const fetcher = useFetcher();

  // Prefetch dashboard data when home page loads
  useEffect(() => {
    fetcher.load('/dashboard');
  }, [fetcher]);

  return (
    <div>
      <h1>Home</h1>
      <Link to="/dashboard">Go to Dashboard</Link>
    </div>
  );
}
```

### Prefetch Based on User Behavior

```tsx
import { useEffect, useState } from 'react';
import { useFetcher } from '@putnami/web';

export default function ProductList() {
  const [hoveredId, setHoveredId] = useState<string | null>(null);
  const fetcher = useFetcher();

  useEffect(() => {
    if (hoveredId) {
      fetcher.load(`/products/${hoveredId}`);
    }
  }, [hoveredId, fetcher]);

  return (
    <ul>
      {products.map(product => (
        <li
          key={product.id}
          onMouseEnter={() => setHoveredId(product.id)}
        >
          <Link to={`/products/${product.id}`}>
            {product.name}
          </Link>
        </li>
      ))}
    </ul>
  );
}
```

## Code Splitting

### Route-Level Splitting

Routes are automatically code-split. Use lazy loading for heavy components:

```tsx
import { lazy, Suspense } from 'react';

const HeavyChart = lazy(() => import('../components/HeavyChart'));

export default function AnalyticsPage() {
  return (
    <div>
      <h1>Analytics</h1>
      <Suspense fallback={<div>Loading chart...</div>}>
        <HeavyChart />
      </Suspense>
    </div>
  );
}
```

### Component-Level Splitting

```tsx
import { lazy, Suspense } from 'react';

const Modal = lazy(() => import('../components/Modal'));

export default function Page() {
  const [showModal, setShowModal] = useState(false);

  return (
    <div>
      <button onClick={() => setShowModal(true)}>Open Modal</button>
      {showModal && (
        <Suspense fallback={<div>Loading...</div>}>
          <Modal onClose={() => setShowModal(false)} />
        </Suspense>
      )}
    </div>
  );
}
```

## Performance Optimization

### Memoization

Memoize expensive computations:

```tsx
import { useMemo } from 'react';

export default function ProductList({ products }: { products: Product[] }) {
  const sortedProducts = useMemo(() => {
    return products.sort((a, b) => a.price - b.price);
  }, [products]);

  return (
    <ul>
      {sortedProducts.map(product => (
        <li key={product.id}>{product.name}</li>
      ))}
    </ul>
  );
}
```

### Callback Memoization

```tsx
import { useCallback } from 'react';

export default function SearchPage() {
  const [query, setQuery] = useState('');

  const handleSearch = useCallback((newQuery: string) => {
    setQuery(newQuery);
    // Expensive search operation
  }, []);

  return (
    <input
      value={query}
      onChange={(e) => handleSearch(e.target.value)}
    />
  );
}
```

### Virtual Scrolling

For long lists:

```tsx
import { useVirtualizer } from '@tanstack/react-virtual';

export default function LongList({ items }: { items: Item[] }) {
  const parentRef = useRef<HTMLDivElement>(null);

  const virtualizer = useVirtualizer({
    count: items.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => 50,
  });

  return (
    <div ref={parentRef} style={{ height: '400px', overflow: 'auto' }}>
      <div style={{ height: `${virtualizer.getTotalSize()}px`, position: 'relative' }}>
        {virtualizer.getVirtualItems().map((virtualItem) => (
          <div
            key={virtualItem.key}
            style={{
              position: 'absolute',
              top: 0,
              left: 0,
              width: '100%',
              height: `${virtualItem.size}px`,
              transform: `translateY(${virtualItem.start}px)`,
            }}
          >
            {items[virtualItem.index].name}
          </div>
        ))}
      </div>
    </div>
  );
}
```

### Image Optimization

```tsx
export default function ImageGallery({ images }: { images: Image[] }) {
  return (
    <div>
      {images.map(image => (
        <img
          key={image.id}
          src={image.src}
          alt={image.alt}
          loading="lazy"
          decoding="async"
        />
      ))}
    </div>
  );
}
```

## Testing Strategies

### Testing Loaders

```typescript
// src/app/posts/loader.test.ts
import { describe, it, expect } from 'bun:test';
import { loader } from './loader';
import { createMockContext } from '../test-utils';

describe('posts loader', () => {
  it('should return posts', async () => {
    const ctx = createMockContext();
    const result = await loader(ctx);

    expect(result).toHaveProperty('posts');
    expect(Array.isArray(result.posts)).toBe(true);
  });
});
```

### Testing Actions

```typescript
// src/app/posts/action.test.ts
import { describe, it, expect } from 'bun:test';
import { action } from './action';
import { createMockContext } from '../test-utils';

describe('create post action', () => {
  it('should create a post', async () => {
    const ctx = createMockContext({
      method: 'POST',
      body: new FormData(),
    });

    ctx.req.formData = async () => {
      const fd = new FormData();
      fd.set('title', 'Test Post');
      return fd;
    };

    const result = await action(ctx);

    expect(result).toHaveProperty('ok', true);
  });
});
```

### Testing Components

```tsx
// src/app/posts/page.test.tsx
import { describe, it, expect } from 'bun:test';
import { render, screen } from '@testing-library/react';
import { useLoaderData } from '@putnami/web';
import PostsPage from './page';

// Mock the hook
jest.mock('@putnami/web', () => ({
  ...jest.requireActual('@putnami/web'),
  useLoaderData: () => ({
    posts: [{ id: '1', title: 'Test Post' }],
  }),
}));

describe('PostsPage', () => {
  it('should render posts', () => {
    render(<PostsPage />);
    expect(screen.getByText('Test Post')).toBeInTheDocument();
  });
});
```

## Error Handling Patterns

### Global Error Boundary

```tsx
// src/app/error.tsx
import { error, useRouteError, Link } from '@putnami/web';

export default error().render(function ErrorBoundary() {
  const err = useRouteError() as Error;

  return (
    <div>
      <h1>Something went wrong</h1>
      <p>{err.message}</p>
      {process.env.NODE_ENV === 'development' && (
        <pre>{err.stack}</pre>
      )}
      <Link to="/">Go Home</Link>
    </div>
  );
});
```

### Route-Specific Error Handling

```tsx
// src/app/dashboard/error.tsx
import { error, useRouteError } from '@putnami/web';

export default error().render(function DashboardError() {
  const err = useRouteError() as Error;

  if (err.status === 403) {
    return (
      <div>
        <h2>Access Denied</h2>
        <p>You don't have permission to view this page.</p>
      </div>
    );
  }

  return (
    <div>
      <h2>Dashboard Error</h2>
      <p>{err.message}</p>
    </div>
  );
});
```

## Deployment Considerations

### Environment Variables

```typescript
// src/main.ts
const config = {
  apiUrl: process.env.API_URL || 'http://localhost:3000',
  databaseUrl: process.env.DATABASE_URL,
  // ...
};

export const app = () => application().use(
  react({
    isDevelopment: process.env.NODE_ENV !== 'production',
  })
);
```

### Build Process

```bash
# Build for production
NODE_ENV=production bun run build

# Start production server
NODE_ENV=production bun run src/main.ts
```

### Docker Deployment

```dockerfile
FROM oven/bun:latest

WORKDIR /app

COPY package.json bun.lock ./
RUN bun install --production

COPY . .
RUN bun run build

EXPOSE 3000

CMD ["bun", "run", "src/main.ts"]
```

### Static Asset Optimization

```typescript
react({
  minify: true,
  sourcemap: 'none',
  splitting: true, // Enable code splitting
})
```

## Plugin bootstrap data and client scripts

A server plugin often needs two things in the browser: a small amount of
per-request data, and a module that reads it. Two request-context slots cover
both. They are generic — the renderer serializes whatever is in them and never
inspects the keys.

```ts
import { mergeClientBootstrap, pushClientScript } from '@putnami/web';

// Inside an HTTP middleware, before next()
mergeClientBootstrap(ctx, { myPlugin: { sessionId, endpoint: '/_myplugin/events' } });
pushClientScript(ctx, '/myplugin-client.js');
```

The SSR renderer then emits, on every HTML page it renders:

```html
<script nonce="...">window.__putnamiBootstrap={"myPlugin":{...}};window.__staticRouterHydrationData = {...};</script>
...
<script type="module" nonce="..." src="/hydrate.js"></script>
<script type="module" nonce="..." src="/myplugin-client.js"></script>
```

Read the data in the browser with `window.__putnamiBootstrap`.

Behaviour worth knowing:

- **`mergeClientBootstrap` shallow-merges.** Two plugins can write; the later
  writer wins per top-level key. Namespace your entry under your own key.
- **`pushClientScript` deduplicates by URL** and normalizes a relative URL to an
  absolute path. Scripts are emitted **after** the hydrate script, so the router
  already exists when they run.
- **Both are nonce-stamped** with the per-request CSP nonce, so a strict
  `script-src 'nonce-...'` policy still allows them.
- **Values are escaped** through the same script-context escaper as hydration
  data. A bootstrap string containing `</script>` cannot terminate the tag.
- **Static pages do not consume these slots.** A prerendered page is served as a
  file, so no request context exists when it is delivered: no bootstrap, no
  injected script. Use an island or a `<Script>` tag in the document for
  behaviour a static page needs.

## Monitoring & Analytics

### Error Tracking

```tsx
// src/app/error.tsx
import { error, useRouteError } from '@putnami/web';
import { useEffect } from 'react';

export default error().render(function ErrorBoundary() {
  const err = useRouteError() as Error;

  useEffect(() => {
    // Send error to monitoring service
    if (typeof window !== 'undefined') {
      window.analytics?.track('error', {
        message: err.message,
        stack: err.stack,
      });
    }
  }, [err]);

  return (
    <div>
      <h1>Something went wrong</h1>
      <p>{err.message}</p>
    </div>
  );
});
```

### Performance Monitoring

```tsx
import { useEffect } from 'react';
import { useNavigation } from '@putnami/web';

export default function Page() {
  const navigation = useNavigation();

  useEffect(() => {
    if (navigation.state === 'idle') {
      // Page loaded, measure performance
      const perfData = performance.getEntriesByType('navigation')[0];
      console.log('Load time:', perfData.loadEventEnd - perfData.fetchStart);
    }
  }, [navigation.state]);

  return <div>Content</div>;
}
```

## Best Practices Summary

1. **Authentication** - Use `layout().render()` in `layout.tsx` and `page().render()` in `page.tsx` for declarative route protection; fall back to loaders for custom logic
2. **Performance** - Prefetch critical routes, use code splitting
3. **Error Handling** - Provide user-friendly error boundaries
4. **Testing** - Test loaders, actions, and components
5. **Monitoring** - Track errors and performance
6. **Optimization** - Memoize expensive operations, use virtual scrolling
7. **Deployment** - Use environment variables, optimize builds

## Next Steps

- Review [API Reference](api-reference.md) for complete API documentation
- Check [Troubleshooting](troubleshooting.md) for common issues
- Explore [Configuration](configuration.md) for deployment settings
