import type { Application } from '@putnami/application';
import { ClientRequestEncodingError, isClientFrameworkError } from '@putnami/client';
import { resetConfigLoader } from '@putnami/runtime';
import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { BlobsClient } from '../clients/ts/src';
import { BLOB_MAX_BYTES, BLOB_MEDIA_TYPE } from '../src/blob-store';
import { app as createApp } from '../src/main';
import { CATALOG_API_KEY } from '../src/workload-identity';

// The TS→TS raw octet cell: the TypeScript provider declares a bounded binary
// echo and a bounded binary read, and the generated TypeScript client calls
// both over a real loopback socket. Every assertion is on octets, never on
// text.

/** Neither valid UTF-8 nor valid JSON: a text or JSON hop would corrupt it. */
const NON_UTF8 = new Uint8Array([0x00, 0xff, 0xfe, 0x80, 0x7f, 0x22, 0x5c, 0x0a]);

describe('service-to-service raw octet payloads', () => {
  let app: Application;
  const originalConfig = process.env.CONFIG_DATA;

  beforeAll(async () => {
    const reservation = Bun.serve({ port: 0, fetch: () => new Response(null, { status: 503 }) });
    const port = reservation.port;
    reservation.stop(true);
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'service-to-service-sample',
        services: {
          'catalog.items': {
            url: `http://localhost:${port}`,
            allowInsecure: true,
            credentials: { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } },
          },
        },
      },
    });
    resetConfigLoader();
    app = createApp({ port });
    await app.start();
  });

  afterAll(async () => {
    await app.stop();
    if (originalConfig === undefined) delete process.env.CONFIG_DATA;
    else process.env.CONFIG_DATA = originalConfig;
    resetConfigLoader();
  });

  it('carries every octet through the declared echo', async () => {
    const client = app.context.get(BlobsClient);
    const payloads: Record<string, Uint8Array> = {
      empty: new Uint8Array(0),
      'non-utf8': NON_UTF8,
      'json-like': new TextEncoder().encode('{"not":"a document"}'),
      'at-bound': new Uint8Array(BLOB_MAX_BYTES).fill(0x7f),
    };
    for (const [name, payload] of Object.entries(payloads)) {
      const echoed = await client.postBlobsEcho({ body: payload });
      expect(`${name}:${Buffer.from(echoed.body).toString('hex')}`).toBe(
        `${name}:${Buffer.from(payload).toString('hex')}`,
      );
      expect(echoed.status).toBe(200);
      expect(echoed.contentType).toBe(BLOB_MEDIA_TYPE);
    }
  });

  it('accepts a stream source and refuses one past the declared bound', async () => {
    const client = app.context.get(BlobsClient);

    const streamed = await client.postBlobsEcho({
      body: new ReadableStream<Uint8Array>({
        start(controller) {
          controller.enqueue(NON_UTF8.slice(0, 4));
          controller.enqueue(NON_UTF8.slice(4));
          controller.close();
        },
      }),
    });
    expect(Buffer.from(streamed.body).toString('hex')).toBe(Buffer.from(NON_UTF8).toString('hex'));

    // The emitted bound fires before a socket exists, and the source is not
    // drained: the stream records how much of it was pulled.
    let pulled = 0;
    const oversized = new ReadableStream<Uint8Array>({
      pull(controller) {
        pulled += 1;
        controller.enqueue(new Uint8Array(1024).fill(0x5a));
      },
    });
    await expect(client.postBlobsEcho({ body: oversized })).rejects.toBeInstanceOf(ClientRequestEncodingError);
    expect(pulled).toBeLessThanOrEqual(BLOB_MAX_BYTES / 1024 + 1);
  });

  it('composes a path parameter and the declared credential with a raw octet response', async () => {
    const client = app.context.get(BlobsClient);

    const blob = await client.getBlobs_id({ path: { id: '1' } });
    expect(Buffer.from(blob.body).toString('hex')).toBe(Buffer.from(NON_UTF8).toString('hex'));
    expect(blob.contentType).toBe(BLOB_MEDIA_TYPE);

    // Zero octets are octets: an empty stored payload round-trips as an empty
    // payload, not as a missing body.
    const empty = await client.getBlobs_id({ path: { id: '2' } });
    expect(empty.body.byteLength).toBe(0);
  });

  it('still declares its typed error on a binary endpoint', async () => {
    const client = app.context.get(BlobsClient);
    const failure = await client.getBlobs_id({ path: { id: 'absent' } }).catch((error: unknown) => error);
    expect(
      isClientFrameworkError(failure, {
        service: 'catalog.items',
        method: 'getBlobs_id',
        status: 404,
        code: 'not_found',
      }),
    ).toBe(true);
  });

  it('declares the two refusals a bounded octet body adds, and a client reads them typed', async () => {
    const client = app.context.get(BlobsClient);
    const baseUrl = JSON.parse(process.env.CONFIG_DATA ?? '{}').clients.services['catalog.items'].url as string;

    // A foreign caller past the bound is refused with the declared status, and
    // the generated client reads that refusal as the declared error rather
    // than as an unattributed remote failure.
    const oversized = await fetch(`${baseUrl}/blobs/echo`, {
      method: 'POST',
      headers: { 'Content-Type': BLOB_MEDIA_TYPE },
      body: new Uint8Array(BLOB_MAX_BYTES + 1),
    });
    expect(oversized.status).toBe(413);

    const wrongType = await fetch(`${baseUrl}/blobs/echo`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    expect(wrongType.status).toBe(415);

    // The contract the client embeds declares both, so the codes are readable
    // by identity and not inferred from a status alone.
    const declared = (
      client as unknown as {
        operationContracts: Record<string, { errors: { status: number; code: string; retryable?: boolean }[] }>;
      }
    ).operationContracts.postBlobsEcho.errors;
    expect(declared).toContainEqual({ status: 413, code: 'http.payload_too_large', retryable: false });
    expect(declared).toContainEqual({ status: 415, code: 'http.unsupported_media_type', retryable: false });
  });
});
