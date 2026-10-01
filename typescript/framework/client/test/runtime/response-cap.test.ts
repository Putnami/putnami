import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { ClientResponseSizeError } from '../../src/runtime/errors';
import { readBodyBytesCapped } from '../../src/runtime/response-cap';

/**
 * The transports' size cap is a MEMORY guarantee, not merely a verdict: the
 * point is that a hostile provider cannot make the caller buffer an arbitrarily
 * large body before it is rejected. The status-level tests in
 * http-transport.test.ts assert only the rejection, which still holds when the
 * read is unbounded — so the read itself is measured here.
 *
 * countingBody hands back a stream that reports how many bytes it was actually
 * asked to produce, one chunk at a time, up to an advertised hostile size.
 */
function countingBody(totalBytes: number, chunkBytes: number): { response: Response; read: () => number } {
  let produced = 0;
  const stream = new ReadableStream<Uint8Array>({
    pull(controller) {
      if (produced >= totalBytes) {
        controller.close();
        return;
      }
      const size = Math.min(chunkBytes, totalBytes - produced);
      produced += size;
      controller.enqueue(new Uint8Array(size));
    },
  });
  return { response: new Response(stream), read: () => produced };
}

describe('response cap bounds the read, not only the verdict', () => {
  specTest(
    'stops pulling a hostile body once it passes the cap',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'the-cap-bounds-the-bytes-read-not-only-the-verdict',
    },
    async () => {
      const maxResponseSize = 1024;
      const hostileSize = 8 * 1024 * 1024;
      const chunk = 4096;
      const { response, read } = countingBody(hostileSize, chunk);

      await expect(readBodyBytesCapped(response, maxResponseSize)).rejects.toBeInstanceOf(ClientResponseSizeError);

      // One chunk of slack: the read may overshoot by at most the chunk that
      // carried it past the cap. Anything beyond that means the whole body was
      // buffered and the cap is only a post-hoc verdict.
      expect(read()).toBeLessThanOrEqual(maxResponseSize + chunk);
      expect(read()).toBeLessThan(hostileSize);
    },
  );

  specTest(
    'a body at the cap is still read in full',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'a-body-at-the-cap-is-read-in-full',
    },
    async () => {
      const maxResponseSize = 4096;
      const { response, read } = countingBody(maxResponseSize, 512);

      const bytes = await readBodyBytesCapped(response, maxResponseSize);

      expect(bytes.length).toBe(maxResponseSize);
      expect(read()).toBe(maxResponseSize);
    },
  );
});
