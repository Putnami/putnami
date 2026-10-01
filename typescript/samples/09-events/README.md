# Events

Event-driven architecture with `@putnami/events` — typed topics, reactive handlers, and on-demand queries.

## Two Patterns

This sample demonstrates two complementary ways to work with events:

### 1. On the Flow (Push)

Events are published and consumed reactively by handlers. When `POST /orders` creates an order, the `order.created` event triggers a pipeline of handlers in the background:

```
POST /orders → publish order.created
  → reserve-inventory.on.ts → publish inventory.reserved
    → send-notification.on.ts → publish notification.sent
```

Handlers are auto-discovered `*.on.ts` files in `src/events/`.

### 2. Fetch on Demand (Pull)

`GET /events` queries the event store at any time to see what happened. This is the pull-based counterpart — you decide when to check, rather than reacting in real time.

## Routes

| Method | Path | Description |
|--------|------|-------------|
| POST | `/orders` | Create an order (publishes events, triggers pipeline) |
| GET | `/events` | Query the event store (fetch on demand) |

## Run

```bash
putnami serve .
```

## Try It

**1. Create an order (triggers the event pipeline on the flow):**

```bash
curl -X POST http://localhost:3909/orders \
  -H "Content-Type: application/json" \
  -d '{"userId": "user-1", "items": [{"productId": "prod-1", "quantity": 2, "price": 29.99}]}'
```

Expected response (HTTP 201):

```json
{
  "orderId": "...",
  "total": 59.98,
  "message": "Order created — event pipeline triggered"
}
```

**2. Fetch events on demand:**

```bash
curl http://localhost:3909/events
```

Expected response — all events from the pipeline:

```json
{
  "events": [
    { "topic": "order.created", "payload": { "orderId": "...", "userId": "user-1", ... }, "timestamp": "..." },
    { "topic": "inventory.reserved", "payload": { "orderId": "...", "reservationId": "...", ... }, "timestamp": "..." },
    { "topic": "notification.sent", "payload": { "orderId": "...", "userId": "...", ... }, "timestamp": "..." }
  ],
  "total": 3
}
```

**3. Create more orders and fetch events again:**

```bash
curl -X POST http://localhost:3909/orders \
  -H "Content-Type: application/json" \
  -d '{"userId": "user-2", "items": [{"productId": "prod-2", "quantity": 1, "price": 9.99}]}'

curl http://localhost:3909/events
```

Each order triggers the full 3-event pipeline. The event store grows by 3 entries per order.

## Test

```bash
putnami test .
```

## What this sample proves

Topics are typed contracts shared by the publisher and the handler: `topics.ts`
declares the payload shape once and both sides import it, so a payload that does
not match fails the delivery instead of reaching the handler. Handlers live in
`src/events/*.on.ts` and are discovered by convention — nothing registers them
by name.

The pipeline is a chain, not a fan-out: `order.created` triggers the handler
that publishes `inventory.reserved`, which triggers the handler that publishes
`notification.sent`. The test posts one order and asserts all three events were
recorded, which is what makes the chain — not just the first hop — observable.

Delivery is at-least-once. This sample's handlers are idempotent by
construction (each records an event keyed by the order); turn on
`events({ simulateDuplicates: true })` to have the local broker redeliver ~2% of
messages and prove it.

Contract: [`@putnami/events`](../../framework/events/README.md) —
[event-messaging specification](../../framework/events/specs/event-messaging.json).
