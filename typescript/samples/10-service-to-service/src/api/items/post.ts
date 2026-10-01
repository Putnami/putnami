import { endpoint } from '@putnami/application';
import { changes } from '../../change-log';
import { itemSchema } from '../../item-schema';
import { type Item, items } from '../../store';

/**
 * The header carrying the stable request identity of this operation. The
 * runtime mints one key per call and repeats it on every attempt, so a provider
 * can recognize a repeat rather than create a second item.
 */
export const IDEMPOTENCY_KEY_HEADER = 'X-Idempotency-Key';

// POST /items — Create an item (provider endpoint).
// `.body()` declares the request shape so the generated client takes a typed
// argument; `.returns()` reuses the shared item model for the response.
export const POST = endpoint()
  .body({ name: String, price: Number, stock: Number })
  .returns({ item: itemSchema })
  // REST, declared: the mounted gRPC plugin would otherwise put Connect first.
  //
  // The one operation that declares a request policy. Creating an item is not
  // safe to repeat blindly, so the provider states the identity header that
  // makes a repeat recognizable, the attempts it accepts, the statuses worth
  // repeating, the budget a call may not exceed, and the consecutive failures
  // after which a consumer must stop calling. A generated client reads all of
  // it from the contract; no consumer writes a retry loop.
  .client({
    transports: ['rest-json'],
    idempotency: { kind: 'idempotent', keyHeader: IDEMPOTENCY_KEY_HEADER },
    resilience: {
      timeoutMs: 2000,
      attemptTimeoutMs: 250,
      retry: { maxAttempts: 3, statuses: [503] },
      // Two consecutive failed calls stop the traffic, and the circuit stays
      // open for a minute: a consumer that keeps calling a provider that is
      // down turns one outage into two.
      circuit: { failureThreshold: 2, resetTimeoutMs: 60_000 },
    },
  })
  .handle(async (ctx) => {
    const body = await ctx.body();
    const item: Item = {
      id: crypto.randomUUID(),
      name: body.name,
      price: body.price,
      stock: body.stock,
    };
    items.set(item.id, item);
    // Every creation is a revision of the change feed: the one mutation this
    // provider serves is what moves the feed a consumer follows.
    changes.append(item.id, item.name);
    return { item };
  });
