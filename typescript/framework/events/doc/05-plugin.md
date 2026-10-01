# Events Plugin

The events plugin integrates with the Putnami application lifecycle and supports file-based handler auto-discovery.

## Setup

```typescript
import { application } from '@putnami/application';
import { events } from '@putnami/events';

const app = application()
  .use(events()); // Auto-discovers events/*.on.ts handlers

await app.start();
```

## Handler Auto-Discovery

The plugin automatically discovers handler files with the `.on.ts` suffix in a sibling `events/` directory relative to the calling file.

### File Convention

```
src/
├── main.ts                        ← events() called here
└── events/
    ├── user-created.on.ts         → export default handler(UserCreated).handle(...)
    ├── order-placed.on.ts         → export default handler(OrderPlaced).handle(...)
    └── payment/
        └── failed.on.ts           → export default handler(PaymentFailed).handle(...)
```

- **Suffix**: `*.on.ts` — identifies a file as an event handler
- **Exports**: default export or named exports are scanned for `HandlerDefinition` objects
- **Nesting**: subdirectories are supported for organization (the path has no semantic meaning)
- **Build-time**: discovery happens during `generate()`, producing a static loader file in `.gen/`

### Single Handler (default export)

```typescript
// events/user-created.on.ts
import { handler } from '@putnami/events';
import { UserCreated } from '../shared/topics';

export default handler(UserCreated)
  .handle(async (msg) => {
    console.log(`Welcome ${msg.payload.name}!`);
  });
```

### Multiple Handlers (named exports)

```typescript
// events/order.on.ts
import { handler } from '@putnami/events';
import { OrderPlaced, OrderShipped } from '../shared/topics';

export const onPlaced = handler(OrderPlaced)
  .handle(async (msg) => { ... });

export const onShipped = handler(OrderShipped)
  .handle(async (msg) => { ... });
```

### Disabling Auto-Discovery

```typescript
import { events } from '@putnami/events';
import { onUserCreated } from './handlers/on-user-created';

// Explicit handler registration (no scanning)
const app = application()
  .use(events({ autoScan: false, handlers: [onUserCreated] }));
```

## Configuration

```typescript
events({
  transport: undefined,      // Explicit transport instance (overrides endpoint/local)
  eventServer: undefined,    // Managed publisher config + code-owned tokenSource
  transports: undefined,     // Named transports for routing
  routes: [],                // Channel/name routes for named transports
  defaultTransport: undefined, // Fallback named transport
  handlers: [...],           // Explicitly registered handler definitions
  outboxes: [...],           // Declared transactional outboxes (runtime-inert)
  scanFolder: 'events',      // Directory name to scan (default: 'events')
  scanPath: undefined,        // Explicit scan path (overrides scanFolder)
  autoScan: true,            // Enable file-based discovery (default: true)
  endpoint: undefined,       // Event service endpoint (overrides EVENTS_ENDPOINT env var)
  token: undefined,          // Bearer token for the endpoint (overrides EVENTS_TOKEN env var)
  port: 4222,               // Local server port (when no endpoint configured)
  drainTimeout: 10_000,      // Shutdown drain timeout in ms
  preloadedModule: undefined, // Generated handler module for bundled builds
})
```

## Transport Selection

The plugin selects exactly one transport during `warmup()`:

| Priority | Source | Use Case |
|----------|--------|----------|
| 1 | `events({ transport })` | Single explicit production broker such as Redis Streams or Google Pub/Sub. |
| 2 | `events.transport: eventserver` plus `events.eventServer` | Managed publish-only Event Server transport. |
| 3 | `events({ transports, routes, defaultTransport })` | Multiple named transports with plugin-level routing. |
| 4 | `endpoint` / `EVENTS_ENDPOINT` | Connect to the local-broker HTTP protocol. |
| 5 | local broker/server | Local development and tests. |

`transport` cannot be combined with `transports`, `routes`, or
`defaultTransport`; use one model per plugin instance.

Use explicit transports for production worker deployments:

```typescript
import { events, redisStreamTransport } from '@putnami/events';

application().use(events({
  transport: redisStreamTransport({
    url: process.env.REDIS_URL,
    keyPrefix: 'putnami:events',
    consumerName: process.env.HOSTNAME,
  }),
}));
```

See `08-production-transports.md` for Redis Streams and Google Pub/Sub setup,
stable handler groups, retry/DLQ behavior, and option references.

The plugin contributes the deploy-time `events` config block. A managed
document can select `transport: eventserver` and provide the contract version,
origin, audience, protocol, and local identity hints. Credential acquisition is
always a code-owned `tokenSource`; provider SDK and metadata access stay outside
`@putnami/events`. Explicit transport objects remain authoritative for existing
direct adapters.

### Multiple Named Transports

Use named transports when one application publishes or subscribes through more
than one backend.

```typescript
import { events, redisStreamTransport } from '@putnami/events';
import { googlePubSubTransport } from '@putnami/events/google';

application().use(events({
  transports: {
    stream: redisStreamTransport({ url: process.env.REDIS_URL }),
    analytics: googlePubSubTransport({ client: pubsub }),
  },
  routes: [
    { channel: 'analytics', transport: 'analytics' },
    { match: 'stream.*', transport: 'stream' },
  ],
  defaultTransport: 'stream',
}));
```

Routing precedence is fixed:

