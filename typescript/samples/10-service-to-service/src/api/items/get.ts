import { endpoint } from '@putnami/application';
import { ArrayOf } from '@putnami/runtime';
import { itemSchema } from '../../item-schema';
import { items } from '../../store';

// GET /items — List all items (provider endpoint).
// `.query()` and `.headers()` declare the request shape, so the generated
// clients carry typed inputs and no consumer builds a URL or a header.
// `.returns()` declares the response shape so the generated client is typed.
export const GET = endpoint()
  .query({ search: String, limit: Number })
  .headers({ 'x-catalog-tenant': String })
  .returns({ tenant: String, items: ArrayOf(itemSchema) })
  // REST, declared: the mounted gRPC plugin would otherwise put Connect first.
  .client({ transports: ['rest-json'] })
  .handle((ctx) => {
    const { search, limit } = ctx.queryParams();
    const tenant = ctx.headerParams()['x-catalog-tenant'];
    const matched = [...items.values()].filter((item) => item.name.includes(search)).slice(0, limit);
    return { tenant, items: matched };
  });
