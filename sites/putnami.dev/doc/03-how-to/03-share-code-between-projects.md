# Share code between projects

You will create a shared library, export a service from it, and use it from an app via dependency injection.

## Steps

### 1) Create a shared package

```bash
putnami projects create @workspace/shared --template typescript-library
```

This scaffolds a TypeScript library at `packages/workspace/shared/` with exports and a test suite.

### 2) Export a shared service

Replace `packages/workspace/shared/src/index.ts`:

```ts
export class GreetingService {
  message() {
    return 'Hello from shared code';
  }
}
```

No decorators or runtime imports needed — services are plain classes.

### 3) Use it in an app

In your app `package.json`, add the shared package as a dependency:

```json
"@workspace/shared": "workspace:*"
```

Register the service in your application entry point (`src/main.ts`):

```ts
import { application, api, http } from '@putnami/application';
import { GreetingService } from '@workspace/shared';

export const app = () =>
  application()
    .use(http())
    .provide(GreetingService)
    .use(api());
```

Then inject it in a route, for example `src/api/hello/get.ts`:

```ts
import { endpoint } from '@putnami/application';
import { GreetingService } from '@workspace/shared';

export const GET = endpoint()
  .inject({ greeting: GreetingService })
  .handle((ctx) => ({ message: ctx.deps.greeting.message() }));
```

## Result

You now have shared code used by more than one project. The library is built, tested, and linked through workspace dependencies.

> **Companion sample:** [`typescript/samples/05-dependency-injection`](https://github.com/putnami/putnami/tree/main/typescript/samples/05-dependency-injection) — a runnable project with explicit provider registration, `endpoint().inject()`, and scoped services. Run `putnami serve @example/dependency-injection` from the workspace root.