| Step | Rule |
|------|------|
| 1 | If the topic has `channel`, use the first `{ channel, transport }` route matching that channel. |
| 2 | Otherwise use the first `{ match, transport }` route matching the topic name. String matches support `*` wildcards. |
| 3 | Otherwise use `defaultTransport`. |
| 4 | If no transport resolves, publish/subscribe fails. |

`channel` is logical metadata on the topic. It is not automatically a transport
name; it only has an effect when a route maps that channel to a named transport.

Use an array for explicit fanout:

```typescript
routes: [
  { channel: 'audit', transport: ['stream', 'analytics'] },
]
```

Fanout is not atomic. One backend can accept the publish while another fails,
and subscribing the same handler through multiple backends can produce duplicate
deliveries. Use it for migrations, mirroring, or audit pipelines where handlers
are idempotent.

## Environment Variables

| Variable | Description |
|----------|-------------|
| `EVENTS_ENDPOINT` | Event service URL. When set, connects to the HTTP event transport instead of starting a local broker. The `endpoint` config option takes precedence over this variable. |
| `EVENTS_TOKEN` | Bearer token for `EVENTS_ENDPOINT`. The `token` config option takes precedence over this variable. |

## Lifecycle

| Phase | Action |
|-------|--------|
| **generate** | Scans `*.on.ts` files and produces a static loader in `.gen/`; emits infra requirements (see below) |
| **warmup** | Loads discovered handlers, selects transport (local or cloud), subscribes handlers |
| **start** | Starts the local events server (if no endpoint) and wires per-handler DI scope creation when a container context is available |
| **stop** | Drains in-flight messages, closes connections |

For the first push deployment, `events.push.enabled: false` still mounts the
receiver route but returns `503` before token verification or handler dispatch.
Switching it to `true` requires the normal issuer, audience, and service-account
allowlist and preserves the existing enabled receiver behavior.

## Infrastructure Requirements

During `generate()`, the plugin derives which event topics the project uses and
writes a per-project infra-requirements scratch fragment to
`<project>/.gen/infra/events.json`. The TypeScript generator syncs that fragment
into committed `<project>/infra/requirements.json`; `putnami build` then merges
committed requirements into the workload's ephemeral `.gen/requirements.json`.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-infra.json",
  "protocolVersion": 2,
  "events": {
    "publishes": ["notification.sent"],
    "subscribes": ["inventory.reserved"]
  }
}
```

- **subscribes** — the topics of every discovered handler.
- **publishes** — the topics passed to `getPublisher()` at the top level of the
  scanned `*.on.ts` handler modules.

Topic names are deduplicated and sorted, so the generated JSON is
byte-deterministic for a given input. When the project neither publishes nor
subscribes to any topic no file is written, and any scratch fragment from a
previous build is removed so a deleted handler never leaves a stale contribution
behind. A topic name that
violates the canonical resource pattern `^[a-z0-9][a-z0-9_./-]{0,63}$` fails the
build with a clear diagnostic.

> **Scope:** only `getPublisher()` calls in scanned `*.on.ts` modules are
> detected. A publisher declared elsewhere (for example in an HTTP route file)
> is not captured automatically — add the publisher to a handler module so the
> generator records it in `<project>/infra/requirements.json`.

## Local Development

When no `EVENTS_ENDPOINT` is configured, the plugin automatically:

1. Checks if a local events server is already running on the port
2. If not, starts one (HTTP server wrapping the in-memory broker)
3. Subscribes all registered handlers (discovered + explicit)

Multiple services can share the same local events server for cross-service event exchange.

The in-memory broker can deliberately redeliver ~2% of handled messages to
surface non-idempotent handlers. It is opt-in — `events({ simulateDuplicates: true })` —
and off by default so the local development loop stays deterministic. It applies
to the local broker only and has no effect once an endpoint or an explicit
transport is configured.

## Dynamic Registration

```typescript
const plugin = events();
plugin.register(onUserCreated);
plugin.register(onOrderPlaced);
plugin.registerOutbox(IdentityOutbox);

const app = application().use(plugin);
```

`registerOutbox()` records a [transactional outbox declaration](./04-publishing.md#transactional-outboxes).
It is build-time metadata only — no transport, transaction, relay, or retry
behavior changes.

## Delegating Plugins

A plugin that composes the events plugin privately — a relay or controller that
holds `events()` as a field instead of mounting it on the module tree — is
invisible to design-graph discovery, because only plugins reachable from the
module tree receive a builder. Implement `DesignDelegate` so its native
contributions are forwarded under the same owning module, instead of restating
them in a wrapper:

```typescript
import type { DesignDelegate, Plugin } from '@putnami/application';
import { events, type EventsPlugin } from '@putnami/events';

class RelayController implements Plugin, DesignDelegate {
  private readonly events: EventsPlugin = events({ outboxes: [IdentityOutbox] });

  designDelegates() {
    return [this.events];
  }
}
```

The seam is read only while the disposable design graph is built. It changes no
plugin lifecycle, hook ordering, or dependency injection.

## Several events() plugins

A workload can use several `events()` plugins, each scanning its own handler folder:

```typescript
import { resolve } from 'node:path';

application()
  .use(events({ scanPath: resolve(import.meta.dir, 'orders') }))
  .use(events({ scanPath: resolve(import.meta.dir, 'billing') }));
```

Each scanning plugin gets its own generated handler loader, so the packaged workload subscribes
the handlers of every folder, and a plugin with `autoScan: false` loads only the handlers you give
it. The build writes one infra requirements file with the topics of every folder. Subscribing one
topic from two plugins with different `delivery` values fails the build.
