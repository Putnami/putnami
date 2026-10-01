import { clearCache, endpoint, evictCache } from '@putnami/application';
import { Optional } from '@putnami/runtime';
import { metrics } from '../../metrics';

// DELETE /admin — Evict cache entries via the framework cache
//
// Demo-only: this admin route is intentionally unauthenticated so the
// sample runs standalone. In a real application, gate this destructive
// operation with `.secure()` (see the authentication how-to) so
// anonymous clients cannot flush your cache:
//
//   export const DELETE = endpoint().secure().body({ ... }).handle(async (ctx) => { ... });
export const DELETE = endpoint()
  .body({ pattern: Optional(String) })
  .handle(async (ctx) => {
    const body = await ctx.body();

    if (body?.pattern) {
      const evicted = await evictCache(body.pattern);
      metrics.recordEvictions(evicted);
      return { evicted, pattern: body.pattern };
    }

    await clearCache();
    metrics.reset();
    return { message: 'Cache cleared' };
  });
