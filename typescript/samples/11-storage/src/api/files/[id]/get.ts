import { HttpResponse, endpoint, json } from '@putnami/application';
import { storage } from '@putnami/storage';
import '../../../store';

// GET /files/[id] — Download a file
export const GET = endpoint()
  .params({ id: String })
  .handle(async (ctx) => {
    const client = await storage('files');
    const file = await client.get(ctx.params.id);

    if (!file) {
      return json({ error: 'File not found' }, { status: 404 });
    }

    return new HttpResponse(file.body, {
      headers: {
        'Content-Type': file.contentType || 'application/octet-stream',
        'Content-Disposition': `attachment; filename="${file.key}"`,
        'Content-Length': String(file.size),
      },
    });
  });
