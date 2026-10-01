# Browser vs Server

`@putnami/utils` uses conditional exports to provide different builds for browser and server environments. This ensures that browser bundles only include utilities that don't depend on server-specific modules, keeping bundle sizes small and avoiding runtime errors.

## How It Works

The package uses the `exports` field in `package.json` to conditionally export different entry points:

```json
{
  "exports": {
    ".": {
      "bun": "./src/server/index.ts",
      "browser": "./src/client/index.ts",
      "default": "./src/server/index.ts"
    }
  }
}
```

- **Browser builds**: Import from `./src/client/index.ts` (only shared utilities)
- **Server builds**: Import from `./src/server/index.ts` (all utilities)

## Available Utilities by Environment

### Shared Utilities (Browser + Server)

These utilities work in both browser and server environments and have no server-specific dependencies:

#### Object Utilities
- `getDeep(obj, path)` - Access nested properties via dot notation
- `mergeDeep(a, b)` - Deep merge two objects
- `sortObjectKeys(obj)` - Sort object keys alphabetically
- `omitUndefinedValues(obj)` - Remove undefined values recursively

#### Array Utilities
- `ensureArray(value)` - Wrap value in array if not already
- `first(arr)` - Get first element
- `last(arr)` - Get last element
- `unique(arr)` - Remove duplicates
- `chunk(arr, size)` - Split into chunks

#### Type Utilities
- `isString(value)` - Check if value is string
- `isObject(value)` - Check if value is object
- `isPlainObject(value)` - Check if plain object (not array/date)
- `isNullish(value)` - Check if null or undefined
- `isFunction(value)` - Check if function
- `throwError(msg)` - Throw error (never returns)
- `Promisable<T>` - Type for values that can be a promise or value

#### URL Utilities
- `buildUrlWithParams(url, params)` - Build URL with query/path params
- `parseQueryString(query)` - Parse query string to object
- `getBaseUrl(url)` - Get URL without query/hash
- `joinUrlPath(...segments)` - Join URL path segments

#### Task Utilities
- `TaskQueue` - Concurrent task processor with limit

### Server-Only Utilities

These utilities require server-specific modules and are only available in server environments (Bun):

#### Path Utilities
- `joinPath(...paths)` - Join path segments
- `relativePath(from, to)` - Get relative path
- `getDirectoryName(path)` - Get directory name
- `resolvePath(...paths)` - Resolve to absolute path
- `getBaseName(path)` - Get filename
- `getExtension(path)` - Get file extension
- `normalizePath(path)` - Normalize path
- `isAbsolutePath(path)` - Check if absolute
- `toPosixPath(path)` - Convert separators to forward slashes
- `joinPosixPath(...paths)` - Join path segments with forward slashes on every platform

#### Workspace Utilities
- `getWorkspaceRoot()` - Get workspace root path
- `getProjectRoot()` - Get current project root
- `getCurrentProject()` - Get current project info
- `getProject(path)` - Get project by path/name
- `listProjectDependencies(name)` - List all dependencies

#### Stack Utilities
- `getCallerInfo(depth)` - Get caller stack info
- `getExternalCaller()` - Get first external caller (outside @putnami)
- `parseStackTrace(error)` - Parse error stack trace

## Usage Examples

### Browser Environment

In browser code, you can safely import shared utilities:

```typescript
// ✅ Works in browser
import { getDeep, mergeDeep, isString, buildUrlWithParams } from '@putnami/utils';

const config = getDeep(data, 'user.settings');
const url = buildUrlWithParams('/api/users/[id]', { id: '123' });
```

```typescript
// ❌ Will fail in browser (not available)
import { joinPath, getWorkspaceRoot } from '@putnami/utils';
// These require server-specific path module
```

### Server Environment

In server code (Bun), you have access to all utilities:

```typescript
// ✅ Works in server
import {
  getDeep,           // Shared utility
  joinPath,          // Server-only utility
  getWorkspaceRoot,  // Server-only utility
} from '@putnami/utils';

const config = getDeep(data, 'user.settings');
const fullPath = joinPath(getWorkspaceRoot(), 'packages', 'core');
```

## Build Tool Configuration

Most modern build tools automatically detect conditional exports:

### Vite

Vite automatically uses the `browser` export when building for the browser:

```typescript
// vite.config.ts
import { defineConfig } from 'vite';

export default defineConfig({
  build: {
    // Vite automatically uses browser exports
  },
});
```

### Webpack

Webpack 5+ supports conditional exports by default:

```typescript
// webpack.config.js
module.exports = {
  resolve: {
    conditionNames: ['browser', 'import', 'require'],
  },
};
```

### Bun

Bun automatically uses the `bun` export for server-side code:

```typescript
// Server code in Bun
import { joinPath, getWorkspaceRoot } from '@putnami/utils';
// ✅ Works - uses server exports
```

## TypeScript Types

TypeScript respects conditional exports, so you'll get proper type checking:

```typescript
// In browser code
import { joinPath } from '@putnami/utils';
// ❌ TypeScript error: joinPath is not exported from client build

// In server code
import { joinPath } from '@putnami/utils';
// ✅ Works - TypeScript knows it's available
```

## Troubleshooting

### "Module not found" in Browser

If you're getting errors about server-only utilities in browser code:

1. **Check your build tool configuration** - Ensure it's using the `browser` export
2. **Verify imports** - Don't import server-only utilities in browser code
3. **Check environment** - Make sure your bundler knows it's building for browser

### "Module not found" in Server

If server-only utilities aren't available:

1. **Check Bun version** - Ensure you're using Bun v1.4.0 or higher
2. **Verify build tool** - Some build tools may incorrectly use browser exports
3. **Check package.json exports** - Ensure conditional exports are properly configured

### Bundle Size Concerns

If your browser bundle is too large:

1. **Use tree-shaking** - Modern bundlers automatically remove unused code
2. **Import specific functions** - Use named imports instead of `import *`
3. **Check for server-only imports** - Ensure you're not accidentally importing server utilities

## Best Practices

1. **Separate browser and server code** - Keep browser and server code in separate files when possible
2. **Use type guards** - Use type checking utilities to ensure values are correct types
3. **Leverage conditional exports** - Let the build tool handle environment detection
4. **Test both environments** - Ensure your code works in both browser and server contexts

## Next Steps

- See [Getting Started](./getting-started.md) for installation and basic usage
- Explore the complete [API Reference](./api-reference.md) for all available functions
