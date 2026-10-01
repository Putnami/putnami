import { endpoint } from '@putnami/application';
import { products } from '../../../store';

// GET /http/products — HTTP-cached product listing
// Cache-Control tells browsers and CDNs to cache this response for 30 seconds.
// ETag enables conditional requests: if the data hasn't changed, the server
// responds with 304 Not Modified instead of resending the full body.
export const GET = endpoint()
  .cache({ maxAge: 30, etag: true })
  .handle(() => ({
    products: [...products.values()],
    total: products.size,
  }));
