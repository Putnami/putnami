# Build an API service

You will create an API service with file-based routing, then add schema validation and typed request handling.

## Steps

### 1) Create the workspace and API project

```bash
putnami init --workspace my-workspace
cd my-workspace
putnami projects create api --template typescript-server
```

The `typescript-server` template scaffolds an API project with `@putnami/application`, file-based routing, and example GET/POST endpoints.

### 2) Run it

```bash
putnami serve api
```

Open `http://localhost:3000`. You see the default JSON response from the GET endpoint. Routes in `src/api/` mount at the root by default — the `api` folder name is for file organisation only.

### 3) Explore the project structure

The scaffolded project lives in `packages/api/`:

```
packages/api/
├── src/
│   ├── main.ts          # Application entrypoint
│   ├── serve.ts          # Server bootstrap
│   └── api/
│       ├── get.ts        # GET / endpoint
│       └── post.ts       # POST / endpoint
└── test/
    └── api.test.ts
```

### 4) Add a health check route

Create `packages/api/src/api/health/get.ts`:

```ts
import { endpoint } from '@putnami/application';

export default endpoint(() => {
  return { ok: true };
});
```

Navigate to `http://localhost:3000/health`.

### 5) Add a validated route

Create `packages/api/src/api/items/post.ts`:

```ts
import { endpoint, MinLength, Optional, ArrayOf } from '@putnami/application';

export default endpoint()
  .body({
    name: MinLength(1),
    description: Optional(String),
    tags: Optional(ArrayOf(String)),
  })
  .handle(async (ctx) => {
    const { name, description, tags } = await ctx.body();
    return {
      item: {
        id: crypto.randomUUID(),
        name,
        description: description ?? '',
        tags: tags ?? [],
      },
    };
  });
```

Sending an invalid body returns a `400` automatically:

```http
POST /items
Content-Type: application/json
{ "name": "" }

-> 400 { "message": "body.name must have length >= 1", ... }
```

### 6) Add a route with path parameter validation

Create `packages/api/src/api/items/[id]/get.ts`:

```ts
import { endpoint, Uuid } from '@putnami/application';

export default endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    // ctx.params.id is typed as string and validated as UUID
    return { item: { id: ctx.params.id } };
  });
```

## Result

You have a running API service with file-based routing and built-in schema validation. Invalid requests are rejected before your handler runs with structured error messages.

## Next steps

- See the [API reference](/docs/frameworks/typescript/api) for all schema types (`Uuid`, `Email`, `Int`, `Min`, `Max`, `Pattern`, ...) and builder methods
- Add [persistence](/docs/how-to/add-persistence) for a database-backed API
- Add [authentication](/docs/how-to/add-authentication) to protect routes

> **Companion sample:** [`typescript/samples/02-rest-api`](https://github.com/putnami/putnami/tree/main/typescript/samples/02-rest-api) — a runnable CRUD API with validation and OpenAPI. Run `putnami serve @example/rest-api` from the workspace root.
