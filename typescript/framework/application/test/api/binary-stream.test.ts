import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { BinaryStream, isConcreteBinaryContentType } from '../../src/api/route/binary';
import { endpoint } from '../../src/api/route/endpoint';
import { buildHttpContext } from '../../src/http/http-context.builder';
import { HttpResponse } from '../../src/http/http-response';
import { retainRawHttpStreamScope } from '../../src/http/raw-http-stream';
import { generateOpenApiSpec } from '../../src/openapi/openapi';
import { CompressionMiddleware } from '../../src/http/compression.middleware';

describe('raw HTTP streams', () => {
  it('bypasses whole-body compression for a declared raw HTTP stream', async () => {
    let pulls = 0;
    const source = new ReadableStream<Uint8Array>(
      {
        pull(c) {
          pulls++;
          c.enqueue(new Uint8Array([255]));
          c.close();
        },
      },
      { highWaterMark: 0 },
    );
    const route = endpoint()
      .returns(BinaryStream({ maxBytes: 4096 }))
      .handle(() => new HttpResponse(source, { headers: { 'Content-Type': 'application/json' } }));
    const context = buildHttpContext({
      req: new Request('http://localhost/stream', { headers: { 'Accept-Encoding': 'gzip' } }),
    });
    const result = await CompressionMiddleware({ threshold: 0 })(
      context,
      async () => (await route.handler(context)) as HttpResponse,
    );
    expect(pulls).toBe(0);
    expect(result?.getBodyInit()).toBe(source);
    await source.cancel();
  });
  specTest(
    'publishes a bounded unary raw HTTP stream',
    {
      feature: 'typescript/api-contracts',
      requirement: 'streamed-binary-payloads',
      check: 'raw-http-streams-publish-wildcard-content-with-a-bound',
    },
    () => {
      const def = endpoint()
        .body(BinaryStream({ maxBytes: 4096 }))
        .returns(BinaryStream({ maxBytes: 4096 }))
        .handle(async (ctx) => new HttpResponse(await ctx.body(), { headers: { 'Content-Type': 'image/png' } }));
      const spec = generateOpenApiSpec([{ method: 'POST', path: '/echo', schemas: def.schemas }], {
        title: 'Streams',
        version: '1',
      });
      const operation = spec.paths['/echo'].post;
      const content = {
        '*/*': {
          schema: { type: 'string', format: 'binary' },
          'x-putnami-max-bytes': 4096,
          'x-putnami-streamed': true,
        },
      };
      expect(operation.requestBody?.content).toEqual(content);
      expect(operation.responses['200'].content).toEqual(content);
      expect(operation.responses['413']).toBeDefined();
      expect(operation.responses['415']).toBeDefined();
      for (const maxBytes of [-1, 0]) {
        expect(() => BinaryStream({ maxBytes })).toThrow(/positive byte bound/);
      }
      expect(() => endpoint().returns({ ...BinaryStream({ maxBytes: 1 }), mediaType: 'image/png' })).toThrow(
        /wildcard/,
      );
    },
  );

  specTest(
    'hands the handler the unconsumed reader and preserves runtime content type',
    {
      feature: 'typescript/api-contracts',
      requirement: 'streamed-binary-payloads',
      check: 'raw-http-streams-preserve-readers-and-concrete-media-types',
    },
    async () => {
      const bytes = new Uint8Array([0, 255, 128]);
      let pulls = 0;
      const source = new ReadableStream<Uint8Array>(
        {
          pull(controller) {
            pulls++;
            controller.enqueue(bytes);
            controller.close();
          },
        },
        { highWaterMark: 0 },
      );
      const req = new Request('http://localhost/echo', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json; profile="opaque octets"' },
        body: source,
      });
      const context = buildHttpContext({ req });
      const route = endpoint()
        .body(BinaryStream({ maxBytes: 4096 }))
        .returns(BinaryStream({ maxBytes: 4096 }))
        .handle(async (ctx) => {
          expect(pulls).toBe(0);
          expect(await ctx.body()).not.toBe(req.body);
          return new HttpResponse(await ctx.body(), { headers: { 'Content-Type': ctx.headers.get('Content-Type')! } });
        });
      const response = (await route.handler(context)) as HttpResponse;
      expect(pulls).toBe(0);
      expect(response.getHeader('Content-Type')).toBe('application/json; profile="opaque octets"');
      expect(new Uint8Array(await response.get().arrayBuffer())).toEqual(bytes);
      for (const value of [
        '',
        '*/*',
        'image/*',
        'image/p*ng',
        'image',
        'image/png;broken',
        'image/png\r\nx: y',
        'image/png\n',
        'image/png; a=one; a=two',
      ]) {
        expect(isConcreteBinaryContentType(value), JSON.stringify(value)).toBe(false);
      }
      for (const value of [
        'image/png',
        'application/json; charset=utf-8',
        'application/vnd.any+json; p="a;b"',
        'image/png ; p = "a;b"',
        'image/png; note=";x=a"; x=b',
      ]) {
        expect(isConcreteBinaryContentType(value), JSON.stringify(value)).toBe(true);
      }
    },
  );

  it('refuses an invalid request or response content type and cancels its reader', async () => {
    let canceled = 0;
    const source = () =>
      new ReadableStream<Uint8Array>(
        {
          cancel() {
            canceled++;
          },
        },
        { highWaterMark: 0 },
      );
    const route = endpoint()
      .body(BinaryStream({ maxBytes: 4096 }))
      .handle(() => {
        throw new Error('must not reach handler');
      });
    for (const contentType of ['', '*/*', 'application/*', 'malformed']) {
      const req = new Request('http://localhost/echo', {
        method: 'POST',
        headers: { 'Content-Type': contentType },
        body: source(),
      });
      await expect(route.handler(buildHttpContext({ req }))).rejects.toThrow(/concrete request content type/);
    }
    expect(canceled).toBe(4);
    const output = endpoint()
      .returns(BinaryStream({ maxBytes: 4096 }))
      .handle(() => new HttpResponse(source()));
    await expect(output.handler(buildHttpContext({ req: new Request('http://localhost/output') }))).rejects.toThrow(
      /concrete Content-Type/,
    );
    expect(canceled).toBe(5);
  });

  it('enforces the streamed request bound incrementally', async () => {
    let canceled = 0;
    const source = new ReadableStream<Uint8Array>(
      {
        start(controller) {
          controller.enqueue(new Uint8Array([1, 2, 3]));
          controller.enqueue(new Uint8Array([4, 5]));
        },
        cancel() {
          canceled++;
        },
      },
      { highWaterMark: 0 },
    );
    const route = endpoint()
      .body(BinaryStream({ maxBytes: 4 }))
      .handle(async (ctx) => {
        await new Response(await ctx.body()).arrayBuffer();
        return new HttpResponse(undefined, { status: 204 });
      });
    const request = new Request('http://localhost/stream', {
      method: 'POST',
      headers: { 'Content-Type': 'application/octet-stream' },
      body: source,
    });
    await expect(route.handler(buildHttpContext({ req: request }))).rejects.toThrow(/declared bound/);
    expect(canceled).toBe(1);
  });

  specTest(
    'keeps the request scope until EOF, cancellation, failure or disconnect',
    {
      feature: 'typescript/api-contracts',
      requirement: 'streamed-binary-payloads',
      check: 'raw-http-streams-release-the-request-scope-at-body-completion',
    },
    async () => {
      for (const terminal of ['eof', 'cancel', 'error', 'abort']) {
        let closed = 0;
        let canceled = 0;
        const abort = new AbortController();
        const context = buildHttpContext({ req: new Request('http://localhost/stream', { signal: abort.signal }) });
        let pull = 0;
        const response = retainRawHttpStreamScope(
          new Response(
            new ReadableStream<Uint8Array>(
              {
                pull(controller) {
                  expect(closed).toBe(0);
                  if (pull++ === 0) controller.enqueue(new Uint8Array([255]));
                  else if (terminal === 'error') controller.error(new Error('source failed'));
                  else controller.close();
                },
                cancel() {
                  canceled++;
                },
              },
              { highWaterMark: 0 },
            ),
          ),
          context,
          () => {
            closed++;
          },
        );
        const reader = response.body?.getReader();
        expect(closed).toBe(0);
        expect((await reader.read()).value).toEqual(new Uint8Array([255]));
        expect(closed).toBe(0);
        if (terminal === 'cancel') await reader.cancel();
        else if (terminal === 'abort') {
          abort.abort();
          await reader.read();
        } else if (terminal === 'error') await expect(reader.read()).rejects.toThrow('source failed');
        else expect((await reader.read()).done).toBe(true);
        await Promise.resolve();
        expect(closed).toBe(1);
        if (terminal === 'cancel' || terminal === 'abort') expect(canceled).toBe(1);
      }
    },
  );
});
