import { endpoint } from '@putnami/application';
import { NotFoundException, Stream } from '@putnami/runtime';
import { itemRevisionSchema } from '../../../../item-schema';
import { items } from '../../../../store';
import { WATCH_SCOPE } from '../../../../workload-identity';

/**
 * How many revisions one connection of the feed delivers before it completes.
 * Three is enough to observe a sequence and short enough that the four network
 * couples stay fast.
 */
const HISTORY_FEED_LENGTH = 3n;

// GET /items/[id]/history — Server stream over the published WebSocket wire.
//
// The same shape as /items/[id]/watch, declared the other way round:
// `transports: ['websocket', 'sse']` puts WebSocket first, and `resume: true`
// states this stream can be continued after a broken socket. The declaration is
// the only difference — the handler, the generated method and the consuming
// application are what they would be on SSE.
export const GET = endpoint()
  .params({ id: String })
  .returns(Stream(itemRevisionSchema))
  .mayThrow('NotFound')
  .secure({ scopes: [WATCH_SCOPE] })
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'catalog-key', scopes: [WATCH_SCOPE] }] }] },
    transports: ['websocket', 'sse'],
    resume: true,
    // The same bounds the stock conversations declare, plus the consumer half
    // of the resume agreement: this operation may ask the provider to continue
    // a broken stream. The published contract refuses one half without the other.
    resilience: {
      stream: {
        handshakeTimeoutMs: 2000,
        idleTimeoutMs: 5000,
        heartbeatMs: 250,
        maxFrameBytes: 16_384,
        maxBufferedMessages: 4,
        reconnect: true,
      },
    },
  })
  .handle(async (ctx) => {
    if (!items.has(ctx.params.id)) {
      throw new NotFoundException('item not found');
    }
    // A continuation is told the position it resumes after, so the revisions
    // carry on rather than starting over. A fresh stream continues after
    // nothing, which is revision 0.
    const from = ctx.resumeFrom ?? 0n;
    for (let offset = 1n; offset <= HISTORY_FEED_LENGTH; offset += 1n) {
      ctx.send({ id: ctx.params.id, revision: (from + offset).toString() });
    }
  });
