import { ItemsClient } from '@example/go-items-client';
import { endpoint } from '@putnami/application';
import { Optional } from '@putnami/runtime';

// GET /consumed-items — the Go provider's catalog, read through the generated
// client. No URL appears here: the binding (`clients.services.items`) supplies
// it, and under `putnami compose` that is the provider's stable proxy URL.
export const GET = endpoint()
  .query({ search: Optional(String) })
  .inject({ items: ItemsClient })
  .handle(async (ctx) => {
    // The provider declares `search` required (a name substring); "e" matches
    // every item of the sample catalog.
    const search = ctx.queryParams().search || 'e';
    const list = await ctx.deps.items.getItems({ query: { search, limit: 10n } });
    return {
      items: (list.items ?? []).map((item) => ({
        id: item.id,
        name: item.name,
        // The contract types price as int64; JSON carries it as a string so no
        // value is rounded on the way out.
        price: item.price === undefined ? undefined : item.price.toString(),
      })),
    };
  });
