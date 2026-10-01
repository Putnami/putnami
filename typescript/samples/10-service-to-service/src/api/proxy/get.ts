import { endpoint } from '@putnami/application';
import { ArrayOf } from '@putnami/runtime';
import { ItemsClient } from '../../../clients/ts/src';
import { itemSchema } from '../../item-schema';

// GET /proxy — Consumer endpoint that calls the provider via the GENERATED client
// (produced from this provider's own OpenAPI spec by `putnami clientgen`; see
// clientGenerator() in main.ts). The client arrives through DI: no base URL, no
// interceptor, no header is written here.
export const GET = endpoint()
  .inject({ itemsClient: ItemsClient })
  .returns({ message: String, tenant: String, items: ArrayOf(itemSchema) })
  // REST, declared: the mounted gRPC plugin would otherwise put Connect first.
  .client({ transports: ['rest-json'] })
  .handle(async (ctx) => ({
    message: 'Fetched items through the generated service binding',
    ...(await ctx.deps.itemsClient.getItems({
      query: { search: '', limit: 10 },
      headers: { 'x-catalog-tenant': 'sample-tenant' },
    })),
  }));
