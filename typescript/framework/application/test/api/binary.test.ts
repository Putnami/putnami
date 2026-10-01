import { specTest } from '@putnami/runtime/spectest';
import { describe, expect, it } from 'bun:test';
import { api } from '../../src/api/api.plugin';
import { Binary } from '../../src/api/route/binary';
import { endpoint } from '../../src/api/route/endpoint';
import { application } from '../../src/application';
import { HttpResponse } from '../../src/http/http-response';
import { http } from '../../src/http/http.plugin';
import { generateOpenApiSpec } from '../../src/openapi/openapi';

// Raw octet declarations, from the writing form to the published contract and
// back through the endpoint pipeline. Every assertion is on octets, never on
// text: a pipeline that decoded or re-encoded them would pass a text
// comparison and still corrupt the payload.

const MEDIA_TYPE = 'application/octet-stream';
const NON_UTF8 = new Uint8Array([0x00, 0xff, 0xfe, 0x80, 0x7f, 0x22, 0x5c, 0x0a]);

function echoRoute(maxBytes: number) {
  return endpoint()
    .body(Binary({ mediaType: MEDIA_TYPE, maxBytes }))
    .returns(Binary({ mediaType: MEDIA_TYPE, maxBytes }))
    .handle(async (ctx) => {
      const body = await ctx.body();
      return new HttpResponse(body, { status: 200, headers: { 'Content-Type': MEDIA_TYPE } });
    });
}

/** Start a real application serving one raw octet echo, and return its base URL. */
async function serveEcho(maxBytes: number): Promise<{ baseUrl: string; stop: () => Promise<void> }> {
  const reservation = Bun.serve({ port: 0, fetch: () => new Response(null, { status: 503 }) });
  const port = reservation.port;
  reservation.stop(true);
  const plugin = api({ autoScan: false });
  plugin.register('/echo', echoRoute(maxBytes), 'POST');
  const instance = application().use(http({ port })).use(plugin);
  await instance.start();
  return { baseUrl: `http://localhost:${port}`, stop: () => instance.stop() };
}

describe('raw octet payloads', () => {
  specTest(
    'declares its media type and its byte bound in the published contract',
    {
      feature: 'typescript/api-contracts',
      requirement: 'binary-payloads',
      check: 'a-raw-octet-payload-is-published-with-its-media-type-and-bound',
    },
    () => {
      const spec = generateOpenApiSpec(
        [
          {
            method: 'POST',
            path: '/echo',
            schemas: {
              bodyBinary: { mediaType: MEDIA_TYPE, maxBytes: 4096 },
              returnsBinary: { mediaType: MEDIA_TYPE, maxBytes: 4096 },
            },
          },
        ],
        { title: 'Blobs', version: '1.0.0' },
      );
      const operation = spec.paths['/echo'].post;
      expect(operation.requestBody?.content[MEDIA_TYPE]).toEqual({
        schema: { type: 'string', format: 'binary' },
        'x-putnami-max-bytes': 4096,
      });
      expect(operation.responses['200'].content?.[MEDIA_TYPE]).toEqual({
        schema: { type: 'string', format: 'binary' },
        'x-putnami-max-bytes': 4096,
      });
      // The two refusals a bounded body adds are declared, not implicit.
      expect(operation.responses['413']).toBeDefined();
      expect(operation.responses['415']).toBeDefined();
    },
  );

  specTest(
    'refuses a payload past the declared bound before it reads the body',
    {
      feature: 'typescript/api-contracts',
      requirement: 'binary-payloads',
      check: 'an-oversized-raw-octet-request-is-refused-before-the-body-is-read',
    },
    async () => {
      const server = await serveEcho(16);
      try {
        // The request announces more octets than the declaration allows and
        // sends none. A pipeline that read first would answer 200 with zero
        // octets; this one answers 413 without reading.
        const announced = await fetch(`${server.baseUrl}/echo`, {
          method: 'POST',
          headers: { 'Content-Type': MEDIA_TYPE, 'Content-Length': '1024' },
          body: new Uint8Array(1024),
        });
        expect(announced.status).toBe(413);

        const sent = await fetch(`${server.baseUrl}/echo`, {
          method: 'POST',
          headers: { 'Content-Type': MEDIA_TYPE },
          body: new Uint8Array(17),
        });
        expect(sent.status).toBe(413);
      } finally {
        await server.stop();
      }
    },
  );

  specTest(
    'refuses a request media type the endpoint never declared',
    {
      feature: 'typescript/api-contracts',
      requirement: 'binary-payloads',
      check: 'an-undeclared-request-media-type-is-refused',
    },
    async () => {
      const server = await serveEcho(64);
      try {
        const response = await fetch(`${server.baseUrl}/echo`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: '{}',
        });
        expect(response.status).toBe(415);
      } finally {
        await server.stop();
      }
    },
  );

  specTest(
    'carries every octet, including none and octets that are not text',
    {
      feature: 'typescript/api-contracts',
      requirement: 'binary-payloads',
      check: 'a-declared-raw-octet-body-reaches-the-handler-unchanged',
    },
    async () => {
      const server = await serveEcho(64);
      try {
        for (const payload of [new Uint8Array(0), NON_UTF8, new Uint8Array(64).fill(0x01)]) {
          const response = await fetch(`${server.baseUrl}/echo`, {
            method: 'POST',
            headers: { 'Content-Type': MEDIA_TYPE },
            body: payload,
          });
          expect(response.status).toBe(200);
          expect(response.headers.get('Content-Type')).toBe(MEDIA_TYPE);
          const echoed = new Uint8Array(await response.arrayBuffer());
          expect(Buffer.from(echoed).toString('hex')).toBe(Buffer.from(payload).toString('hex'));
        }
      } finally {
        await server.stop();
      }
    },
  );

  it('refuses a declaration that could not be published', () => {
    expect(() => Binary({ mediaType: 'application/json', maxBytes: 16 })).toThrow(/JSON media type/);
    expect(() => Binary({ mediaType: 'application/vnd.acme+json', maxBytes: 16 })).toThrow(/JSON media type/);
    expect(() => Binary({ mediaType: 'not a media type', maxBytes: 16 })).toThrow(/is not a media type/);
    expect(() => Binary({ maxBytes: 0 })).toThrow(/strictly positive/);
    expect(() => Binary({ maxBytes: 1.5 })).toThrow(/strictly positive/);
    expect(Binary({ maxBytes: 8 })).toEqual({
      __putnamiBinary: true,
      mediaType: 'application/octet-stream',
      maxBytes: 8,
    });
  });
});
