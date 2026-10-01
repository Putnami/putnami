# Topics

A topic is a typed contract that defines the name and payload schema for an event. Topics are pure data — they carry no transport or connection logic and can be shared across services.

## Defining a Topic

```typescript
import { topic, Uuid, Email } from '@putnami/events';

export const UserCreated = topic('user.created', {
  id: Uuid,
  email: Email,
  name: String,
});
```

The `topic()` function returns a `TopicDefinition<S>` where `S` is the schema type. Both publishers and subscribers import this definition to get full type safety.

## Topic Options

Topics can carry contract metadata without depending on a physical broker:

```typescript
export const PageViewed = topic(
  'analytics.page_view',
  {
    id: Uuid,
    path: String,
  },
  {
    version: 'v1',
    channel: 'analytics',
    metadata: { owner: 'growth' },
  },
);
```

| Option | Description |
|--------|-------------|
| `version` | Contract version copied to published envelopes as `topicVersion`. |
| `channel` | Logical channel used by plugin transport routing. It is not a Redis/Pub/Sub transport name. |
| `metadata` | Static string metadata for tooling and documentation. |

Keep channels domain-oriented, for example `analytics`, `audit`, or `realtime`.
The deployment config decides which physical transport handles each channel.

## Topic Naming

Use dot-separated names to create a natural hierarchy:

```typescript
topic('user.created', { ... })
topic('user.updated', { ... })
topic('order.placed', { ... })
topic('order.shipped', { ... })
```

### DLQ Topics

Dead-letter topics follow the convention `{topic}.dlq`:

```
user.created      → user.created.dlq
order.placed      → order.placed.dlq
```

## Schema Integration

Topics use the same schema system as `@putnami/runtime`. All schema primitives and constraints are available:

```typescript
import { topic, Uuid, Email, Int, Optional, ArrayOf } from '@putnami/events';

export const OrderPlaced = topic('order.placed', {
  orderId: Uuid,
  userId: Uuid,
  items: ArrayOf({ productId: Uuid, quantity: Int, price: Number }),
  discount: Optional(Number),
  total: Number,
});
```

## Type Inference

The payload type is automatically inferred from the schema:

```typescript
import type { InferTopicPayload } from '@putnami/events';

type UserPayload = InferTopicPayload<typeof UserCreated>;
// { id: string; email: string; name: string }
```
