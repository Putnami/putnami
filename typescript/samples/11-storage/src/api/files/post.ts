import { endpoint, json } from '@putnami/application';
import type { HttpRequestContext } from '@putnami/application';
import { storage } from '@putnami/storage';
import '../../store';

// POST /files — Upload a file
//
// Demo-only: this endpoint is intentionally unauthenticated so the
// sample runs without an identity provider. In a real application,
// gate uploads with `.secure()` (see the authentication how-to) so
// anonymous clients cannot fill your bucket:
//
//   export const POST = endpoint().secure().handle(async (ctx) => { ... });
export const POST = endpoint().handle(async (ctx) => {
  const reqCtx = ctx as unknown as HttpRequestContext;
  const contentType = reqCtx.headers.get('content-type') || 'application/octet-stream';
  const fileName = reqCtx.headers.get('x-file-name') || `upload-${Date.now()}`;

  const data = Buffer.from(await reqCtx.req.arrayBuffer());
  const client = await storage('files');

  try {
    const result = await client.put(fileName, data, { contentType });

    return json({ key: result.key, size: result.size, contentType }, { status: 201 });
  } catch (error) {
    if (error instanceof Error && error.name === 'StorageValidationError') {
      return json({ error: error.message }, { status: 400 });
    }
    throw error;
  }
});
