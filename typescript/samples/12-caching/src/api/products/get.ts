import { endpoint, getCacheStorage } from '@putnami/application';
import { metrics } from '../../metrics';
import { type Product, products } from '../../store';

const CACHE_KEY = 'products:all';
const TTL_MS = 30_000;

interface ProductList {
  products: Product[];
  total: number;
}

// GET /products — Cached product listing (framework cache, cache-aside)
export const GET = endpoint().handle(async () => {
  const storage = getCacheStorage();

  const cached = await storage.get<ProductList>(CACHE_KEY);
  if (cached) {
    metrics.recordHit();
    return { ...cached.value, source: 'cache' };
  }

  metrics.recordMiss();
  const result: ProductList = {
    products: [...products.values()],
    total: products.size,
  };

  await storage.set(CACHE_KEY, result, TTL_MS);

  return { ...result, source: 'computed' };
});
