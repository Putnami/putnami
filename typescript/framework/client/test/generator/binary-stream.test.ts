import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSpec } from '../../src/generator/openapi-reader';
import { generateStrictTypeScriptClient } from '../../src/generator/ts/strict-ts-generator';

function document() {
  return {
    openapi: '3.0.3',
    info: { title: 'Streams', version: '1' },
    'x-putnami-client': { protocolVersion: 1, service: { id: 'streams', audience: 'streams' }, credentials: {} },
    paths: {
      '/stream': {
        post: {
          operationId: 'streamBody',
          'x-putnami-client': {
            stream: 'unary',
            transports: [{ protocol: 'rest-json', path: '/stream', encoding: 'json' }],
            security: { alternatives: [{ allOf: [] }] },
            errors: [],
            idempotency: { kind: 'idempotent' },
          },
          requestBody: {
            required: true,
            content: {
              '*/*': {
                schema: { type: 'string', format: 'binary' },
                'x-putnami-max-bytes': 4096,
                'x-putnami-streamed': true,
              },
            },
          },
          responses: {
            '200': {
              description: 'OK',
              content: {
                '*/*': {
                  schema: { type: 'string', format: 'binary' },
                  'x-putnami-max-bytes': 4096,
                  'x-putnami-streamed': true,
                },
              },
            },
          },
        },
      },
    },
    // biome-ignore lint/suspicious/noExplicitAny: mutable foreign-document boundary fixture
  } as any;
}

describe('raw HTTP stream generation', () => {
  specTest(
    'preserves the streamed contract and emits reader ownership',
    {
      feature: 'typescript/service-clients',
      requirement: 'streamed-binary-payloads',
      check: 'raw-http-streams-generate-explicit-content-types-and-readers',
    },
    () => {
      const spec = readOpenApiSpec(document());
      expect(spec.services[0].methods[0].request?.content).toEqual([
        { mediaType: '*/*', schema: { type: 'string', format: 'binary' }, maxBytes: 4096, streamed: true },
      ]);
      const source = generateStrictTypeScriptClient(spec, {
        packageName: '@test/stream-client',
        outputDir: 'clients/ts',
      })
        .map((file) => file.content)
        .join('\n');
      expect(source).toContain('contentType: string;');
      expect(source).toContain('readonly body: ReadableStream<Uint8Array>;');
      expect(source).toContain('body: input.body,');
      expect(source).toContain('requestMediaType: input.contentType,');
      expect(source).toContain('this.requestBinaryStream<200>');
      expect(source).not.toContain('await readBoundedBody');
      expect(source).toContain('maxRequestBytes: 4096');
      expect(source).toContain('maxPayloadBytes: 4096');
    },
  );

  it('refuses contradictory declarations and cache policies', () => {
    for (const value of [false, 1, 'true']) {
      const doc = document();
      doc.paths['/stream'].post.requestBody.content['*/*']['x-putnami-streamed'] = value;
      expect(() => readOpenApiSpec(doc)).toThrow(/streamed octets require/);
    }
    for (const bound of [0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
      const doc = document();
      doc.paths['/stream'].post.requestBody.content['*/*']['x-putnami-max-bytes'] = bound;
      expect(() => readOpenApiSpec(doc)).toThrow(/positive/);
      const ir = readOpenApiSpec(document());
      const request = ir.services[0].methods[0].request;
      if (!request) throw new Error('fixture requires a request contract');
      request.content[0].maxBytes = bound;
      expect(() =>
        generateStrictTypeScriptClient(ir, { packageName: '@test/streams', outputDir: 'clients/ts' }),
      ).toThrow(/positive byte bound/);
    }
    const unbounded = document();
    unbounded.paths['/stream'].post.requestBody.content['*/*']['x-putnami-max-bytes'] = undefined;
    expect(() => readOpenApiSpec(unbounded)).toThrow(/positive .*bound/);
    for (const mediaType of ['image/png', 'application/json', 'image/*']) {
      const doc = document();
      const content = doc.paths['/stream'].post.requestBody.content;
      doc.paths['/stream'].post.requestBody.content = { [mediaType]: content['*/*'] };
      expect(() => readOpenApiSpec(doc)).toThrow(/wildcard media type/);
    }
    const cached = document();
    cached.paths['/stream'].post['x-putnami-client'].resilience = { cache: { freshMs: 1000 } };
    expect(() => readOpenApiSpec(cached)).toThrow(/response cache/);
    const boundedWildcard = document();
    const media = boundedWildcard.paths['/stream'].post.requestBody.content['*/*'];
    media['x-putnami-streamed'] = undefined;
    media['x-putnami-max-bytes'] = 4;
    expect(() => readOpenApiSpec(boundedWildcard)).toThrow(/concrete media type/);
    const nonbinary = document();
    nonbinary.paths['/stream'].post.requestBody.content['*/*'].schema = { type: 'string' };
    expect(() => readOpenApiSpec(nonbinary)).toThrow(/raw octets only/);
  });
});
