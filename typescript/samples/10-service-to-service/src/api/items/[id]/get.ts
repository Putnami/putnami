import { endpoint, HttpResponse } from '@putnami/application';
import { NotFoundException } from '@putnami/runtime';
import { itemSchema } from '../../../item-schema';
import { items } from '../../../store';

/**
 * The one id this provider fails on with a status it never declared, so a
 * consumer can be checked against an unknown remote error.
 */
export const UNDECLARED_FAILURE_ID = 'boom';

// GET /items/[id] — Get a single item (provider endpoint).
// `.mayThrow('NotFound')` declares the failure, so the generated clients narrow
// it to a typed error carrying the stable `not_found` code instead of an opaque
// non-2xx string.
export const GET = endpoint()
  .params({ id: String })
  .returns({ item: itemSchema })
  .mayThrow('NotFound')
  // REST, declared: the mounted gRPC plugin would otherwise put Connect first.
  .client({ transports: ['rest-json'] })
  .handle(async (ctx) => {
    // A failure the operation never declared. A consumer must read it as the
    // unknown remote error it is — never as one of the declared errors, and
    // never with the provider's own prose.
    if (ctx.params.id === UNDECLARED_FAILURE_ID) {
      return HttpResponse.json({ error: 'catalog replica lag 42s on shard 7' }, { status: 503 });
    }
    const item = items.get(ctx.params.id);

    if (!item) {
      throw new NotFoundException('item not found');
    }

    return { item };
  });
