import { endpoint } from '@putnami/application';
import { NotFoundException, Stream } from '@putnami/runtime';
import { stockDeltaSchema, stockTotalSchema } from '../../../../stock-schema';
import { items } from '../../../../store';
import { WATCH_SCOPE } from '../../../../workload-identity';

// GET /items/[id]/adjust — Client stream (provider endpoint).
//
// `.body(Stream(...))` with a unary `.returns(...)` declares a client stream:
// the consumer sends deltas, ends its own direction, and reads one declared
// result. There is no SSE alternative — SSE carries one direction — so the
// published transport list holds WebSocket alone and the generated clients open
// it without any branch in the consuming application.
export const GET = endpoint()
  .params({ id: String })
  .body(Stream(stockDeltaSchema))
  .returns(stockTotalSchema)
  .mayThrow('NotFound')
  .secure({ scopes: [WATCH_SCOPE] })
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'catalog-key', scopes: [WATCH_SCOPE] }] }] },
    // Declared, not defaulted: the generated clients read these same numbers
    // from the published contract, so the two ends agree on when a stream is
    // idle, how large a reassembled message may be, and how deep the inbound
    // queue runs before the provider applies backpressure.
    resilience: {
      stream: {
        handshakeTimeoutMs: 2000,
        idleTimeoutMs: 5000,
        heartbeatMs: 250,
        maxFrameBytes: 16_384,
        maxBufferedMessages: 4,
      },
    },
  })
  .handle(async (ctx) => {
    // Refused before the first message is read: a consumer never sends into a
    // conversation the provider has already declined.
    if (!items.has(ctx.params.id)) {
      throw new NotFoundException('item not found');
    }
    let applied = 0;
    let total = 0;
    for await (const delta of ctx.messages()) {
      applied += 1;
      total += delta.delta;
    }
    return { id: ctx.params.id, applied, total };
  });
