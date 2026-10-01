import { endpoint } from '@putnami/application';
import { NotFoundException, Stream } from '@putnami/runtime';
import { QUOTE_TICKS, quotes, quoteTickSchema } from '../../../../quote-store';
import { WATCH_SCOPE } from '../../../../workload-identity';

// GET /quotes/[id]/ticks — a Connect-only server stream (provider endpoint).
//
// The same narrowing as GET /quotes/[id]: the stream travels as Connect
// envelope frames ended by one EndStreamResponse, and the generated method is
// the one an SSE or WebSocket stream would emit.
export const GET = endpoint()
  .params({ id: String })
  .returns(Stream(quoteTickSchema))
  .mayThrow('NotFound')
  .secure({ scopes: [WATCH_SCOPE] })
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'catalog-key', scopes: [WATCH_SCOPE] }] }] },
    transports: ['connect'],
  })
  .handle(async (ctx) => {
    if (!quotes.has(ctx.params.id)) {
      throw new NotFoundException('quote not found');
    }
    for (let sequence = 1; sequence <= QUOTE_TICKS; sequence += 1) {
      ctx.send({ id: ctx.params.id, sequence });
    }
  });
