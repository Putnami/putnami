# Testing Utilities

Helpers for integration-testing `@putnami/application` apps with Bun's test runner.

## `createTestApp`

Creates, loads, and starts an application with an ephemeral HTTP port.
Eliminates the boilerplate of `application() → http({ port: 0 }) → load() → start()`.
Plugin generation hooks and generated-loader imports still run, but the helper does not publish
`.gen/schema/capabilities.json`; that manifest remains exclusively owned by the
scheduler's generate step.

Import from `@putnami/application/testing`.

```typescript
import { createTestApp } from '@putnami/application/testing';
import { platform } from '@putnami/application';

const { fetch, stop } = await createTestApp({
  plugins: [platform()],
});

const res = await fetch('/healthz');
expect(res.status).toBe(200);

await stop();
```

### Options

| Option | Type | Description |
|---|---|---|
| `plugins` | `(Plugin \| Module)[]` | Plugins to register (an `http({ port: 0 })` is always included) |
| `configure` | `(app: Application) => void` | Callback to configure the app before start (register providers, routes, etc.) |

### Return value (`TestApp`)

| Property | Type | Description |
|---|---|---|
| `app` | `Application` | The running application instance |
| `baseUrl` | `string` | Resolved base URL (e.g. `http://localhost:54321`) |
| `fetch` | `(path, init?) => Promise<Response>` | `fetch` pre-bound to `baseUrl` |
| `stop` | `() => Promise<void>` | Stop the application and release resources |

### Example with DI and routes

```typescript
const { fetch, stop } = await createTestApp({
  plugins: [sql(), events()],
  configure: (app) => {
    app.provide(MyService);
    const httpPlugin = app.getPlugin(HttpPlugin);
    httpPlugin.get('/test', () => ({ ok: true }));
  },
});
```

## `waitFor`

Poll an async function until it returns a defined value, or timeout.
Useful for waiting on async side-effects in tests without fragile `Bun.sleep()` calls.

```typescript
import { waitFor } from '@putnami/application/testing';

const user = await waitFor(async () => {
  return db.users.findOne({ email: 'test@example.com' });
});
expect(user).toBeDefined();
```

### Parameters

| Parameter | Type | Default | Description |
|---|---|---|---|
| `fn` | `() => Promise<T \| undefined>` | — | Async function to poll. Return a value to resolve, or `undefined` to keep polling. |
| `timeoutMs` | `number` | `4000` | Maximum time to wait before returning `undefined`. |
| `intervalMs` | `number` | `100` | Polling interval. |
