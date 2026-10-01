# Schema & Validation

The schema module (`@putnami/runtime`) provides type-safe schema definitions with compile-time inference and runtime validation. Schemas are used across the framework for endpoint bodies, query parameters, configuration, and domain models.

## Defining a Schema

Use native constructors (`String`, `Number`, `Boolean`) and schema helpers to describe the shape of your data:

```typescript
import { schema, Optional, Int, Email, OneOf, ArrayOf } from '@putnami/runtime';

const TaskSchema = schema({
  title: String,
  description: Optional(String),
  priority: Int,
  status: OneOf('todo', 'in_progress', 'done'),
  assignees: ArrayOf(Email),
});
```

The `schema()` helper preserves full type inference — `InferSchema<typeof TaskSchema>` produces the equivalent TypeScript type automatically.

## Primitives

| Schema | TypeScript type | Validation |
|--------|----------------|------------|
| `String` | `string` | Must be a string |
| `Number` | `number` | Must be a number |
| `Boolean` | `boolean` | Must be a boolean |

## Built-in Types

| Schema | TypeScript type | Validation |
|--------|----------------|------------|
| `Uuid` | `string` | UUID v4 format |
| `Email` | `string` | Basic email format |
| `Int` | `number` | Integer (no decimals) |
| `Url` | `string` | Valid URL |
| `DateIso` | `string` | ISO 8601 date (`2024-01-15` or `2024-01-15T10:30:00Z`) |

## Constraints

Numeric and string constraints restrict values beyond their base type:

```typescript
import { Min, Max, MinLength, MaxLength, Pattern, OneOf, Constrained } from '@putnami/runtime';

const PasswordSchema = schema({
  username: MinLength(3),
  password: Constrained(MinLength(8), MaxLength(128)),
  age: Constrained(Min(0), Max(150)),
  role: OneOf('admin', 'editor', 'viewer'),
  code: Pattern(/^[A-Z]{3}-\d{4}$/),
});
```

| Constraint | Applies to | Description |
|------------|-----------|-------------|
| `Min(n)` | `number` | Value must be >= n |
| `Max(n)` | `number` | Value must be <= n |
| `MinLength(n)` | `string` | Length must be >= n |
| `MaxLength(n)` | `string` | Length must be <= n |
| `Pattern(regex)` | `string` | Must match the regex |
| `OneOf(...values)` | `string` | Must be one of the listed values |
| `Constrained(...descriptors)` | any | Combine multiple constraints |

## Collections

```typescript
import { ArrayOf, MapOf, Optional } from '@putnami/runtime';

const Schema = schema({
  tags: ArrayOf(String),                // string[]
  scores: ArrayOf(Int),                 // number[]
  metadata: MapOf(String, String),      // Record<string, string>
  optionalList: Optional(ArrayOf(Uuid)),// string[] | undefined
});
```

`MapOf(keyType, valueType)` accepts scalar key types (`String`, `Number`, `Boolean`, `Int`).

## Nested Objects

Schemas can contain nested object schemas:

```typescript
const AddressSchema = schema({
  street: String,
  city: String,
  zip: Pattern(/^\d{5}$/),
});

const UserSchema = schema({
  name: String,
  address: AddressSchema,
});
```

## Optional & Defaults

```typescript
import { Optional, Default } from '@putnami/runtime';

const ConfigSchema = schema({
  host: Default(String, 'localhost'),  // string (defaults to 'localhost')
  port: Default(Number, 3000),         // number (defaults to 3000)
  debug: Optional(Boolean),            // boolean | undefined
});
```

`Default` makes the field optional in input but always present in the validated output.

## Environment & Secrets

For configuration schemas, bind values to environment variables or async resolvers:

```typescript
import { Env, Resolve, Sensitive } from '@putnami/runtime';

const DbConfig = schema({
  host: Env('DB_HOST', String),
  port: Env('DB_PORT', Int),
  password: Sensitive(Env('DB_PASSWORD', String)),
  token: Resolve(() => secretManager.getSecret('db-token'), String),
});
```

| Helper | Purpose |
|--------|---------|
| `Env(varName, type)` | Read from environment variable |
| `Resolve(fn, type)` | Resolve asynchronously at bootstrap |
| `Sensitive(type)` | Redact value in validation errors |
| `Desc(text, type)` | Add description for documentation / OpenAPI |

## Runtime Validation

Use `validateSchema()` to validate data at runtime:

```typescript
import { validateSchema } from '@putnami/runtime';

const result = validateSchema(TaskSchema, requestBody);

if (result.errors.length > 0) {
  // result.errors: Array<{ field: string, message: string }>
  return new Response(JSON.stringify({ errors: result.errors }), { status: 400 });
}

// result.data is validated and typed
const task = result.data;
```

### Coercion

When validating URL parameters or query strings (which are always strings), enable coercion:

```typescript
const result = validateSchema(MySchema, queryParams, { coerce: true });
```

With `coerce: true`, string values are automatically converted to numbers and booleans where the schema expects them.

## Framework Integration

Schemas are used directly in endpoint definitions — validation happens automatically:

```typescript
import { endpoint } from '@putnami/application';
import { Int, OneOf, Optional } from '@putnami/runtime';

export const PUT = endpoint()
  .params({ id: String })
  .body({
    title: Optional(String),
    status: Optional(OneOf('todo', 'in_progress', 'done')),
    priority: Optional(Int),
  })
  .handle(async (ctx) => {
    const body = await ctx.body(); // fully typed and validated
    // body.status is 'todo' | 'in_progress' | 'done' | undefined
  });
```

The same schema types are used for:
- **Endpoint params, query, and body** — validated on each request
- **Configuration** (`useConfig`) — validated at startup
- **Domain models** — shared type definitions with runtime checks

## Support and compatibility

The schema vocabulary is owned by `@putnami/utils` and re-exported by
`@putnami/runtime`. Both are public, documented, maintained packages classified
`stable`. The declarative-schema specification and its accepted one-vocabulary
ADR live next to the `@putnami/utils` source. The promise is that one
declaration serves configuration, request and response validation, and generated
documentation; that coercion stays opt-in; and that the compiled validator used
on the request path agrees with the reference validator or delegates to it.

Before v1.0.0, minor `0.x` releases may still contain documented breaking
changes; strict compatibility across every pre-1.0 minor is not promised.
