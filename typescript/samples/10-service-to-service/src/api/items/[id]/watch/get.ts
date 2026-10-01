import { endpoint } from '@putnami/application';
import { NotFoundException, Stream } from '@putnami/runtime';
import { itemSchema } from '../../../../item-schema';
import { items } from '../../../../store';
import { WATCH_SCOPE } from '../../../../workload-identity';

/** Gap between two updates in follow mode: short enough to observe, long enough not to spin. */
const WATCH_FOLLOW_INTERVAL_MS = 20;

// GET /items/[id]/watch — Server stream (provider endpoint).
//
// `.returns(Stream(...))` declares a server stream, so the generated clients
// expose a typed stream handle instead of a one-shot call. `.secure({ verify })`
// runs before the response head, which is what makes a refused credential a
// non-2xx status rather than a 200 followed by a terminal event.
export const GET = endpoint()
  .params({ id: String })
  // Declared required: the TypeScript emitter renders an optional query
  // parameter as `input.query?["follow"]`, which does not parse.
  .query({ follow: Boolean })
  .returns(Stream(itemSchema))
  .mayThrow('NotFound')
  .secure({ scopes: [WATCH_SCOPE] })
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'catalog-key', scopes: [WATCH_SCOPE] }] }] },
    // SSE then WebSocket, declared: the mounted gRPC plugin would otherwise put
    // Connect first.
    transports: ['sse', 'websocket'],
  })
  .handle(async (ctx) => {
    const item = items.get(ctx.params.id);
    if (!item) {
      throw new NotFoundException('item not found');
    }
    // Spread with the optional property named: the declared stream message
    // states `discontinuedAt` explicitly, absent or not.
    const message = { ...item, discontinuedAt: item.discontinuedAt };
    ctx.send(message);
    if (String(ctx.queryParams().follow) !== 'true') {
      return;
    }
    // Follow mode keeps the stream open on purpose: without it a consumer could
    // only ever prove the one-shot path, never cancellation or shutdown.
    while (!ctx.signal.aborted) {
      await new Promise((resolve) => setTimeout(resolve, WATCH_FOLLOW_INTERVAL_MS));
      if (ctx.signal.aborted) return;
      ctx.send(message);
    }
  });
