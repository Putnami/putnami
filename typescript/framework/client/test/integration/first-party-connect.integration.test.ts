import { afterEach, describe, expect, test } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import {
  api,
  application,
  Constrained,
  endpoint,
  grpc,
  http,
  Int,
  IntWidth,
  openapi,
  proto,
  Stream,
} from '@putnami/application';
import { NotFoundException, OneOf, resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';
import { ClientServiceConfigError } from '../../src/runtime/errors';
import type { StreamObserver } from '../../src/runtime/stream.type';
import { SuccessBody } from '../../src/runtime/success-body';

/**
 * TypeScript provider → generated TypeScript consumer, over Connect, on a real
 * port.
 *
 * The declaration is written once. The contract the consumer reads is the
 * document the provider publishes, the client is the one the emitter produces
 * from it, and the calls below traverse a real socket in three shapes: unary
 * JSON, unary binary protobuf, and a server stream of envelope frames.
 *
 * Whether those bytes are the Connect protocol's is settled elsewhere, against
 * the specification corpus. What this file settles is that the two halves reach
 * each other without a consumer choosing a transport, a codec or a deadline.
 */

const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
const temporaryDirectories: string[] = [];
const originalConfig = process.env.CONFIG_DATA;

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
  if (originalConfig === undefined) delete process.env.CONFIG_DATA;
  else process.env.CONFIG_DATA = originalConfig;
  resetConfigLoader();
});

/** Collect a stream to its terminal, under a controlled deadline. */
function collectStream<T>(observer: StreamObserver<T>, timeoutMs = 5000): Promise<T[]> {
  return new Promise((resolve, reject) => {
    const messages: T[] = [];
    const timer = setTimeout(() => reject(new Error('stream did not terminate')), timeoutMs);
    observer.onMessage((message) => messages.push(message));
    observer.onError((error) => {
      clearTimeout(timer);
      reject(error);
    });
    observer.onComplete(() => {
      clearTimeout(timer);
      resolve(messages);
    });
  });
}

interface GeneratedModule {
  [name: string]: unknown;
}

