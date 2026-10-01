import { afterEach, describe, expect } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { api, application, endpoint, http, openapi } from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
const temporaryDirectories: string[] = [];
const originalConfig = process.env.CONFIG_DATA;

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
  if (originalConfig === undefined) delete process.env.CONFIG_DATA;
  else process.env.CONFIG_DATA = originalConfig;
  resetConfigLoader();
});

describe('first-party response cache, provider to generated consumer', () => {
  specTest(
    'a declared cache reaches the emitted client, which answers repeated reads from memory and drops them on invalidation',
    {
      feature: 'typescript/service-clients',
      requirement: 'response-cache',
      check: 'the-emitted-ts-client-pins-the-response-cache-capability-and-its-manifest-declares-the-policy',
    },
    async () => {
      let providerCalls = 0;
      const providerHttp = http({ port: 0 });
      const providerApi = api({
        autoScan: false,
        client: { service: { id: 'identity', audience: 'urn:identity' }, credentials: {} },
      });
      providerApi.register(
        '/accounts/[id]',
        endpoint()
          .params({ id: String })
          .returns({ id: String, calls: Number })
          .client({
            security: { alternatives: [{ allOf: [] }] },
            idempotency: { kind: 'safe' },
            resilience: { cache: { freshMs: 5000, staleMs: 300_000, keyFields: ['path.id'] } },
          })
          .handle((context) => {
            providerCalls++;
            return { id: context.params.id, calls: providerCalls };
          }),
        'GET',
      );
      const providerOpenApi = openapi({ title: 'Identity', version: '1.0.0' });
      const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
      const consumer = application();

      await provider.start();
      let providerStopped = false;
      try {
        const port = providerHttp.getServer()?.port;
        const contract = providerOpenApi.spec();
        if (!port || !contract) throw new Error('provider did not start');
        // Provider declaration → published contract → strict reader → emitter.
        const ir = readOpenApiSource(JSON.stringify(contract), { mode: 'firstParty' });
        const method = ir.services
          .flatMap((service) => service.methods)
          .find((entry) => entry.path === '/accounts/{id}');
        expect(method?.client?.resilience?.cache).toEqual({ freshMs: 5000, staleMs: 300_000, keyFields: ['path.id'] });
        const files = generateTypeScriptClient(ir, { packageName: '@test/identity-client' });
        const module = files.find((file) => file.content.includes('SERVICE_DESCRIPTOR'));
        expect(module?.content).toContain("requireClientRuntimeCapabilities(['response-cache']);");

        const directory = mkdtempSync(join(tmpdir(), 'putnami-cached-client-'));
        temporaryDirectories.push(directory);
        for (const file of files) {
          const destination = join(directory, file.path);
          mkdirSync(dirname(destination), { recursive: true });
          writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
        }
        // Loading the emitted module runs its capability pin against this runtime.
        const generated = await import(join(directory, 'src', 'index.ts'));
        const clientName = Object.keys(generated).find(
          (name) => name.endsWith('Client') && !name.startsWith('register') && !name.startsWith('bind'),
        );
        const registerName = Object.keys(generated).find((name) => name.startsWith('register'));
        if (!clientName || !registerName || !method) throw new Error('the emitted module has no client binding');
        const Client = generated[clientName] as new (...args: never[]) => Record<string, unknown>;
        (generated[registerName] as (target: typeof consumer) => unknown)(consumer);
        process.env.CONFIG_DATA = JSON.stringify({
          clients: {
            clientId: 'consumer-tests',
            services: { identity: { url: `http://localhost:${port}`, allowInsecure: true } },
          },
        });
        resetConfigLoader();
        await consumer.start();

        const client = consumer.context.get(Client);
        const getAccount = (id: string) =>
          (client[method.name] as (input: { path: { id: string } }) => Promise<{ id: string; calls: number }>).call(
            client,
            { path: { id } },
          );
        // Twenty concurrent consumer calls, one provider call.
        const answers = await Promise.all(Array.from({ length: 20 }, () => getAccount('a1')));
        expect(new Set(answers.map((answer) => JSON.stringify(answer)))).toEqual(
          new Set([JSON.stringify({ id: 'a1', calls: 1 })]),
        );
        expect(providerCalls).toBe(1);

        // Inside the fresh window the answer survives the provider going away.
        await provider.stop();
        providerStopped = true;
        expect(await getAccount('a1')).toEqual({ id: 'a1', calls: 1 });

        // An invalidation sends the next call upstream, which is now down.
        const invalidate = client['invalidateResponses'] as (prefix: string) => number;
        expect(invalidate.call(client, `${method.operationId}?path.id=%22a1%22`)).toBe(1);
        await expect(getAccount('a1')).rejects.toThrow();
      } finally {
        await consumer.stop();
        if (!providerStopped) await provider.stop();
      }
    },
  );

  specTest(
    'the emitted client bypasses the cache per call and drops every answer carrying a declared response field',
    {
      feature: 'typescript/service-clients',
      requirement: 'response-cache-bypass-and-field-invalidation',
      check: 'an-invalidation-by-a-response-field-drops-every-answer-carrying-the-value',
    },
    async () => {
      let providerCalls = 0;
      const owners: Record<string, string> = { a1: 'p1', a2: 'p1', a3: 'p2' };
      const providerHttp = http({ port: 0 });
      const providerApi = api({
        autoScan: false,
        client: { service: { id: 'identity', audience: 'urn:identity' }, credentials: {} },
      });
      const cache = { freshMs: 60_000, keyFields: ['path.id'], invalidationFields: ['principalId'] };
      for (const route of ['/accounts/[id]', '/profiles/[id]']) {
        providerApi.register(
          route,
          endpoint()
            .params({ id: String })
            .returns({ id: String, principalId: String, calls: Number })
            .client({
              security: { alternatives: [{ allOf: [] }] },
              idempotency: { kind: 'safe' },
              resilience: { cache },
            })
            .handle((context) => {
              providerCalls++;
              return { id: context.params.id, principalId: owners[context.params.id] ?? 'p1', calls: providerCalls };
            }),
          'GET',
        );
      }
      const providerOpenApi = openapi({ title: 'Identity', version: '1.0.0' });
      const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
      const consumer = application();

      await provider.start();
      try {
        const port = providerHttp.getServer()?.port;
        const contract = providerOpenApi.spec();
        if (!port || !contract) throw new Error('provider did not start');
        // Provider declaration → published contract → strict reader → emitter.
        const ir = readOpenApiSource(JSON.stringify(contract), { mode: 'firstParty' });
        const methods = ir.services.flatMap((service) => service.methods);
        const accounts = methods.find((entry) => entry.path === '/accounts/{id}');
        const profiles = methods.find((entry) => entry.path === '/profiles/{id}');
        if (!accounts || !profiles) throw new Error('the published contract lost an operation');
        expect(accounts.client?.resilience?.cache).toEqual(cache);
        const files = generateTypeScriptClient(ir, { packageName: '@test/identity-client' });
        expect(files.find((file) => file.path === 'src/types.ts')?.content).toContain(
          'withoutResponseCache?: boolean;',
        );

        const directory = mkdtempSync(join(tmpdir(), 'putnami-cached-client-'));
        temporaryDirectories.push(directory);
        for (const file of files) {
          const destination = join(directory, file.path);
          mkdirSync(dirname(destination), { recursive: true });
          writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
        }
        const generated = (await import(join(directory, 'src', 'index.ts'))) as Record<string, unknown>;
        // The emitter groups the two paths into two client classes of one
        // service: they share that service's cache in the consumer.
        type GeneratedClass = new (...args: never[]) => Record<string, unknown>;
        const classOf = (methodName: string) =>
          Object.values(generated).find(
            (value): value is GeneratedClass =>
              typeof value === 'function' &&
              typeof (value.prototype as Record<string, unknown>)?.[methodName] === 'function',
          );
        const Accounts = classOf(accounts.name);
        const Profiles = classOf(profiles.name);
        if (!Accounts || !Profiles) throw new Error('the emitted module has no client for an operation');
        for (const [name, register] of Object.entries(generated))
          if (name.startsWith('register')) (register as (target: typeof consumer) => unknown)(consumer);
        process.env.CONFIG_DATA = JSON.stringify({
          clients: {
            clientId: 'consumer-tests',
            services: { identity: { url: `http://localhost:${port}`, allowInsecure: true } },
          },
        });
        resetConfigLoader();
        await consumer.start();

        const accountsClient = consumer.context.get(Accounts);
        const profilesClient = consumer.context.get(Profiles);
        type Answer = { id: string; principalId: string; calls: number };
        const call = (name: string, id: string, options?: { withoutResponseCache?: boolean }) => {
          const client = name === accounts.name ? accountsClient : profilesClient;
          return (client[name] as (input: { path: { id: string } }, options?: object) => Promise<Answer>).call(
            client,
            { path: { id } },
            options,
          );
        };

        // A bypassed call reads nothing: a fresh answer is stored, and the
        // bypassed call still reaches the provider.
        expect(await call(accounts.name, 'a1')).toEqual({ id: 'a1', principalId: 'p1', calls: 1 });
        expect(await call(accounts.name, 'a1', { withoutResponseCache: true })).toEqual({
          id: 'a1',
          principalId: 'p1',
          calls: 2,
        });
        // ...and stores nothing: the next plain call reads the first answer.
        expect(await call(accounts.name, 'a1')).toEqual({ id: 'a1', principalId: 'p1', calls: 1 });
        expect(providerCalls).toBe(2);

        // Answers of two operations name p1; one invalidation by that value
        // drops them all and leaves p2's answer.
        await call(accounts.name, 'a2');
        await call(accounts.name, 'a3');
        await call(profiles.name, 'a1');
        expect(providerCalls).toBe(5);
        const invalidate = accountsClient['invalidateResponsesByField'] as (field: string, value: string) => number;
        expect(invalidate.call(accountsClient, 'principalId', 'p1')).toBe(3);
        expect((await call(accounts.name, 'a1')).calls).toBe(6);
        expect((await call(profiles.name, 'a1')).calls).toBe(7);
        expect((await call(accounts.name, 'a3')).calls).toBe(4);
        expect(providerCalls).toBe(7);
      } finally {
        await consumer.stop();
        await provider.stop();
      }
    },
  );
});
