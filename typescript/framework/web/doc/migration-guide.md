# Migration Guide: React v1 to v2

This guide helps you migrate from React v1 to v2 in Putnami. The v2 release includes significant improvements to routing, SSR, and client-side features.

## Overview of Changes

### Breaking Changes

1. **Router Parameter Matching**: Fixed behavior for nested parameter routes (e.g., `/docs/[package]` vs `/docs/[package]/[topic]`)
2. **Path Normalization**: Trailing slashes are now automatically normalized
3. **React Router Upgrade**: Upgraded from `7.10.1` to `7.12.0`
4. **Generator API**: `ReactApplicationGenerator` now properly implements `addError()` method

### New Features

1. **Prefetching**: New `Link` component with intelligent prefetching (`intent`, `render`, `none`)
2. **Route Tree Utilities**: Shared utilities for consistent route ID generation
3. **Suffix Pattern Support**: Router now supports parameter routes with suffixes (e.g., `/users/[id].json`)
4. **Enhanced Type Safety**: Better TypeScript types for hydration data

## Step-by-Step Migration

### 1. Update Dependencies

Update your `package.json`:

```json
{
  "dependencies": {
    "@putnami/web": "workspace:*"  // or latest version
  }
}
```

Then run:
```bash
bun install
```

### 2. Update Router Usage

If you're using manual route configuration, no changes are required. However, the router now handles nested parameter routes more accurately.

**Before (v1):**
```typescript
// /docs/[package] might incorrectly match /docs/[package]/[topic]
router.add('/docs/[package]', 'package-handler');
router.add('/docs/[package]/[topic]', 'topic-handler');
```

**After (v2):**
```typescript
// Now correctly distinguishes between routes
router.add('/docs/[package]', 'package-handler');
router.add('/docs/[package]/[topic]', 'topic-handler');
// /docs/core -> matches package-handler only
// /docs/core/config -> matches topic-handler
```

### 3. Update Link Components

If you're using React Router's `Link` directly, consider switching to Putnami's enhanced `Link`:

**Before (v1):**
```tsx
import { Link } from 'react-router';

<Link to="/about">About</Link>
```

**After (v2):**
```tsx
import { Link } from '@putnami/web';

// Prefetch on hover/focus (default: 'none')
<Link to="/about" prefetch="intent">About</Link>

// Prefetch immediately on render
<Link to="/about" prefetch="render">About</Link>
```

### 4. Update Error Boundaries

If you're using the generator's `addError()` method, it now works correctly:

**Before (v1):**
```typescript
// addError() was not implemented
generator.addError('error.tsx'); // No-op
```

**After (v2):**
```typescript
// addError() now properly generates error boundary routes
generator.addError('error.tsx'); // Generates .reactError() call
```

### 5. Handle Path Normalization

Trailing slashes are now automatically normalized:

**Before (v1):**
```typescript
// /docs/ and /docs might be treated differently
router.find('/docs/'); // Might not match /docs route
```

**After (v2):**
```typescript
// Trailing slashes are normalized
router.find('/docs/'); // Automatically matches /docs route
router.find('/docs');  // Same result
```

### 6. Update Type Definitions

If you're accessing hydration data directly, use the new type:

**Before (v1):**
```typescript
const hydrationData = (window as any).__staticRouterHydrationData;
```

**After (v2):**
```typescript
// Type is now properly defined
const hydrationData = window.__staticRouterHydrationData;
// hydrationData is typed as StaticRouterHydrationData | undefined
```

## Adopting render modes (SSG / ISR / islands)

The render-mode APIs are **additive** — existing SSR pages keep working
unchanged. Opt in per route:

- Add `.static()` to a `page()` to pre-render it (SSG). See
  [Rendering Modes](./rendering-modes.md).
- Add `{ revalidate }` for ISR; enumerate dynamic params with `{ paths }`.
- Add `loader().static()` to run a loader at build and bake its data in.
- Move interactivity into `*.island.tsx` files (`island().load(strategy)`).

**Behaviour change to know about:** a `.static()` page is served as
zero-JavaScript HTML — it does **not** receive full-page client-side hydration.
Interactivity on a static page must come from islands. SSR pages are unaffected
and keep full-page hydration. A `.static()` route that reads request data
(`ctx.user`, headers, query, cookies) now fails the build with a
`StaticRenderViolation` instead of silently rendering per request — drop
`.static()` to keep it as SSR.

Each build also writes `.gen/putnami-web-manifest.json`; commit a baseline and
run `putnami-web-manifest-diff` in CI to review rendering changes.

## Testing Your Migration

1. **Test Route Matching**: Verify that nested parameter routes work correctly
2. **Test Path Normalization**: Ensure routes with/without trailing slashes work
3. **Test Prefetching**: Verify that link prefetching works as expected
4. **Test Error Boundaries**: Ensure error boundaries are properly registered
5. **Test Render Modes**: Verify static routes serve pre-rendered HTML and islands hydrate

## Common Issues

### Issue: Routes not matching after upgrade

**Solution**: Check that your route definitions don't rely on the old (incorrect) matching behavior. Nested parameter routes should now work correctly.

### Issue: Hydration errors

**Solution**: Ensure your server and client are using the same version of `@putnami/web`. The hydration data type has been improved for better type safety.

### Issue: Prefetching not working

**Solution**: Make sure you're using the `Link` component from `@putnami/web`, not directly from `react-router`.

## Need Help?

- Check the [Troubleshooting Guide](./troubleshooting.md)
- Review the [API Reference](./api-reference.md)
- See [Advanced Patterns](./advanced-patterns.md) for migration examples
