import { Binary, endpoint, HttpResponse } from '@putnami/application';
import { BLOB_MAX_BYTES, BLOB_MEDIA_TYPE } from '../../../blob-store';

// POST /blobs/echo — hand the request octets straight back (provider endpoint).
//
// `Binary` declares what a JSON schema cannot: the body *is* octets, under a
// named media type, bounded. The endpoint pipeline applies both facts before
// this handler runs — 415 on an undeclared media type, 413 on a payload past
// the bound, taken from Content-Length before an octet is read — so the
// handler never validates, decodes or re-encodes anything.
export const POST = endpoint()
  .body(Binary({ mediaType: BLOB_MEDIA_TYPE, maxBytes: BLOB_MAX_BYTES }))
  .returns(Binary({ mediaType: BLOB_MEDIA_TYPE, maxBytes: BLOB_MAX_BYTES }))
  .handle(async (ctx) => {
    const body = await ctx.body();
    return new HttpResponse(body, { status: 200, headers: { 'Content-Type': BLOB_MEDIA_TYPE } });
  });