/** The exact bytes the provider writes for one Connect JSON call, read outside the client. */
async function providerJsonAnswer(port: number, connectPath: string, id: string): Promise<string> {
  const response = await fetch(`http://localhost:${port}${connectPath}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'Connect-Protocol-Version': '1' },
    body: JSON.stringify({ params: { id } }),
  });
  if (!response.ok) throw new Error(`provider answered ${response.status}`);
  return response.text();
}

/** Emit the client the published contract implies, and import it. */
async function emitClient(document: unknown, packageName: string): Promise<GeneratedModule> {
  const ir = readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' });
  const files = generateTypeScriptClient(ir, { packageName });
  const directory = mkdtempSync(join(tmpdir(), 'putnami-connect-client-'));
  temporaryDirectories.push(directory);
  for (const file of files) {
    const destination = join(directory, file.path);
    mkdirSync(dirname(destination), { recursive: true });
    writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
  }
  return (await import(join(directory, 'src', 'index.ts'))) as GeneratedModule;
}

describe('first-party Connect: a TypeScript provider answers a generated TypeScript consumer', () => {
  specTest(
    'unary JSON, unary proto and a server stream all reach a real provider on a real port',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-transport',
      check: 'a-generated-client-reaches-a-real-provider-over-connect-in-three-shapes',
    },
    async () => {
      const providerHttp = http({ port: 0 });
      const providerApi = api({
        autoScan: false,
        client: {
          service: { id: 'catalog.gadgets', audience: 'api://gadgets' },
          credentials: {},
        },
      });
      const anonymous = { alternatives: [{ allOf: [] }] } as const;

      providerApi.register(
        '/gadgets/[id]',
        endpoint()
          .params({ id: String })
          .returns({ id: String, name: String, state: OneOf('active', 'retired') })
          .client({ security: anonymous, idempotency: { kind: 'safe' } })
          .handle((context) => ({ id: context.params.id, name: 'connected', state: 'retired' as const })),
        'GET',
      );

      providerApi.register(
        '/gadgets',
        endpoint()
          .body({ name: String, quantity: Int, batch: Constrained(Int, IntWidth('int32')) })
          .returns({ id: String, quantity: Int, batch: Constrained(Int, IntWidth('int32')) })
          .client({ security: anonymous, idempotency: { kind: 'idempotent' } })
          .handle(async (context) => {
            const body = await context.body();
            return { id: `g-${body.name}`, quantity: body.quantity, batch: body.batch };
          }),
        'POST',
      );

      providerApi.register(
        '/gadgets/watch',
        endpoint()
          .returns(Stream({ id: String, seq: Int }))
          .client({ security: anonymous, idempotency: { kind: 'safe' } })
          .handle(async (context) => {
            context.send({ id: 'g-1', seq: 1 });
            context.send({ id: 'g-2', seq: 2 });
          }),
        'GET',
      );

      providerApi.register(
        '/gadgets/missing/[id]',
        endpoint()
          .params({ id: String })
          .returns({ id: String })
          .mayThrow('NotFound')
          .throws(404, 'Missing gadget', { code: String, reason: String })
          .client({ security: anonymous, idempotency: { kind: 'safe' } })
          .handle((context) => {
            throw new NotFoundException({ code: 'NotFound', reason: `safe-${context.params.id}` });
          }),
        'GET',
      );

      const providerOpenApi = openapi({ title: 'Gadgets', version: '1.0.0' });
      const provider = application()
        .use(providerHttp)
        .use(providerApi)
        .use(proto({ packageName: 'catalog.gadgets.v1' }))
        .use(grpc())
        .use(providerOpenApi);

      await provider.start();
      try {
        const server = providerHttp.getServer();
        if (!server) throw new Error('provider HTTP server did not start');
        const document = providerOpenApi.spec();
        if (!document) throw new Error('provider OpenAPI contract was not emitted');

        // The provider advertises Connect first, in both codecs, because the
        // gRPC plugin is installed. Nothing below names a transport.
        const getOperation = (path: string, method = 'get') =>
          // biome-ignore lint/suspicious/noExplicitAny: the published document is read as data here
          ((document as any).paths[path][method]['x-putnami-client'] ?? {}) as {
            transports: { protocol: string; encoding: string; protobufMethod?: string }[];
          };
        expect(getOperation('/gadgets/{id}').transports.map((entry) => `${entry.protocol}:${entry.encoding}`)).toEqual([
          'connect:proto',
          'connect:json',
          'rest-json:json',
        ]);
        expect(getOperation('/gadgets/watch').transports.map((entry) => entry.protocol)).toEqual([
          'connect',
          'connect',
          'sse',
          'websocket',
        ]);
        // biome-ignore lint/suspicious/noExplicitAny: the published document is read as data here
        expect((document as any)['x-putnami-client'].protobuf.package).toBe('catalog.gadgets.v1');

        const generated = await emitClient(document, '@test/gadgets-connect-client');
        const ir = readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' });
        const methods = ir.services.flatMap((service) => service.methods);
        const byPath = (path: string) => methods.find((method) => method.path === path);
        const getGadget = byPath('/gadgets/{id}');
        const createGadget = byPath('/gadgets');
        const watchGadgets = byPath('/gadgets/watch');
        const missingGadget = byPath('/gadgets/missing/{id}');
        if (!getGadget || !createGadget || !watchGadgets || !missingGadget) {
          throw new Error('provider operations did not reach the generated IR');
        }

        const ClientClass = generated['GadgetsClient'] as new (config: {
          baseUrl: string;
          transport: 'http';
        }) => Record<string, (...args: never[]) => unknown>;
        const client = new ClientClass({ baseUrl: `http://localhost:${server.port}`, transport: 'http' });

        // 1. Unary — the first declared transport is `connect:proto`, so this
        //    call travels as binary protobuf without the caller saying so.
        const read = client[getGadget.name] as (input: { path: { id: string } }) => Promise<{
          id: string;
          name: string;
          state: string;
        }>;
        expect(await read.call(client, { path: { id: 'g-64' } })).toEqual({
          id: 'g-64',
          name: 'connected',
          state: 'retired',
        });
        // A success body sink is refused on this transport before anything is
        // sent: the runtime re-encodes the reply from protobuf, so no bytes
        // it could hand over are the provider's.
        const sink = new SuccessBody();
        const readWithSink = read as unknown as (
          input: { path: { id: string } },
          options: { successBody: SuccessBody },
        ) => Promise<unknown>;
        await expect(readWithSink.call(client, { path: { id: 'g-64' } }, { successBody: sink })).rejects.toBeInstanceOf(
          ClientServiceConfigError,
        );
        expect(sink.bytes).toBeUndefined();

        // 2. Unary with a body, and an integer wide enough to prove the width
        //    survived the codec.
        // A declared `int` is 64 bits, so the generated client types it `bigint`
        // and the binary codec carries it as a 64-bit varint; a declared `int32`
        // stays a `number`. The value stays inside the JavaScript safe range
        // because a TypeScript *provider* validates integers as `number` — see
        // the limits in the report. A negative `int32` exercises the
        // sign-extension the wire requires.
        const create = client[createGadget.name] as (input: {
          body: { name: string; quantity: bigint; batch: number };
        }) => Promise<{ id: string; quantity: bigint; batch: number }>;
        const created = await create.call(client, { body: { name: 'bolt', quantity: 4200000000n, batch: -7 } });
        expect(created).toEqual({ id: 'g-bolt', quantity: 4200000000n, batch: -7 });

        // 3. Server stream — envelope frames terminated by one EndStreamResponse.
        const watch = client[watchGadgets.name] as () => StreamObserver<{ id: string; seq: bigint }>;
        expect(await collectStream(watch.call(client))).toEqual([
          { id: 'g-1', seq: 1n },
          { id: 'g-2', seq: 2n },
        ]);

        // 4. A declared error keeps its stable code and its declared details,
        //    carried over Connect in the framework detail rather than in `code`.
        const missing = client[missingGadget.name] as (input: { path: { id: string } }) => Promise<unknown>;
        const failure = await missing.call(client, { path: { id: 'gadget' } }).catch((error: unknown) => error);
        expect(failure).toMatchObject({
          service: 'catalog.gadgets',
          method: missingGadget.operationId,
          status: 404,
          code: 'not_found',
          details: { code: 'NotFound', reason: 'safe-gadget' },
        });
        const guard = generated[
          Object.keys(generated).find((name) => name.endsWith('NotFoundError') && name.includes('Missing')) ?? ''
        ] as (error: unknown) => boolean;
        expect(guard(failure)).toBe(true);

        client.dispose?.();
      } finally {
        await provider.stop();
      }
    },
    30_000,
  );

  test('the same client answers over JSON when the provider declares only that codec', async () => {
    const providerHttp = http({ port: 0 });
    const providerApi = api({
      autoScan: false,
      client: { service: { id: 'catalog.gadgets', audience: 'api://gadgets' }, credentials: {} },
    });
    providerApi.register(
      '/gadgets/[id]',
      endpoint()
        .params({ id: String })
        .returns({ id: String, name: String })
        .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
        .handle((context) => ({ id: context.params.id, name: 'json-only' })),
      'GET',
    );
    const providerOpenApi = openapi({ title: 'Gadgets', version: '1.0.0' });
    const provider = application()
      .use(providerHttp)
      .use(providerApi)
      .use(proto({ packageName: 'catalog.gadgets.v1' }))
      // Only the JSON media types are accepted, so the document advertises only
      // `connect:json` and the emitted client can carry nothing else.
      .use(grpc({ acceptContentTypes: ['application/json', 'application/connect+json'] }))
      .use(providerOpenApi);

    await provider.start();
    try {
      const server = providerHttp.getServer();
      const document = providerOpenApi.spec();
      if (!server || !document) throw new Error('provider did not start');
      // biome-ignore lint/suspicious/noExplicitAny: the published document is read as data here
      const transports = (document as any).paths['/gadgets/{id}'].get['x-putnami-client'].transports as {
        protocol: string;
        encoding: string;
        path: string;
      }[];
      expect(transports.map((entry) => `${entry.protocol}:${entry.encoding}`)).toEqual([
        'connect:json',
        'rest-json:json',
      ]);

      const generated = await emitClient(document, '@test/gadgets-json-connect-client');
      const ir = readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' });
      const method = ir.services.flatMap((service) => service.methods)[0];
      const ClientClass = generated['GadgetsClient'] as new (config: {
        baseUrl: string;
        transport: 'http';
      }) => Record<string, (...args: never[]) => unknown>;
      const client = new ClientClass({ baseUrl: `http://localhost:${server.port}`, transport: 'http' });
      const read = client[method.name] as (
        input: { path: { id: string } },
        options?: { successBody?: SuccessBody },
      ) => Promise<{ id: string; name: string }>;
      expect(await read.call(client, { path: { id: 'g-7' } })).toEqual({ id: 'g-7', name: 'json-only' });
      // The Connect JSON codec carries the declared document itself, so the
      // generated call hands over the provider's bytes.
      const sink = new SuccessBody();
      const answer = await read.call(client, { path: { id: 'g-8' } }, { successBody: sink });
      expect(answer).toEqual({ id: 'g-8', name: 'json-only' });
      const delivered = new TextDecoder().decode(sink.bytes);
      expect(JSON.parse(delivered)).toEqual({ id: 'g-8', name: 'json-only' });
      const connectPath = transports.find((entry) => entry.protocol === 'connect')?.path ?? '';
      expect(delivered).toBe(await providerJsonAnswer(server.port, connectPath, 'g-8'));
      client.dispose?.();
    } finally {
      await provider.stop();
    }
  }, 30_000);
});
