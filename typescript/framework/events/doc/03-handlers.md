# Handlers

Handlers subscribe to topics and process messages. They use a fluent builder API similar to `endpoint()`.

## Basic Handler

```typescript
import { handler } from '@putnami/events';
import { UserCreated } from '../shared/topics/user';

export default handler(UserCreated)
  .handle(async (msg) => {
    console.log(msg.payload.email); // typed as string
    // Returning normally = ack
  });
```

## Handler Options

```typescript
handler(OrderPlaced)
  .options({
    group: 'billing.reserve-credit', // stable external transport subscription id
    distribution: 'broadcast',  // 'competing' (default) | 'broadcast'
    maxRetries: 5,              // default: 10
    maxBackoff: 30_000,         // default: 60_000 (60s)
    timeout: 10_000,            // default: 30_000 (30s)
    concurrency: 4,             // default: 0 (unlimited)
    queueLimit: 100,            // default: 0 (unlimited)
    overflow: 'throw',          // 'throw' (default) | 'drop'
    dlq: true,                  // default: true
    ack: 'auto',                // 'auto' (default) | 'manual'
  })
  .handle(async (msg) => { ... });
```

### Options Reference

| Option | Default | Description |
|--------|---------|-------------|
| `group` | `undefined` | Stable subscription/group name for external transports such as Redis Streams or Google Pub/Sub. |
| `distribution` | `'competing'` | `'competing'`: round-robin across instances. `'broadcast'`: all instances receive every message. |
| `maxRetries` | `10` | Maximum delivery attempts before sending to DLQ — total attempts, not extra retries (`1` = no retry, `N` = up to N attempts). |
| `maxBackoff` | `60_000` | Maximum delay between retries (ms). Caps exponential growth. |
| `timeout` | `30_000` | Handler execution timeout (ms). `0` disables. |
| `concurrency` | `0` | Maximum concurrent invocations for this handler. `0` means unlimited. |
| `queueLimit` | `0` | Maximum queued deliveries waiting for a concurrency slot. `0` means unlimited. |
| `overflow` | `'throw'` | Behavior when `queueLimit` is reached: throw from publish or drop the message with a metric. |
| `dlq` | `true` | Send failed messages to `{topic}.dlq` after max retries. When `false`, failed messages are logged and discarded. |
| `ack` | `'auto'` | `'auto'`: return = ack, throw = nack. `'manual'`: call `msg.ack()` / `msg.nack()`. |

Retries always use exponential backoff (1s, 2s, 4s, ... capped at `maxBackoff`) with 0-25% random jitter.

### Groups And External Transports

`group` is optional for the local broker, but should be set for production
brokers. Redis Streams maps it to a consumer group. Google Pub/Sub maps it to a
subscription name by default.

Use the same group for replicas of the same worker:

```typescript
handler(OrderPlaced)
  .options({ group: 'billing.reserve-credit', distribution: 'competing' })
  .handle(reserveCredit);
```

Use different groups for different logical subscribers on the same topic:

```typescript
export const reserveCredit = handler(OrderPlaced)
  .options({ group: 'billing.reserve-credit' })
  .handle(reserveCreditForOrder);

export const sendConfirmation = handler(OrderPlaced)
  .options({ group: 'email.order-confirmation' })
  .handle(sendOrderConfirmation);
```

Avoid generated/default group names in production because handler ordering,
file layout, and deployment shape can change over time.

## Handler DI (`.inject()`)

Inject DI dependencies directly into event handlers:

```typescript
import { handler } from '@putnami/events';
import { UserCreated } from '../shared/topics/user';
import { UserRepository, WelcomeMailer } from '../services';

export default handler(UserCreated)
  .inject({ users: UserRepository, mailer: WelcomeMailer })
  .handle(async ({ users, mailer }, msg) => {
    const user = await users.findById(msg.payload.id);
    if (user) {
      await mailer.send(user.email);
    }
  });
```

When running through `Application + events()`, each handler invocation gets its own DI scope automatically.

## Attribute Filtering

Filter messages server-side by attributes:

```typescript
handler(OrderPlaced)
  .filter({ attributes: { region: 'eu' } })
  .handle(async (msg) => {
    // Only receives messages with attributes.region === 'eu'
  });
```

## Message Object

The handler receives a `Message<T>` with:

```typescript
interface Message<T> {
  id: string;              // Unique message ID
  topic: string;           // Topic name
  payload: T;              // Typed payload
  key?: string;             // Optional routing / partition key
  dedupeKey?: string;       // Optional idempotency key
  topicVersion?: string;    // Optional topic contract version
  timestamp: Date;         // Publish time
  attributes: Record<string, string>;
  attempt: number;         // Current delivery attempt (1-based)
  traceId?: string;        // Distributed tracing correlation
  signal: AbortSignal;     // Aborted when handler timeout expires
  ack(): void;             // Manual ack (manual mode only)
  nack(reason?: string): void; // Manual nack
}
```

Use `msg.signal` to cancel downstream work when a timeout expires:

```typescript
handler(UserCreated)
  .options({ timeout: 5_000 })
  .handle(async (msg) => {
    await fetch('https://example.internal/process', { signal: msg.signal });
  });
```

## Acknowledgement Modes

### Auto (default)

```typescript
handler(UserCreated).handle(async (msg) => {
  await processUser(msg.payload);
  // Returning = ack
  // Throwing = nack → retry
});
```

### Manual

```typescript
handler(UserCreated)
  .options({ ack: 'manual' })
  .handle(async (msg) => {
    try {
      await longProcess(msg.payload);
      msg.ack();
    } catch (err) {
      msg.nack('Processing failed');
    }
  });
```

## Retry Backoff Schedule

With default settings (10 retries, 60s max):

| Attempt | Delay |
|---------|-------|
| 1 | ~1s |
| 2 | ~2s |
| 3 | ~4s |
| 4 | ~8s |
| 5 | ~16s |
| 6 | ~32s |
| 7-10 | ~60s (capped) |

Delays include 0-25% random jitter to prevent thundering herd.
