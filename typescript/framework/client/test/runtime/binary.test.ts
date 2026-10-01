import { specTest } from '@putnami/runtime/spectest';
import { describe, expect, it } from 'bun:test';
import { readBoundedBody } from '../../src/runtime/binary';
import { ClientRequestEncodingError } from '../../src/runtime/errors';

// The request-side half of a declared raw octet bound: an oversized source is
// refused, and it is refused without being read to the end.

describe('readBoundedBody', () => {
  specTest(
    'refuses an oversized stream one octet past the declared bound, without draining it',
    {
      feature: 'typescript/service-clients',
      requirement: 'binary-payloads',
      check: 'an-oversized-raw-octet-request-is-refused-before-the-source-is-drained',
    },
    async () => {
      let pulled = 0;
      const source = new ReadableStream<Uint8Array>({
        pull(controller) {
          pulled += 1;
          controller.enqueue(new Uint8Array(4));
        },
      });
      await expect(readBoundedBody(source, 8)).rejects.toBeInstanceOf(ClientRequestEncodingError);
      // Eight octets fit in two chunks; the third is the one that proves the
      // overflow. Anything beyond that is a source the bound drained.
      expect(pulled).toBeLessThanOrEqual(3);
    },
  );

  it('carries every declared source shape, and refuses an unbounded declaration', async () => {
    const payload = new Uint8Array([0x00, 0xff, 0x80]);
    expect(Buffer.from(await readBoundedBody(payload, 8)).toString('hex')).toBe('00ff80');
    expect(Buffer.from(await readBoundedBody(payload.buffer as ArrayBuffer, 8)).toString('hex')).toBe('00ff80');
    const streamed = await readBoundedBody(
      new ReadableStream<Uint8Array>({
        start(controller) {
          controller.enqueue(payload.slice(0, 1));
          controller.enqueue(payload.slice(1));
          controller.close();
        },
      }),
      8,
    );
    expect(Buffer.from(streamed).toString('hex')).toBe('00ff80');

    // Zero octets are octets; an absent source carries none.
    expect((await readBoundedBody(new Uint8Array(0), 8)).byteLength).toBe(0);
    expect((await readBoundedBody(undefined, 8)).byteLength).toBe(0);

    await expect(readBoundedBody(new Uint8Array(9), 8)).rejects.toBeInstanceOf(ClientRequestEncodingError);
    await expect(readBoundedBody(payload, 0)).rejects.toBeInstanceOf(ClientRequestEncodingError);
  });
});
