import { endpoint } from '@putnami/application';
import { NotFoundException } from '@putnami/runtime';
import { missingQuoteSchema, quoteSchema, quotes } from '../../../quote-store';
import { WATCH_SCOPE } from '../../../workload-identity';

// GET /quotes/[id] — a Connect-only unary route (provider endpoint).
//
// `transports: ['connect']` narrows the derived list to the Connect wire, in the
// gRPC plugin's encoding order (protobuf, then JSON), so every generated client
// dispatches this call over Connect without a consumer branch. `.throws(404, …)`
// declares the detail a missing quote carries, which travels in the Connect
// error's framework detail rather than in its code.
export const GET = endpoint()
  .params({ id: String })
  .returns(quoteSchema)
  .mayThrow('NotFound')
  .throws(404, 'Missing quote', missingQuoteSchema)
  .secure({ scopes: [WATCH_SCOPE] })
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'catalog-key', scopes: [WATCH_SCOPE] }] }] },
    transports: ['connect'],
  })
  .handle((ctx) => {
    const quote = quotes.get(ctx.params.id);
    if (!quote) {
      throw new NotFoundException({ resource: 'quote', id: ctx.params.id });
    }
    return quote;
  });
