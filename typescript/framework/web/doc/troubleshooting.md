# Troubleshooting

Common issues and solutions when building React applications with `@putnami/web`.

## Hydration Mismatches

### Issue: "Hydration failed because the initial UI does not match"

**Cause:** Server and client render different HTML.

**Solutions:**

1. **Avoid browser-only APIs during render:**
   ```tsx
   // ❌ Bad
   export default function Page() {
     const width = window.innerWidth;
     return <div>{width}</div>;
   }

   // ✅ Good
   export default function Page() {
     const [width, setWidth] = useState(0);
     useEffect(() => {
       setWidth(window.innerWidth);
     }, []);
     return <div>{width || 0}</div>;
   }
   ```

2. **Use consistent random values:**
   ```tsx
   // ❌ Bad
   export default function Page() {
     return <div>{Math.random()}</div>;
   }

   // ✅ Good
   export default function Page() {
     const [value] = useState(() => Math.random());
     return <div>{value}</div>;
   }
   ```

3. **Handle dates consistently:**
   ```tsx
   // ❌ Bad
   export default function Page() {
     return <div>{new Date().toLocaleString()}</div>;
   }

   // ✅ Good
   export default function Page() {
     const [time, setTime] = useState('');
     useEffect(() => {
       setTime(new Date().toLocaleString());
     }, []);
     return <div>{time || 'Loading...'}</div>;
   }
   ```

## Routes Not Found

### Issue: Routes return 404

**Causes and Solutions:**

1. **Check file naming:**
   - Files must be named `page.tsx` (not `Page.tsx` or `index.tsx`)
   - Layouts must be `layout.tsx`
   - Loaders must be `loader.ts`

2. **Verify scan folder:**
   ```typescript
   // Check your configuration
   react({
     scanFolder: 'app', // Should match your folder structure
   })
   ```

3. **Check file location:**
   ```
   src/
   └── app/          ← Plugin scans here
       └── page.tsx
   ```

4. **Restart development server:**
   Routes are generated on startup. Restart after adding new routes.

## Loader Errors

### Issue: Loader returns undefined or errors

**Solutions:**

1. **Always return data:**
   ```typescript
   import { loader } from '@putnami/web';

   // ❌ Bad
   export default loader(async (ctx) => {
     await fetchData(); // No return
   });

   // ✅ Good
   export default loader(async (ctx) => {
     const data = await fetchData();
     return { data };
   });
   ```

2. **Handle errors properly:**
   ```typescript
   import { loader } from '@putnami/web';
   import { HttpException } from '@putnami/runtime';

   export default loader(async (ctx) => {
     try {
       const data = await fetchData();
       return { data };
     } catch (error) {
       throw new HttpException(500, 'Failed to load data');
     }
   });
   ```

3. **Check return type:**
   Loaders must return serializable data (JSON-compatible).

## Action Not Working

### Issue: Form submission doesn't trigger action

**Solutions:**

1. **Check form method:**
   ```tsx
   // ✅ Correct
   <Form method="post">
     <button type="submit">Submit</button>
   </Form>
   ```

2. **Verify action file exists:**
   ```
   src/app/contact/
   ├── page.tsx
   └── action.ts  ← Must exist
   ```

3. **Check action export:**
   ```typescript
   import { action } from '@putnami/web';

   // ✅ Correct
   export default action(async (ctx) => {
     // ...
   });
   ```

4. **Verify route matches:**
   Action must be in the same route folder as the page.

## TypeScript Errors

### Issue: Type errors with route parameters

**Solution:**

TypeScript automatically infers route parameters from file structure:

```typescript
// Route: /posts/[id]
import { loader } from '@putnami/web';

export default loader(async (ctx) => {
  // ctx.params.id is automatically typed as string
  const id = ctx.params.id;
  // ...
});
```

If you get type errors, ensure:
1. File structure matches route pattern
2. Dynamic segments use `[param]` syntax
3. TypeScript can resolve the route structure

### Issue: useLoaderData type errors

**Solution:**

Always provide a type parameter:

```typescript
// ✅ Good
interface PageData {
  user: User;
}

export default function Page() {
  const { user } = useLoaderData<PageData>();
  // ...
}
```

## Build Errors

### Issue: Client bundle fails to build

**Solutions:**

1. **Check for TypeScript errors:**
   ```bash
   bun run build
   ```

2. **Verify imports:**
   Ensure all imports are correct and dependencies are installed.

