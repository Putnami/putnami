import { endpoint } from '@putnami/application';
import { Optional } from '@putnami/runtime';
import { storage } from '@putnami/storage';
import '../../store';

// GET /files — List uploaded files with optional prefix filter
export const GET = endpoint()
  .query({ prefix: Optional(String) })
  .handle(async (ctx) => {
    const query = ctx.queryParams();
    const client = await storage('files');
    const result = await client.list({ prefix: query.prefix });

    return {
      files: result.objects.map((obj) => ({
        key: obj.key,
        size: obj.size,
        contentType: obj.contentType,
        lastModified: obj.lastModified,
      })),
      total: result.objects.length,
    };
  });
