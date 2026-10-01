import { endpoint } from '@putnami/application';
import { expensiveLookup } from '../../../../store';

// GET /http/products/:id — HTTP-cached single product
// Private caching (not shared by CDNs) with ETag validation.
// The browser caches for 60 seconds, then revalidates using If-None-Match.
// If the product data hasn't changed, the server returns 304 — saving bandwidth
// and skipping JSON serialisation on the client.
export const GET = endpoint()
  .params({ id: String })
  .cache({ privateMaxAge: 60, etag: true })
  .handle((ctx) => {
    const product = expensiveLookup(ctx.params.id);

    if (!product) {
      return new Response('Product not found', { status: 404 });
    }

    return { product };
  });
