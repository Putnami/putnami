import { afterEach, describe, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import {
  apiKeyStrategy,
  application,
  api,
  authenticate,
  ByteStream,
  endpoint,
  http,
  openapi,
  Stream,
} from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';
import type { ByteStream as ClientByteStream, FrameStream } from '../../src/runtime/provider-ws-transport';

const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
const API_KEY = 'gateway-secret';
const temporaryDirectories: string[] = [];
const running: (() => Promise<void>)[] = [];
const originalConfig = process.env.CONFIG_DATA;

afterEach(async () => {
  for (const stop of running.splice(0)) await stop();
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
  if (originalConfig === undefined) delete process.env.CONFIG_DATA;
  else process.env.CONFIG_DATA = originalConfig;
  resetConfigLoader();
});

const EventIn = { type: String, topic: String };
const EventOut = { type: String, data: String };

/**
 * A real TypeScript provider on a real port, declaring the two provider-owned
 * wires the way a db-gateway and an event-server would, plus a byte stream its
 * own security chain refuses.
 */
async function startGateway(): Promise<{ baseUrl: string; document: string }> {
  const providerHttp = http({ port: 0 });
  providerHttp.prepend(authenticate({ anyOf: [apiKeyStrategy({ keys: [API_KEY], header: 'X-Gateway-Key' })] }));
  const security = { security: { alternatives: [{ allOf: [{ profile: 'gateway-key' }] }] } } as const;
  const providerApi = api({
    autoScan: false,
    client: {
      service: { id: 'gateway', audience: 'api://gateway' },
      credentials: { 'gateway-key': { kind: 'api-key', header: 'X-Gateway-Key' } },
      defaults: { resilience: { stream: { maxFrameBytes: 64 } } },
    },
  });
  providerApi.register(
    '/v1/databases/connect',
    endpoint()
      .query({ database: String })
      .body(ByteStream())
      .returns(ByteStream())
      .secure({ principalKind: 'apikey' })
      .client(security)
      .handle(async (context) => {
        context.send(new TextEncoder().encode(`db=${context.queryParams().database}`));
        for await (const chunk of context.messages()) context.send(chunk);
      }),
    'GET',
  );
  providerApi.register(
    '/v1/databases/locked',
    endpoint()
      .body(ByteStream())
      .returns(ByteStream())
      .secure({ principalKind: 'apikey', roles: ['gateway:admin'] })
      .client({ security: { alternatives: [{ allOf: [{ profile: 'gateway-key', roles: ['gateway:admin'] }] }] } })
      .handle(async () => undefined),
    'GET',
  );
  providerApi.register(
    '/events/ws',
    endpoint()
      .body(Stream(EventIn))
      .returns(Stream(EventOut))
      .subprotocol('putnami.events.v1')
      .secure({ principalKind: 'apikey' })
      .client(security)
      .handle(async (context) => {
        for await (const frame of context.messages()) {
          if (frame.type === 'done') return;
          context.send({ type: 'event', data: `${frame.topic}-1` });
          context.send({ type: 'event', data: `${frame.topic}-2` });
        }
      }),
    'GET',
  );
  const providerOpenApi = openapi({ title: 'Gateway', version: '1.0.0' });
  const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
  await provider.start();
  running.push(async () => {
    await provider.stop();
  });
  const document = providerOpenApi.spec();
  if (!document) throw new Error('provider OpenAPI contract was not emitted');
  return { baseUrl: `http://localhost:${providerHttp.getServer()?.port}`, document: JSON.stringify(document) };
}

async function readAtLeast(reader: ReadableStreamDefaultReader<Uint8Array>, length: number): Promise<Uint8Array> {
  const chunks: Uint8Array[] = [];
  let total = 0;
  while (total < length) {
    const { value, done } = await reader.read();
    if (done) break;
    chunks.push(value);
    total += value.byteLength;
  }
  const joined = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    joined.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return joined;
}

describe('provider-owned websocket wires, provider to generated TypeScript consumer', () => {
  specTest(
    'derives, emits, binds and carries both provider-owned wires through the real runtime',
    {
      feature: 'typescript/service-clients',
      requirement: 'provider-owned-websocket-wires',
      check: 'the-emitted-typescript-client-carries-both-provider-owned-wires-through-the-real-runtime',
    },
    async () => {
      const provider = await startGateway();
      const ir = readOpenApiSource(provider.document, { mode: 'firstParty' });
      const methods = ir.services.flatMap((service) => service.methods);
      const connectMethod = methods.find((method) => method.path === '/v1/databases/connect');
      const lockedMethod = methods.find((method) => method.path === '/v1/databases/locked');
      const eventsMethod = methods.find((method) => method.path === '/events/ws');
      if (!connectMethod || !lockedMethod || !eventsMethod) throw new Error('provider wires did not reach the IR');
      const files = generateTypeScriptClient(ir, { packageName: '@test/gateway-client' });
      expect(files.every((file) => !file.content.includes(API_KEY))).toBe(true);
      const directory = mkdtempSync(join(tmpdir(), 'putnami-provider-wire-client-'));
      temporaryDirectories.push(directory);
      for (const file of files) {
        const destination = join(directory, file.path);
        mkdirSync(dirname(destination), { recursive: true });
        writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
      }
      const generated = (await import(join(directory, 'src', 'index.ts'))) as Record<string, unknown>;
      // The provider's paths group into more than one generated service class;
      // each one is registered and resolved the way a consumer would.
      const classes = Object.entries(generated).filter(
        ([name, value]) =>
          typeof value === 'function' &&
          name.endsWith('Client') &&
          !name.startsWith('register') &&
          !name.startsWith('bind'),
      );
      const registrations = Object.entries(generated).filter(([name]) => name.startsWith('register'));
      if (classes.length === 0 || registrations.length !== classes.length) {
        throw new Error('the generated package exports no client');
      }
      process.env.CONFIG_DATA = JSON.stringify({
        clients: {
          clientId: 'gateway-consumer',
          services: {
            gateway: {
              url: provider.baseUrl,
              allowInsecure: true,
              credentials: { 'gateway-key': { source: 'static', value: API_KEY } },
            },
          },
        },
      });
      resetConfigLoader();
      const consumer = application().use(http({ port: 0 }));
      for (const [, register] of registrations) (register as (target: typeof consumer) => typeof consumer)(consumer);
      await consumer.start();
      running.push(async () => {
        await consumer.stop();
      });
      const instances = classes.map(
        ([, value]) =>
          consumer.context.get(value as new (...args: never[]) => object) as Record<
            string,
            (...args: unknown[]) => unknown
          >,
      );
      // One bound client that owns every operation name, for the calls below.
      const gateway: Record<string, (...args: unknown[]) => unknown> = {};
      for (const instance of instances) {
        for (const method of [connectMethod, lockedMethod, eventsMethod]) {
          const call = instance[method.name];
          if (typeof call === 'function') gateway[method.name] = (...args: unknown[]) => call.apply(instance, args);
        }
      }

      // The byte stream: a readable and writable pair over binary messages.
      const tunnel = (await gateway[connectMethod.name]?.({
        query: { database: 'main' },
      })) as ClientByteStream;
      const reader = tunnel.readable.getReader();
      expect(new TextDecoder().decode(await readAtLeast(reader, 7))).toBe('db=main');
      const payload = new Uint8Array(150).map((_, index) => (index * 37) % 256);
      const writer = tunnel.writable.getWriter();
      await writer.write(payload);
      writer.releaseLock();
      expect([...(await readAtLeast(reader, payload.byteLength))]).toEqual([...payload]);
      reader.releaseLock();
      await tunnel.close();
      await tunnel.closed;

      // The provider's own security chain refuses the upgrade before a socket exists.
      await expect(gateway[lockedMethod.name]?.() as Promise<unknown>).rejects.toThrow(/refused the websocket upgrade/);

      // The typed frame stream: the provider's own JSON values, no envelope.
      const events = gateway[eventsMethod.name]?.() as FrameStream<
        { type: string; topic: string },
        { type: string; data: string }
      >;
      const received: { type: string; data: string }[] = [];
      const ended = new Promise<void>((resolvePromise, reject) => {
        events.onMessage((frame) => received.push(frame));
        events.onError(reject);
        events.onComplete(() => resolvePromise());
      });
      events.send({ type: 'subscribe', topic: 'orders' });
      events.send({ type: 'done', topic: '' });
      await ended;
      expect(received).toEqual([
        { type: 'event', data: 'orders-1' },
        { type: 'event', data: 'orders-2' },
      ]);
    },
    30_000,
  );
});
