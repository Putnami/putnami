import { Binary, endpoint, HttpResponse } from '@putnami/application';
import { NotFoundException } from '@putnami/runtime';
import { BLOB_MAX_BYTES, BLOB_MEDIA_TYPE, blobs } from '../../../blob-store';
import { WATCH_SCOPE } from '../../../workload-identity';

// GET /blobs/[id] — read one stored blob verbatim (provider endpoint).
//
// A declared binary success costs the endpoint nothing else: the path
// parameter still reaches the generated clients as a typed input, `.mayThrow()`
// still projects the typed error, and the declared credential is still
// injected by the binding — no consumer writes the header.
export const GET = endpoint()
  .params({ id: String })
  .returns(Binary({ mediaType: BLOB_MEDIA_TYPE, maxBytes: BLOB_MAX_BYTES }))
  .mayThrow('NotFound')
  .secure({ scopes: [WATCH_SCOPE] })
  .client({ security: { alternatives: [{ allOf: [{ profile: 'catalog-key', scopes: [WATCH_SCOPE] }] }] } })
  .handle((ctx) => {
    const blob = blobs.get(ctx.params.id);

    if (!blob) {
      throw new NotFoundException('blob not found');
    }

    return new HttpResponse(blob, { status: 200, headers: { 'Content-Type': BLOB_MEDIA_TYPE } });
  });
