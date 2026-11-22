# Build a Postgres API in TypeScript

You will build an HTTP API in TypeScript that stores items in Postgres, test it against a real database, and run it as a Docker image.

## Where the docs are

- On putnami.dev: [API](/docs/frameworks/typescript/api), [Persistence](/docs/frameworks/typescript/persistence), [Testing](/docs/frameworks/typescript/testing), [Configuration](/docs/frameworks/typescript/configuration) and [Publish & Docker](/docs/frameworks/typescript/extension/publish-and-docker).
- On your machine: every `@putnami/*` package ships its API reference as `AI.md`, with its `doc/` folder, in `node_modules`, such as `node_modules/@putnami/database/AI.md`.

## Steps

### 1) Create the workspace and the API project

`putnami init` sets up the workspace in the current directory:

```bash
mkdir shop && cd shop
putnami init --project api
```

The `typescript-server` template, the TypeScript default, creates the project in `api/`: `src/main.ts` composes the application, `src/api/` holds one file per route, and `test/api.test.ts` tests them. `putnami serve api` runs it.

### 2) Add the database package

```bash
putnami deps add @putnami/database --projects api
```

### 3) Declare the table and its migration

Create `api/src/db/items.ts`:

```ts
import { Column, Key, Repository, Table, Uuid, sqlSourceInline } from '@putnami/database';

export const ItemsTable = Table('items', {
  id: Key(Uuid),
  name: Column(String),
});

export class ItemRepository extends Repository<typeof ItemsTable> {
  constructor() {
    super(ItemsTable);
  }
}

export const itemsMigrations = {
  name: 'items-migrations',
  migrationSources: () => [
    sqlSourceInline({
      namespace: 'items',
      datasource: { name: 'default', schema: 'public' },
      definitions: [
        {
          name: '20260101000000-create-items',
          sql: 'CREATE TABLE items (id UUID PRIMARY KEY, name TEXT NOT NULL);',
          down: 'DROP TABLE items;',
        },
      ],
    }),
  ],
};
```

A table definition creates no SQL: the migration owns the table. TypeScript declares migrations inline; it has no loader for `.sql` files.

### 4) Connect the service

Replace `api/src/main.ts`. With `autoApply`, the service applies its migrations at startup, before it listens:

```ts
import { api, application, http, logger } from '@putnami/application';
import { sql } from '@putnami/database';
import { itemsMigrations } from './db/items';

export const app = () =>
  application().use(http()).use(logger()).use(sql({ autoApply: true })).use(itemsMigrations).use(api());
```

The service reads its connection from `DATABASE_URL`, or from `database.default.url` in a `conf/` file such as `api/conf/.env.local.yaml`. A binding that a deploy target or `provision()` injects wins over both. Add `?sslmode=disable` for a server without TLS other than `localhost`. `putnami serve api` starts no database: give it one there.

### 5) Add the routes

Create `api/src/api/items/post.ts`:

```ts
import { endpoint, json, MinLength } from '@putnami/application';
import { ItemRepository } from '../../db/items';

export default endpoint()
  .body({ name: MinLength(1) })
  .handle(async (ctx) => {
    const { name } = await ctx.body();
    const item = await new ItemRepository().save({ id: crypto.randomUUID(), name });
    return json(item, { status: 201 });
  });
```

Create `api/src/api/items/[id]/get.ts`:

```ts
import { endpoint, Uuid } from '@putnami/application';
import { NotFoundException } from '@putnami/runtime';
import { ItemRepository } from '../../../db/items';

export default endpoint()
  .params({ id: Uuid })
  .handle(async (ctx) => {
    const item = await new ItemRepository().get({ id: ctx.params.id });
    if (!item) throw new NotFoundException(`item ${ctx.params.id}`);
    return item;
  });
```

Create `api/src/api/items/[id]/delete.ts`. A handler that returns nothing answers `204 No Content`:

```ts
import { endpoint, Uuid } from '@putnami/application';
import { ItemRepository } from '../../../db/items';

export default endpoint()
  .params({ id: Uuid })
  .handle(async (ctx) => {
    await new ItemRepository().delete({ id: ctx.params.id });
  });
```

### 6) Build

```bash
putnami build --projects api
```

The build writes `api/infra/requirements.json`, which says the service needs a Postgres database, and the migration bundle under `api/.gen/migration-bundle/`. Commit `infra/requirements.json`: `putnami test` reads it to give the tests a database. Build again after you add or change a migration.

### 7) Test against Postgres

`putnami test` starts a throwaway Postgres, in Docker or from a local install. `provision()` creates an isolated database for the suite and applies the migrations the build bundled, so the application starts with `sql()`, without `autoApply`. Create `api/test/items.test.ts`:

```ts
import { afterAll, beforeAll, expect, it } from 'bun:test';
import { api } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { provision, sql } from '@putnami/database';

let db: Awaited<ReturnType<typeof provision>>;
let testApp: TestApp;

beforeAll(async () => {
  db = await provision();
  testApp = await createTestApp({ plugins: [sql(), api({ scanPath: 'src/api' })] });
});

afterAll(async () => {
  await testApp?.stop();
  await db?.cleanup();
});

it('creates, reads and deletes an item', async () => {
  const created = await testApp.fetch('/items', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: 'apple' }),
  });
  expect(created.status).toBe(201);
  const { id } = await created.json();

  expect((await testApp.fetch(`/items/${id}`)).status).toBe(200);
  expect((await testApp.fetch(`/items/${id}`, { method: 'DELETE' })).status).toBe(204);
  expect((await testApp.fetch(`/items/${id}`)).status).toBe(404);
});
```

```bash
putnami test --projects api
```

### 8) Package the image

`putnami package` builds only the channels you turn on, and fails for a named project with none. Turn the image channel on in `api/putnami.json`, beside what the template wrote:

```json
{
  "options": {
    "package": {
      "docker": true,
      "port": 8080
    }
  }
}
```

```bash
putnami package --projects api --docker-load --docker-tag local
```

The image is tagged `api:c-<content hash>`, and `--docker-tag local` adds `api:local`; `--docker-load` loads it into Docker. Add `--platform linux/arm64` on Apple silicon. The image sets `PORT` to the package port, 3000 when none is set, and the service listens on it.

### 9) Run the image

Run Postgres and the service on one Docker network, and pass the connection in `DATABASE_URL`. Start the service once Postgres accepts connections: it connects and migrates before it listens, and stops when the database does not answer.

```bash
docker network create shop
docker run -d --name db --network shop -e POSTGRES_PASSWORD=postgres postgres:17
until docker exec db pg_isready -h 127.0.0.1 -U postgres; do sleep 1; done
docker run --rm --network shop -p 8080:8080 \
  -e DATABASE_URL='postgres://postgres:postgres@db:5432/postgres?sslmode=disable' \
  api:local
```

The service applies its migration at startup. In another shell:

```bash
curl -s -X POST localhost:8080/items -H 'Content-Type: application/json' -d '{"name":"apple"}'
```

## Result

You now have a TypeScript API that stores its data in Postgres, applies its migrations, passes its tests against a real database and runs as a Docker image.