3. **Check conditional exports:**
   Browser-only code should use conditional exports:
   ```typescript
   if (typeof window !== 'undefined') {
     // Browser-only code
   }
   ```

### Issue: Routes not generated

**Solutions:**

1. **Check autoScan:**
   ```typescript
   react({
     autoScan: true, // Must be true
   })
   ```

2. **Verify scanPath:**
   ```typescript
   react({
     scanPath: '/absolute/path/to/app', // Or let it auto-detect
   })
   ```

3. **Check file structure:**
   Ensure files follow naming conventions.

## Performance Issues

### Issue: Slow page loads

**Solutions:**

1. **Optimize loaders:**
   ```typescript
   import { loader } from '@putnami/web';

   // Use parallel loading
   export default loader(async (ctx) => {
     const [user, posts, stats] = await Promise.all([
       getUser(),
       getPosts(),
       getStats(),
     ]);
     return { user, posts, stats };
   });
   ```

2. **Enable code splitting:**
   ```typescript
   react({
     splitting: true,
   })
   ```

3. **Use prefetching:**
   ```tsx
   <Link to="/dashboard" prefetch="intent">Dashboard</Link>
   ```

### Issue: SSR timeout

**Solutions:**

1. **Increase timeout:**
   ```typescript
   react({
     ssrTimeout: 5000, // 5 seconds
   })
   ```

2. **Optimize slow operations:**
   - Move heavy work to background jobs
   - Cache expensive computations
   - Use streaming for large data

## Development Issues

### Issue: Changes not reflected

**Solutions:**

1. **Restart development server:**
   Routes are generated on startup.

2. **Clear generated files:**
   ```bash
   rm -rf .gen
   ```

3. **Check file watcher:**
   Ensure file changes are detected.

### Issue: Hot reload not working

**Solutions:**

1. **Check file structure:**
   Ensure files are in the correct location.

2. **Verify imports:**
   Circular imports can break hot reload.

3. **Restart server:**
   Sometimes a full restart is needed.

## Deployment Issues

### Issue: Production build fails

**Solutions:**

1. **Set environment variables:**
   ```bash
   NODE_ENV=production bun run build
   ```

2. **Check configuration:**
   ```typescript
   react({
     isDevelopment: false,
     minify: true,
     sourcemap: 'none',
   })
   ```

3. **Verify all dependencies:**
   Ensure production dependencies are installed.

### Issue: Routes return 404 in production

**Solutions:**

1. **Check build output:**
   Ensure routes are generated in `.gen/`.

2. **Verify public folder:**
   Client bundles should be in `.gen/public/`.

3. **Check server configuration:**
   Ensure server serves static files correctly.

## Common Patterns

### Debugging Loaders

```typescript
import { loader } from '@putnami/web';

export default loader(async (ctx) => {
  console.log('Loader called:', ctx.req.url);
  console.log('Params:', ctx.params);

  try {
    const data = await fetchData();
    console.log('Data loaded:', data);
    return { data };
  } catch (error) {
    console.error('Loader error:', error);
    throw error;
  }
});
```

### Debugging Actions

```typescript
import { action } from '@putnami/web';

export default action(async (ctx) => {
  const formData = await ctx.req.formData();
  console.log('Form data:', Object.fromEntries(formData));

  try {
    const result = await processForm(formData);
    console.log('Action result:', result);
    return { result, ok: true, status: 200 };
  } catch (error) {
    console.error('Action error:', error);
    throw error;
  }
});
```

### Checking Route Generation

```typescript
// Add logging to see generated routes
const reactApp = new ReactApplication();
console.log('Routes:', reactApp.getHttpPlugin().getRoutes());
```

## Getting Help

If you're still experiencing issues:

1. **Check the documentation:**
   - [Getting Started](getting-started.md)
   - [File-based Routing](file-based-routing.md)
   - [Data Loading](data-loading.md)

2. **Review examples:**
   - Check `samples/react/` for working examples

3. **Debug step by step:**
   - Start with a minimal example
   - Add complexity gradually
   - Check console/terminal for errors

4. **Common mistakes:**
   - Forgetting to return data from loaders
   - Using browser APIs during SSR
   - Incorrect file naming
   - Missing exports

## Next Steps

- Review [Getting Started](getting-started.md) for setup
- Check [Configuration](configuration.md) for settings
- Explore [Advanced Patterns](advanced-patterns.md) for solutions
