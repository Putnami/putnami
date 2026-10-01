import { endpoint } from '@putnami/application';
import { NotFoundException } from '@putnami/runtime';
import { missingQuoteSchema, quoteSchema, quotes } from '../../../../quote-store';
import { WATCH_SCOPE } from '../../../../workload-identity';

// GET /quotes/[id]/snapshot — the same quote as GET /quotes/[id], declared
// Connect-only with JSON first (provider endpoint).
//
// The gRPC plugin serves protobuf before JSON. `connectEncodings` states this
// operation's own order, so every generated client dispatches it over Connect
// JSON — the encoding a browser's network panel can read — without a consumer
// branch. The two quote routes differ in that declaration and nothing else.
export const GET = endpoint()
  .params({ id: String })
  .returns(quoteSchema)
  .mayThrow('NotFound')
  .throws(404, 'Missing quote', missingQuoteSchema)
  .secure({ scopes: [WATCH_SCOPE] })
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'catalog-key', scopes: [WATCH_SCOPE] }] }] },
    transports: ['connect'],
    connectEncodings: ['json', 'proto'],
  })
  .handle((ctx) => {
    const quote = quotes.get(ctx.params.id);
    if (!quote) {
      throw new NotFoundException({ resource: 'quote', id: ctx.params.id });
    }
    return quote;
  });
