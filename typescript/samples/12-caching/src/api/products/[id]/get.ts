import { endpoint, getCacheStorage } from '@putnami/application';
import { metrics } from '../../../metrics';
import { type Product, expensiveLookup } from '../../../store';

const TTL_MS = 60_000;

// GET /products/[id] — Cached single product (simulates expensive lookup)
export const GET = endpoint()
  .params({ id: String })
  .handle(async (ctx) => {
    const storage = getCacheStorage();
    const cacheKey = `products:${ctx.params.id}`;

    const cached = await storage.get<Product>(cacheKey);
    if (cached) {
      metrics.recordHit();
      return { product: cached.value, source: 'cache' };
    }

    metrics.recordMiss();
    const product = expensiveLookup(ctx.params.id);

    if (!product) {
      return new Response('Product not found', { status: 404 });
    }

    await storage.set(cacheKey, product, TTL_MS);

    return { product, source: 'computed' };
  });
