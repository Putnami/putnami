import { endpoint, json } from '@putnami/application';
import { storage } from '@putnami/storage';
import '../../../store';

// DELETE /files/[id] — Delete a file
//
// Demo-only: unauthenticated so the sample runs standalone. Real
// applications must gate destructive operations with `.secure()` and
// an ownership check.
export const DELETE = endpoint()
  .params({ id: String })
  .handle(async (ctx) => {
    const client = await storage('files');
    const exists = await client.exists(ctx.params.id);

    if (!exists) {
      return json({ error: 'File not found' }, { status: 404 });
    }

    await client.delete(ctx.params.id);

    return { deleted: true, key: ctx.params.id };
  });
