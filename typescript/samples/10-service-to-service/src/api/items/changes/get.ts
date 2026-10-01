import { endpoint } from '@putnami/application';
import { NotFoundException, Optional, Stream } from '@putnami/runtime';
import { changes, itemChangeSchema } from '../../../change-log';
import { WATCH_SCOPE } from '../../../workload-identity';

// GET /items/changes — The catalog change feed: an SSE server stream that
// declares a cursor continuation.
//
// Every change carries the provider's position after it; `sseContinuation`
// names the output field that is and the query parameter that receives it, and
// `reconnect: true` is the consumer half, declared beside it. A generated
// client whose connection breaks reopens after the last change its caller
// received, on whichever instance answers; no consumer writes a reconnect loop,
// and no consumer reads the cursor. The route also speaks the negotiated SSE
// wire, so a consumer reads an explicit completion and never mistakes a cut
// connection for the end of the feed.
//
// The handler continues exclusively after the position it is given — the
// provider's obligation a continuation relies on — sends every change up to
// `until`, and completes. A position this log never issued is the declared
// not_found: the feed never restarts from the beginning on a cursor it does not
// recognize.
export const GET = endpoint()
  .query({
    // The position the feed continues after: the cursor of the last change the
    // consumer received. Absent, the feed starts at its first retained change.
    cursor: Optional(String),
    // The revision the feed completes after. The feed waits for it: a revision
    // that does not exist yet keeps the stream open until a creation appends
    // it, which is what lets a consumer observe a continuation.
    until: Number,
  })
  .returns(Stream(itemChangeSchema))
  .mayThrow('NotFound')
  .secure({ scopes: [WATCH_SCOPE] })
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'catalog-key', scopes: [WATCH_SCOPE] }] }] },
    transports: ['sse'],
    sseContinuation: { mode: 'cursor', cursor: { outputField: 'cursor', queryParameter: 'cursor' } },
    // No idle timeout on purpose: a change feed is quiet for as long as the
    // catalog is.
    resilience: { stream: { reconnect: true, maxFrameBytes: 16_384, maxBufferedMessages: 4 } },
  })
  .handle(async (ctx) => {
    const { cursor, until } = ctx.queryParams();
    let after = changes.position(cursor);
    if (after === undefined) {
      throw new NotFoundException('the position is not in the retained change log');
    }
    while (after < until) {
      // biome-ignore lint/performance/noAwaitInLoops: changes are sent in order
      const change = await changes.next(after, ctx.signal);
      // Abandoned or drained: the wire writes no terminal, and a consumer that
      // reconnects continues after the last change it received.
      if (!change) return;
      ctx.send(change);
      after = change.revision;
    }
  });
