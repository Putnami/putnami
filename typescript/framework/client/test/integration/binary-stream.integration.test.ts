import { describe, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { api, application, BinaryStream, endpoint, http, HttpResponse, openapi } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';
import type { ServiceBinding } from '../../src/runtime/service-binding';
import type { BaseClient, ClientConfig } from '../../src/runtime/base-client';
import type { BinarySource, StreamedBinaryPayload } from '../../src/runtime/binary';

describe('raw HTTP stream generated consumer', () => {
  specTest(
    'loads and binds the generated client to a real provider',
    {
      feature: 'typescript/service-clients',
      requirement: 'streamed-binary-payloads',
      check: 'a-generated-bound-client-streams-through-a-real-provider',
    },
    async () => {
      const providerHttp = http({ port: 0 });
      const providerApi = api({
        autoScan: false,
        client: { service: { id: 'raw-streams', audience: 'raw-streams' }, credentials: {} },
      });
      providerApi.register(
        '/echo',
        endpoint()
          .body(BinaryStream({ maxBytes: 4 * 1024 * 1024 }))
          .returns(BinaryStream({ maxBytes: 4 * 1024 * 1024 }))
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'idempotent' } })
          .handle(
            async (ctx) =>
              new HttpResponse(await ctx.body(), { headers: { 'Content-Type': ctx.headers.get('Content-Type')! } }),
          ),
        'POST',
      );
      const providerOpenApi = openapi({ title: 'Raw streams', version: '1.0.0' });
      const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
      const consumer = application();
      const directory = mkdtempSync(join(tmpdir(), 'putnami-raw-stream-client-'));
      await provider.start();
      try {
        const document = providerOpenApi.spec();
        const ir = readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' });
        const method = ir.services[0].methods[0];
        expect(method.client?.stream).toBe('unary');
        const files = generateTypeScriptClient(ir, { packageName: '@test/raw-streams' });
        const clientEntry = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
        for (const file of files) {
          const destination = join(directory, file.path);
          mkdirSync(dirname(destination), { recursive: true });
          writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${clientEntry}'`));
        }
        const generated = await import(join(directory, 'src', 'index.ts'));
        const Client = generated[ir.services[0].className] as new (config: ClientConfig) => BaseClient;
        const register = generated[`register${ir.services[0].className}`] as (
          target: typeof consumer,
          binding: ServiceBinding,
        ) => typeof consumer;
        register(consumer, {
          url: `http://localhost:${providerHttp.getServer()?.port}`,
          clientId: 'raw-consumer',
          allowInsecure: true,
        });
        await consumer.start();
        const client = consumer.context.get(Client);
        const echo = (
          client as unknown as Record<
            string,
            (input: { body: BinarySource; contentType: string }) => Promise<StreamedBinaryPayload>
          >
        )[method.name];
        for (const [contentType, bytes] of [
          ['image/png; name="opaque"', new Uint8Array([0, 255, 128, 10])],
          ['application/json; profile="raw octets"', new Uint8Array([255, 0, 254])],
          ['application/octet-stream', new Uint8Array(0)],
        ] as const) {
          const source = new ReadableStream<Uint8Array>({
            start(c) {
              if (bytes.length) c.enqueue(bytes);
              c.close();
            },
          });
          const result = await echo.call(client, { body: source, contentType });
          expect(result.status).toBe(200);
          expect(result.contentType).toBe(contentType);
          expect(new Uint8Array(await new Response(result.body).arrayBuffer())).toEqual(bytes);
        }
      } finally {
        await consumer.stop();
        await provider.stop();
        rmSync(directory, { recursive: true, force: true });
      }
    },
  );
});
