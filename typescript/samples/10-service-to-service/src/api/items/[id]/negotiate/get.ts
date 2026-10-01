import { endpoint } from '@putnami/application';
import { NotFoundException, Stream } from '@putnami/runtime';
import { stockDeltaSchema, stockTotalSchema } from '../../../../stock-schema';
import { items } from '../../../../store';
import { WATCH_SCOPE } from '../../../../workload-identity';

// GET /items/[id]/negotiate — Bidirectional stream (provider endpoint).
//
// Same declaration as the client stream with both directions streamed: every
// delta is answered with the running total, and the last value the provider
// sends is the declared result. Both directions run at once, so a consumer that
// half-closes still reads what is already in flight.
export const GET = endpoint()
  .params({ id: String })
  .body(Stream(stockDeltaSchema))
  .returns(Stream(stockTotalSchema))
  .mayThrow('NotFound')
  .secure({ scopes: [WATCH_SCOPE] })
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'catalog-key', scopes: [WATCH_SCOPE] }] }] },
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
    if (!items.has(ctx.params.id)) {
      throw new NotFoundException('item not found');
    }
    let applied = 0;
    let total = 0;
    for await (const delta of ctx.messages()) {
      applied += 1;
      total += delta.delta;
      ctx.send({ id: ctx.params.id, applied, total });
    }
    // The declared result of a bidirectional stream is the value the handler
    // returns; it reaches a generated client as the last message before
    // completion, exactly as the Go provider's ctx.Result does.
    return { id: ctx.params.id, applied, total };
  });
